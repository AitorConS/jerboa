//go:build ignore

// Guest fixture, compiled explicitly by the PostgreSQL acceptance scripts.
package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

func writeMessage(w io.Writer, kind byte, payload []byte) error {
	buf := make([]byte, 5+len(payload))
	buf[0] = kind
	binary.BigEndian.PutUint32(buf[1:5], uint32(4+len(payload)))
	copy(buf[5:], payload)
	_, err := w.Write(buf)
	return err
}

func readMessage(r *bufio.Reader) (byte, []byte, error) {
	kind, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	var size uint32
	if err := binary.Read(r, binary.BigEndian, &size); err != nil {
		return 0, nil, err
	}
	if size < 4 || size > 16<<20 {
		return 0, nil, fmt.Errorf("invalid PostgreSQL message size %d", size)
	}
	payload := make([]byte, size-4)
	_, err = io.ReadFull(r, payload)
	return kind, payload, err
}

func serverError(payload []byte) error {
	fields := strings.Split(string(payload), "\x00")
	var message string
	for _, field := range fields {
		if len(field) > 1 && field[0] == 'M' {
			message = field[1:]
		}
	}
	if message == "" {
		message = fmt.Sprintf("server error payload %q", payload)
	}
	return fmt.Errorf("PostgreSQL: %s", message)
}

func hmacSHA256(key []byte, message string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return mac.Sum(nil)
}

func scramAttributes(message string) map[byte]string {
	attributes := map[byte]string{}
	for _, part := range strings.Split(message, ",") {
		if len(part) >= 2 && part[1] == '=' {
			attributes[part[0]] = part[2:]
		}
	}
	return attributes
}

// scram performs SCRAM-SHA-256 over an authenticated TLS connection.
func scram(conn net.Conn, r *bufio.Reader, mechanisms []byte, password string) error {
	if !strings.Contains(string(mechanisms), "SCRAM-SHA-256\x00") {
		return fmt.Errorf("server does not offer SCRAM-SHA-256: %q", mechanisms)
	}
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	nonce := base64.StdEncoding.EncodeToString(raw)
	clientFirstBare := "n=,r=" + nonce
	clientFirst := "n,," + clientFirstBare
	initial := append([]byte("SCRAM-SHA-256\x00"), 0, 0, 0, 0)
	binary.BigEndian.PutUint32(initial[len(initial)-4:], uint32(len(clientFirst)))
	if err := writeMessage(conn, 'p', append(initial, clientFirst...)); err != nil {
		return err
	}
	serverFirst, err := expectAuth(r, 11)
	if err != nil {
		return err
	}
	attributes := scramAttributes(serverFirst)
	iterations, err := strconv.Atoi(attributes['i'])
	if err != nil || iterations < 4096 {
		return fmt.Errorf("unacceptable SCRAM iteration count %q", attributes['i'])
	}
	salt, err := base64.StdEncoding.DecodeString(attributes['s'])
	if err != nil || !strings.HasPrefix(attributes['r'], nonce) {
		return fmt.Errorf("invalid SCRAM server-first message %q", serverFirst)
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return err
	}
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	withoutProof := "c=biws,r=" + attributes['r']
	authMessage := clientFirstBare + "," + serverFirst + "," + withoutProof
	proof := hmacSHA256(storedKey[:], authMessage)
	for i := range proof {
		proof[i] ^= clientKey[i]
	}
	final := withoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof)
	if err := writeMessage(conn, 'p', []byte(final)); err != nil {
		return err
	}
	serverFinal, err := expectAuth(r, 12)
	if err != nil {
		return err
	}
	expected := hmacSHA256(hmacSHA256(salted, "Server Key"), authMessage)
	signature, err := base64.StdEncoding.DecodeString(scramAttributes(serverFinal)['v'])
	if err != nil || !hmac.Equal(signature, expected) {
		return errors.New("SCRAM server signature mismatch")
	}
	return nil
}

func expectAuth(r *bufio.Reader, code uint32) (string, error) {
	kind, body, err := readMessage(r)
	if err != nil {
		return "", err
	}
	if kind == 'E' {
		return "", serverError(body)
	}
	if kind != 'R' || len(body) < 4 || binary.BigEndian.Uint32(body[:4]) != code {
		return "", fmt.Errorf("expected authentication message %d, got %c %v", code, kind, body)
	}
	return string(body[4:]), nil
}

func connect(address, password string) (net.Conn, *bufio.Reader, error) {
	return connectTLS(address, password, "postgres.jerboa.test", false)
}

type backendConn struct {
	net.Conn
	pid, cancelKey uint32
}

func connectTLS(address, password, serverName string, untrusted bool) (net.Conn, *bufio.Reader, error) {
	conn, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		return nil, nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		conn.Close()
		return nil, nil, err
	}
	request := []byte{0, 0, 0, 8, 4, 210, 22, 47}
	if _, err := conn.Write(request); err != nil {
		conn.Close()
		return nil, nil, err
	}
	var reply [1]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil || reply[0] != 'S' {
		conn.Close()
		return nil, nil, fmt.Errorf("server refused TLS: %q (%v)", reply, err)
	}
	certificate, err := base64.StdEncoding.DecodeString(os.Getenv("PGCA_BASE64"))
	roots := x509.NewCertPool()
	if err != nil || !roots.AppendCertsFromPEM(certificate) {
		conn.Close()
		return nil, nil, errors.New("PGCA_BASE64 must contain the trusted PEM certificate")
	}
	if untrusted {
		roots = x509.NewCertPool()
	}
	secure := tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: serverName, MinVersion: tls.VersionTLS12})
	if err := secure.Handshake(); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("TLS verification: %w", err)
	}
	conn = secure
	_ = conn.SetDeadline(time.Time{})
	user := os.Getenv("PGUSER")
	if user == "" {
		user = "postgres"
	}
	database := os.Getenv("PGDATABASE")
	if database == "" {
		database = "postgres"
	}
	payload := append([]byte{0, 3, 0, 0}, []byte("user\x00"+user+"\x00database\x00"+database+"\x00client_encoding\x00UTF8\x00\x00")...)
	packet := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(packet[:4], uint32(len(packet)))
	copy(packet[4:], payload)
	if _, err := conn.Write(packet); err != nil {
		conn.Close()
		return nil, nil, err
	}
	r := bufio.NewReader(conn)
	authenticated := false
	backend := &backendConn{Conn: conn}
	for {
		kind, body, err := readMessage(r)
		if err != nil {
			conn.Close()
			return nil, nil, err
		}
		switch kind {
		case 'R':
			if len(body) < 4 {
				conn.Close()
				return nil, nil, fmt.Errorf("short authentication request %v", body)
			}
			switch code := binary.BigEndian.Uint32(body[:4]); code {
			case 0:
				// A server that accepts us without SCRAM would be trust auth: refuse it.
				if !authenticated {
					conn.Close()
					return nil, nil, errors.New("server accepted connection without SCRAM authentication")
				}
			case 10:
				if err := scram(conn, r, body[4:], password); err != nil {
					conn.Close()
					return nil, nil, err
				}
				authenticated = true
			default:
				conn.Close()
				return nil, nil, fmt.Errorf("refusing authentication method %d (only SCRAM-SHA-256 is accepted)", code)
			}
		case 'E':
			conn.Close()
			return nil, nil, serverError(body)
		case 'K':
			if len(body) != 8 {
				conn.Close()
				return nil, nil, errors.New("invalid BackendKeyData")
			}
			backend.pid = binary.BigEndian.Uint32(body[:4])
			backend.cancelKey = binary.BigEndian.Uint32(body[4:])
		case 'Z':
			return backend, r, nil
		}
	}
}

// query runs a simple-protocol query returning the first column of each row.
// It always reads through ReadyForQuery so the session stays usable after errors.
func query(conn net.Conn, r *bufio.Reader, sql string) ([]string, error) {
	if err := writeMessage(conn, 'Q', append([]byte(sql), 0)); err != nil {
		return nil, err
	}
	var rows []string
	var queryErr error
	for {
		kind, body, err := readMessage(r)
		if err != nil {
			return nil, err
		}
		switch kind {
		case 'D':
			if len(body) < 6 || binary.BigEndian.Uint16(body[:2]) < 1 {
				return nil, fmt.Errorf("unexpected DataRow %v", body)
			}
			size := int(int32(binary.BigEndian.Uint32(body[2:6])))
			if size < 0 {
				rows = append(rows, "NULL")
				continue
			}
			if 6+size > len(body) {
				return nil, fmt.Errorf("invalid DataRow length %d", size)
			}
			rows = append(rows, string(body[6:6+size]))
		case 'E':
			if queryErr == nil {
				queryErr = serverError(body)
			}
		case 'Z':
			return rows, queryErr
		}
	}
}

type session struct {
	conn net.Conn
	r    *bufio.Reader
}

func (s session) one(sql string) string {
	rows, err := query(s.conn, s.r, sql)
	if err != nil {
		fail("%s: %v", sql, err)
	}
	if len(rows) != 1 {
		fail("%s: expected one row, got %q", sql, rows)
	}
	return rows[0]
}

func (s session) exec(sql string) {
	if _, err := query(s.conn, s.r, sql); err != nil {
		fail("%s: %v", sql, err)
	}
}

func (s session) expect(sql, want string) {
	if got := s.one(sql); got != want {
		fail("%s: got %q, want %q", sql, got, want)
	}
	fmt.Printf("CHECK %s = %s\n", sql, want)
}

func (s session) expectError(sql, fragment string) {
	_, err := query(s.conn, s.r, sql)
	if err == nil || !strings.Contains(err.Error(), fragment) {
		fail("%s: expected error containing %q, got %v", sql, fragment, err)
	}
	fmt.Printf("CHECK %s rejected: %v\n", sql, err)
}

func fail(format string, args ...any) {
	fmt.Printf("FAIL "+format+"\n", args...)
	os.Exit(1)
}

func open(host string) session {
	var lastErr error
	for attempt := 0; attempt < 240; attempt++ {
		conn, r, err := connect(net.JoinHostPort(host, "5432"), os.Getenv("PGPASSWORD"))
		if err == nil {
			return session{conn, r}
		}
		lastErr = err
		if strings.Contains(err.Error(), "authentication") || strings.Contains(err.Error(), "SCRAM") {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	fail("connect %s: %v", host, lastErr)
	return session{}
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		return fallback
	}
	return value
}

// suite exercises SQL semantics and the durability-relevant server settings.
func suite(s session) {
	s.expect("SHOW ssl", "on")
	s.expect("SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()", "t")
	version := s.one("SELECT version()")
	if !strings.Contains(version, "PostgreSQL 11devel") {
		fail("unexpected version %q", version)
	}
	fmt.Println("VERSION=" + version)
	s.expect("SHOW fsync", "on")
	s.expect("SHOW synchronous_commit", "on")
	s.expect("SHOW full_page_writes", "on")
	s.expect("SHOW data_checksums", "on")
	s.expect("SHOW password_encryption", "scram-sha-256")
	s.expect("SHOW wal_sync_method", "fdatasync")
	s.expect("SHOW backend_flush_after", "0")
	s.expect("SHOW checkpoint_flush_after", "0")
	s.expect("SHOW bgwriter_flush_after", "0")
	// Nanos has no writeback hint: a nonzero value must be refused, not silently ignored.
	s.expectError("SET backend_flush_after = 16", "outside the valid range")

	s.exec("CREATE TABLE IF NOT EXISTS jerboa_probe (id integer PRIMARY KEY, value text NOT NULL)")
	s.exec("INSERT INTO jerboa_probe (id, value) VALUES (1, 'nanos-arm64') ON CONFLICT (id) DO UPDATE SET value = EXCLUDED.value")
	s.exec("DROP TABLE IF EXISTS jerboa_sql")
	s.exec("CREATE TABLE jerboa_sql (id integer PRIMARY KEY, grp integer NOT NULL, payload text NOT NULL)")
	s.exec("CREATE INDEX jerboa_sql_grp ON jerboa_sql (grp)")
	s.exec("INSERT INTO jerboa_sql SELECT g, g % 10, md5(g::text) FROM generate_series(1, 20000) g")
	s.exec("UPDATE jerboa_sql SET payload = md5(payload) WHERE grp = 3")
	s.exec("DELETE FROM jerboa_sql WHERE grp = 7")
	s.expect("SELECT count(*) FROM jerboa_sql", "18000")
	s.expectError("INSERT INTO jerboa_sql VALUES (1, 1, 'duplicate')", "duplicate key")
	s.exec("BEGIN")
	s.exec("INSERT INTO jerboa_sql VALUES (900001, 1, 'rolled-back')")
	s.exec("ROLLBACK")
	s.expect("SELECT count(*) FROM jerboa_sql WHERE id = 900001", "0")
	s.exec("DO $$ BEGIN PERFORM 1; END $$")
	s.exec("SET enable_seqscan = off")
	s.expect("SELECT count(*) FROM jerboa_sql WHERE grp = 3", "2000")
	s.exec("RESET enable_seqscan")
	s.expect("SELECT count(*) FROM jerboa_sql WHERE grp = 3 AND payload <> md5(md5(id::text))", "0")
	s.exec("VACUUM ANALYZE jerboa_sql")
	s.exec("CHECKPOINT")
	s.expect("SELECT value FROM jerboa_probe WHERE id = 1", "nanos-arm64")
	fmt.Println("VALUE=nanos-arm64")
}

// ack commits one row per autocommit INSERT and prints ACK only after the
// server acknowledged the COMMIT. It checkpoints periodically so an abrupt
// stop can land during a checkpoint.
func ack(s session) {
	s.exec("CREATE TABLE IF NOT EXISTS durability_probe (id integer PRIMARY KEY, payload text NOT NULL)")
	start, err := strconv.Atoi(s.one("SELECT coalesce(max(id), 0) + 1 FROM durability_probe"))
	if err != nil {
		fail("start id: %v", err)
	}
	fmt.Printf("ACK_START %d\n", start)
	limit := start + envInt("PGACK_MAX", 1000000)
	for id := start; id < limit; id++ {
		if _, err := query(s.conn, s.r, fmt.Sprintf("INSERT INTO durability_probe VALUES (%d, repeat(md5('%d'), 8))", id, id)); err != nil {
			fmt.Printf("ACK_STOPPED after %d: %v\n", id-1, err)
			os.Exit(0)
		}
		fmt.Printf("ACK %d\n", id)
		if id%250 == 0 {
			if _, err := query(s.conn, s.r, "CHECKPOINT"); err != nil {
				fmt.Printf("ACK_STOPPED during checkpoint after %d: %v\n", id, err)
				os.Exit(0)
			}
			fmt.Printf("CHECKPOINTED %d\n", id)
		}
	}
	fmt.Println("ACK_DONE")
}

// recover checks that every acknowledged row up to PGACK survived with intact
// payload, that all pages still pass checksum verification on a full read, and
// that the index agrees with the heap.
func recoverCheck(s session, strict bool) {
	acked := envInt("PGACK", -1)
	if acked < 0 {
		fail("PGACK is required")
	}
	s.expect("SHOW data_checksums", "on")
	present := s.one(fmt.Sprintf("SELECT count(*) FROM durability_probe WHERE id <= %d", acked))
	maxID := s.one("SELECT coalesce(max(id), 0) FROM durability_probe")
	corrupt := s.one("SELECT count(*) FROM durability_probe WHERE payload <> repeat(md5(id::text), 8)")
	fmt.Printf("RECOVERED acked=%d present=%s max=%s bad_payload=%s\n", acked, present, maxID, corrupt)
	if !strict {
		fmt.Println("REPORT_OK")
		return
	}
	if present != strconv.Itoa(acked) {
		fail("lost acknowledged commits: %s of %d present", present, acked)
	}
	if corrupt != "0" {
		fail("%s rows have corrupt payload", corrupt)
	}
	// Full heap read verifies every page checksum; index-only path must agree.
	heap := s.one("SELECT count(*) FROM durability_probe")
	s.exec("SET enable_seqscan = off")
	s.exec("SET enable_bitmapscan = off")
	index := s.one("SELECT count(*) FROM durability_probe WHERE id > 0")
	s.exec("RESET enable_seqscan")
	s.exec("RESET enable_bitmapscan")
	if heap != index {
		fail("heap count %s differs from index count %s", heap, index)
	}
	s.expect("SELECT value FROM jerboa_probe WHERE id = 1", "nanos-arm64")
	s.expect("SELECT count(*) FROM jerboa_sql", "18000")
	if os.Getenv("PGAMCHECK") == "1" {
		s.exec("CREATE EXTENSION IF NOT EXISTS amcheck")
		s.exec("SELECT bt_index_check('durability_probe_pkey'::regclass)")
		s.exec("SELECT bt_index_check('jerboa_sql_pkey'::regclass)")
		fmt.Println("CHECK amcheck bt_index_check passed")
	}
	fmt.Println("DURABLE_OK")
}

// waitFor polls sql until it returns want, so asynchronous statistics and
// autovacuum can catch up. Each poll is its own transaction, which both
// flushes this backend's pending counters and takes a fresh stats snapshot.
func (s session) waitFor(sql, want string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	var got string
	for {
		got = s.one(sql)
		if got == want {
			fmt.Printf("CHECK %s = %s\n", sql, want)
			return
		}
		if time.Now().After(deadline) {
			fail("%s: still %q after %s, want %q", sql, got, timeout, want)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

const statsColumns = "n_tup_ins, n_tup_upd, n_tup_del, n_live_tup, n_dead_tup, seq_scan, " +
	"coalesce(idx_scan, 0), vacuum_count, autovacuum_count, analyze_count, autoanalyze_count"

func (s session) tableStats(table string) string {
	return s.one(fmt.Sprintf("SELECT concat_ws(',', %s) FROM pg_stat_user_tables WHERE relname = '%s'", statsColumns, table))
}

// statsRuntime checks that the collector and the autovacuum launcher really run.
func statsRuntime(s session) {
	s.expect("SHOW track_counts", "on")
	s.expect("SHOW autovacuum", "on")
	s.waitFor("SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'autovacuum launcher'", "1", 30*time.Second)
	// Backends are threads with 64-bit ids. Every thread must stay visible
	// with a unique positive 32-bit pid that matches pg_backend_pid().
	s.expect("SELECT concat_ws(',', count(*) FILTER (WHERE backend_type = 'checkpointer'), "+
		"count(*) FILTER (WHERE backend_type = 'background writer'), "+
		"count(*) FILTER (WHERE backend_type = 'walwriter')) FROM pg_stat_activity", "1,1,1")
	s.expect("SELECT count(*) FROM pg_stat_activity WHERE pid = pg_backend_pid() AND backend_type = 'client backend'", "1")
	s.expect("SELECT count(DISTINCT pid) = count(*) AND min(pid) > 0 FROM pg_stat_activity", "t")
	// A collector that cannot answer inquiries makes this return NULL or stale data.
	s.waitFor("SELECT pg_stat_get_snapshot_timestamp() > now() - interval '30 seconds'", "t", 30*time.Second)
}

// pidsReport records thread visibility without failing, to measure how often
// threads are missing from pg_stat_activity.
func pidsReport(s session) {
	time.Sleep(5 * time.Second)
	types := s.one("SELECT string_agg(backend_type || ':' || pid, ' ' ORDER BY backend_type) FROM pg_stat_activity")
	missing := s.one("SELECT concat_ws(',', " +
		"CASE WHEN count(*) FILTER (WHERE backend_type = 'autovacuum launcher') = 0 THEN 'autovacuum launcher' END, " +
		"CASE WHEN count(*) FILTER (WHERE backend_type = 'checkpointer') = 0 THEN 'checkpointer' END, " +
		"CASE WHEN count(*) FILTER (WHERE backend_type = 'background writer') = 0 THEN 'background writer' END, " +
		"CASE WHEN count(*) FILTER (WHERE backend_type = 'walwriter') = 0 THEN 'walwriter' END, " +
		"CASE WHEN count(*) FILTER (WHERE pid = pg_backend_pid()) = 0 THEN 'self' END) FROM pg_stat_activity")
	fmt.Printf("PIDS backend_pid=%s visible=[%s] missing=[%s]\n", s.one("SELECT pg_backend_pid()"), types, missing)
}

// statsSuite verifies exact counter updates and automatic VACUUM/ANALYZE driven
// only by controlled per-table thresholds. av_control has autovacuum disabled
// and must keep its dead tuples and zero automatic runs.
func statsSuite(s session) {
	statsRuntime(s)
	timeout := time.Duration(envInt("PGSTATS_TIMEOUT", 180)) * time.Second
	s.exec("ALTER SYSTEM SET autovacuum_naptime = '1s'")
	s.expect("SELECT pg_reload_conf()", "t")
	s.waitFor("SHOW autovacuum_naptime", "1s", 30*time.Second)

	before := s.one("SELECT xact_commit FROM pg_stat_database WHERE datname = current_database()")
	s.exec("DROP TABLE IF EXISTS av_probe")
	s.exec("DROP TABLE IF EXISTS av_control")
	s.exec("CREATE TABLE av_probe (id integer PRIMARY KEY, payload text NOT NULL) WITH (" +
		"autovacuum_vacuum_threshold = 100, autovacuum_vacuum_scale_factor = 0, " +
		"autovacuum_analyze_threshold = 100, autovacuum_analyze_scale_factor = 0, " +
		"log_autovacuum_min_duration = 0)")
	s.exec("CREATE TABLE av_control (id integer PRIMARY KEY, payload text NOT NULL) WITH (autovacuum_enabled = false)")
	s.exec("INSERT INTO av_control SELECT g, md5(g::text) FROM generate_series(1, 500) g")
	s.exec("DELETE FROM av_control WHERE id % 2 = 0")
	s.exec("INSERT INTO av_probe SELECT g, md5(g::text) FROM generate_series(1, 2000) g")
	s.expect("SELECT count(*) FROM av_probe", "2000")
	s.exec("SET enable_seqscan = off")
	s.expect("SELECT payload FROM av_probe WHERE id = 7", "8f14e45fceea167a5a36dedd4bea2543")
	s.exec("RESET enable_seqscan")
	s.exec("DELETE FROM av_probe WHERE id > 1000")
	s.exec("UPDATE av_probe SET payload = md5(payload) WHERE id <= 10")

	// Exact counters: every value follows from the statements above.
	s.waitFor("SELECT concat_ws(',', n_tup_ins, n_tup_del, n_tup_upd) FROM pg_stat_user_tables WHERE relname = 'av_probe'",
		"2000,1000,10", 30*time.Second)
	s.waitFor("SELECT seq_scan >= 1 AND idx_scan >= 1 FROM pg_stat_user_tables WHERE relname = 'av_probe'", "t", 30*time.Second)
	s.waitFor("SELECT concat_ws(',', n_tup_ins, n_tup_del, n_live_tup, n_dead_tup) FROM pg_stat_user_tables WHERE relname = 'av_control'",
		"500,250,250,250", 30*time.Second)
	s.waitFor(fmt.Sprintf("SELECT xact_commit > %s FROM pg_stat_database WHERE datname = current_database()", before), "t", 30*time.Second)
	fmt.Println("STATS_BEFORE_AUTOVACUUM av_probe=" + s.tableStats("av_probe"))

	// Automatic maintenance, without any manual VACUUM or ANALYZE on these tables.
	s.waitFor("SELECT autovacuum_count >= 1 AND autoanalyze_count >= 1 AND n_dead_tup = 0 AND last_autovacuum IS NOT NULL "+
		"AND last_autoanalyze IS NOT NULL FROM pg_stat_user_tables WHERE relname = 'av_probe'", "t", timeout)
	s.expect("SELECT vacuum_count + analyze_count FROM pg_stat_user_tables WHERE relname = 'av_probe'", "0")
	// ANALYZE and VACUUM update the catalog, not only the counters.
	s.waitFor("SELECT reltuples::bigint FROM pg_class WHERE relname = 'av_probe'", "1000", timeout)
	s.expect("SELECT count(*) > 0 FROM pg_stats WHERE tablename = 'av_probe' AND attname = 'id'", "t")
	s.expect("SELECT concat_ws(',', n_dead_tup, vacuum_count, autovacuum_count, analyze_count, autoanalyze_count) "+
		"FROM pg_stat_user_tables WHERE relname = 'av_control'", "250,0,0,0,0")
	s.expect("SELECT count(*) FROM av_probe", "1000")
	fmt.Println("STATS_AFTER_AUTOVACUUM av_probe=" + s.tableStats("av_probe"))

	// Backends are threads here: per-session pending counters must not be
	// lost or mixed when many sessions report concurrently.
	s.exec("DROP TABLE IF EXISTS av_concurrent")
	s.exec("CREATE TABLE av_concurrent (worker integer NOT NULL, id integer NOT NULL) WITH (autovacuum_enabled = false)")
	const workers, rows = 8, 250
	done := make(chan error, workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			c, r, err := connect(net.JoinHostPort(os.Getenv("PGHOST"), "5432"), os.Getenv("PGPASSWORD"))
			if err != nil {
				done <- err
				return
			}
			defer c.Close()
			for i := 0; i < rows; i++ {
				if _, err := query(c, r, fmt.Sprintf("INSERT INTO av_concurrent VALUES (%d, %d)", w, i)); err != nil {
					done <- err
					return
				}
			}
			if _, err := query(c, r, fmt.Sprintf("DELETE FROM av_concurrent WHERE worker = %d AND id < %d", w, w+1)); err != nil {
				done <- err
				return
			}
			done <- nil
		}(w)
	}
	for w := 0; w < workers; w++ {
		if err := <-done; err != nil {
			fail("concurrent stats session: %v", err)
		}
	}
	// Deletes are 1+2+...+8 = 36 rows.
	s.waitFor("SELECT concat_ws(',', n_tup_ins, n_tup_del, n_live_tup, n_dead_tup) FROM pg_stat_user_tables WHERE relname = 'av_concurrent'",
		fmt.Sprintf("%d,36,%d,36", workers*rows, workers*rows-36), 30*time.Second)
	fmt.Println("STATS_OK")
}

func signalsSuite(s session) {
	defer s.conn.Close()
	s.expect("SHOW max_parallel_workers", "0")
	s.expect("SHOW max_parallel_workers_per_gather", "0")
	_ = s.conn.SetDeadline(time.Now().Add(180 * time.Second))
	target := open(os.Getenv("PGHOST"))
	defer target.conn.Close()
	_ = target.conn.SetDeadline(time.Now().Add(45 * time.Second))
	pid := target.one("SELECT pg_backend_pid()")
	s.exec("CREATE ROLE signal_unprivileged")
	s.exec("SET ROLE signal_unprivileged")
	s.expectError("SELECT pg_cancel_backend("+pid+")", "must be a superuser")
	s.expectError("SELECT pg_terminate_backend("+pid+")", "must be a superuser")
	s.exec("RESET ROLE")
	s.exec("DROP ROLE signal_unprivileged")
	done := make(chan error, 1)
	go func() {
		_, err := query(target.conn, target.r, "SELECT pg_sleep(30)")
		done <- err
	}()
	s.waitFor("SELECT count(*) FROM pg_stat_activity WHERE pid = "+pid+" AND wait_event = 'PgSleep'", "1", 10*time.Second)
	s.expect("SELECT pg_cancel_backend("+pid+")", "t")
	if err := <-done; err == nil || !strings.Contains(err.Error(), "canceling statement due to user request") {
		fail("backend cancellation: %v", err)
	}
	target.expect("SELECT 42", "42")
	backend := target.conn.(*backendConn)
	if strconv.FormatUint(uint64(backend.pid), 10) != pid {
		fail("BackendKeyData PID differs from SQL PID")
	}
	go func() {
		_, err := query(target.conn, target.r, "SELECT pg_sleep(30)")
		done <- err
	}()
	s.waitFor("SELECT count(*) FROM pg_stat_activity WHERE pid = "+pid+" AND wait_event = 'PgSleep'", "1", 10*time.Second)
	sendCancel := func(key uint32) {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(os.Getenv("PGHOST"), "5432"), 5*time.Second)
		if err != nil {
			fail("cancel connection: %v", err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		packet := make([]byte, 16)
		binary.BigEndian.PutUint32(packet[0:4], 16)
		binary.BigEndian.PutUint32(packet[4:8], 80877102)
		binary.BigEndian.PutUint32(packet[8:12], backend.pid)
		binary.BigEndian.PutUint32(packet[12:16], key)
		if _, err := c.Write(packet); err != nil {
			fail("cancel request: %v", err)
		}
		if data, err := io.ReadAll(c); err != nil || len(data) != 0 {
			fail("cancel request completion: %q %v", data, err)
		}
	}
	sendCancel(backend.cancelKey ^ 1)
	select {
	case err := <-done:
		fail("wrong cancel key affected query: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	sendCancel(backend.cancelKey)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "canceling statement due to user request") {
		fail("protocol cancellation: %v", err)
	}
	target.expect("SELECT 42", "42")
	fmt.Println("PROTOCOL_CANCEL_OK")
	s.expect("SELECT pg_terminate_backend("+pid+")", "t")
	if _, err := query(target.conn, target.r, "SELECT 1"); err == nil {
		fail("terminated backend still accepts queries")
	}
	s.waitFor("SELECT count(*) FROM pg_stat_activity WHERE pid = "+pid, "0", 10*time.Second)
	s.expect("SELECT pg_cancel_backend("+pid+")", "f")
	s.expect("SELECT pg_terminate_backend(1)", "f")
	s.expect("SELECT 42", "42")
	fmt.Println("BACKEND_SIGNALS_OK")

	// An actual automatic worker must hold the conflicting table lock long
	// enough for ProcSleep's deadlock-timeout cancellation path to run.
	s.exec("SET deadlock_timeout = '100ms'")
	s.exec("SET statement_timeout = '20s'")
	s.exec("ALTER SYSTEM SET autovacuum_naptime = '1s'")
	s.expect("SELECT pg_reload_conf()", "t")
	s.exec("DROP TABLE IF EXISTS av_lock_probe")
	s.exec("CREATE TABLE av_lock_probe (id integer, payload text) WITH (autovacuum_enabled = false)")
	s.exec("INSERT INTO av_lock_probe SELECT g, repeat(md5(g::text), 32) FROM generate_series(1, 20000) g")
	s.exec("DELETE FROM av_lock_probe WHERE id % 2 = 0")
	s.waitFor("SELECT n_dead_tup >= 10000 FROM pg_stat_user_tables WHERE relname = 'av_lock_probe'", "t", 30*time.Second)
	s.exec("ALTER TABLE av_lock_probe SET (autovacuum_enabled = true, autovacuum_vacuum_threshold = 1, " +
		"autovacuum_vacuum_scale_factor = 0, autovacuum_vacuum_cost_delay = 100, autovacuum_vacuum_cost_limit = 1)")
	s.waitFor("SELECT count(*) > 0 FROM pg_stat_activity a JOIN pg_locks l ON l.pid = a.pid "+
		"WHERE a.backend_type = 'autovacuum worker' AND l.relation = 'av_lock_probe'::regclass "+
		"AND l.mode = 'ShareUpdateExclusiveLock' AND l.granted", "t", 90*time.Second)
	s.exec("BEGIN")
	s.exec("LOCK TABLE av_lock_probe IN ACCESS EXCLUSIVE MODE")
	s.exec("ALTER TABLE av_lock_probe SET (autovacuum_enabled = false)")
	s.exec("COMMIT")
	s.expect("SELECT count(*) FROM av_lock_probe", "10000")
	s.exec("SET max_parallel_workers = 2")
	s.exec("SET max_parallel_workers_per_gather = 2")
	s.exec("SET force_parallel_mode = on")
	s.exec("SET min_parallel_table_scan_size = 0")
	s.exec("SET parallel_setup_cost = 0")
	s.exec("SET parallel_tuple_cost = 0")
	plan, err := query(s.conn, s.r, "EXPLAIN (ANALYZE, COSTS OFF) SELECT sum(id) FROM av_lock_probe")
	if err != nil {
		fail("parallel aggregate: %v", err)
	}
	if text := strings.Join(plan, "\n"); !strings.Contains(text, "Workers Launched: 0") {
		fail("Nanos parallel fallback was bypassed: %s", text)
	}
	for i := 0; i < 8; i++ {
		s.expect("SELECT sum(id) FROM av_lock_probe", "100000000")
	}
	fmt.Println("PARALLEL_FALLBACK_OK")
	s.exec("DROP TABLE av_lock_probe")
	fmt.Println("AUTOVACUUM_LOCK_CANCEL_OK")
}

// statsRestart checks counter persistence and resumed automatic maintenance.
func statsRestart(s session) {
	statsRuntime(s)
	timeout := time.Duration(envInt("PGSTATS_TIMEOUT", 180)) * time.Second
	s.expect("SHOW autovacuum_naptime", "1s")
	s.expect("SELECT count(*) FROM av_probe", "1000")
	s.expect("SELECT count(*) FROM av_control", "250")
	s.expect("SELECT reltuples::bigint FROM pg_class WHERE relname = 'av_probe'", "1000")
	initial := s.tableStats("av_probe")
	fmt.Println("STATS_AT_RESTART av_probe=" + initial)
	autovacuums := s.one("SELECT coalesce((SELECT autovacuum_count FROM pg_stat_user_tables WHERE relname = 'av_probe'), 0)")
	if autovacuums == "0" {
		fmt.Println("STATS_RESET")
	} else {
		fmt.Println("STATS_PERSISTED autovacuum_count=" + autovacuums)
	}
	s.exec("DELETE FROM av_probe WHERE id > 500")
	s.waitFor("SELECT n_tup_del >= 500 FROM pg_stat_user_tables WHERE relname = 'av_probe'", "t", 30*time.Second)
	s.waitFor(fmt.Sprintf("SELECT autovacuum_count > %s AND n_dead_tup = 0 FROM pg_stat_user_tables WHERE relname = 'av_probe'", autovacuums),
		"t", timeout)
	s.waitFor("SELECT reltuples::bigint FROM pg_class WHERE relname = 'av_probe'", "500", timeout)
	s.expect("SELECT concat_ws(',', autovacuum_count, autoanalyze_count) FROM pg_stat_user_tables WHERE relname = 'av_control'", "0,0")
	fmt.Println("STATS_AFTER_RESTART av_probe=" + s.tableStats("av_probe"))
	fmt.Println("STATS_RESTART_OK")
}

func authReject(host string) {
	address := net.JoinHostPort(host, "5432")
	for _, password := range []string{"", "wrong-" + os.Getenv("PGPASSWORD")} {
		conn, _, err := connect(address, password)
		if err == nil {
			conn.Close()
			fail("connection with password %q unexpectedly succeeded", password)
		}
		if !strings.Contains(err.Error(), "password authentication failed") {
			fail("unexpected rejection for password %q: %v", password, err)
		}
		fmt.Printf("CHECK rejected password length %d: %v\n", len(password), err)
	}
	fmt.Println("AUTH_REJECT_OK")
}

func tlsReject(host string) {
	address := net.JoinHostPort(host, "5432")
	for _, test := range []struct {
		name      string
		untrusted bool
		fragment  string
	}{
		{"wrong.jerboa.test", false, "not wrong.jerboa.test"},
		{"postgres.jerboa.test", true, "unknown authority"},
	} {
		conn, _, err := connectTLS(address, os.Getenv("PGPASSWORD"), test.name, test.untrusted)
		if err == nil {
			conn.Close()
			fail("invalid TLS identity accepted")
		}
		if !strings.Contains(err.Error(), test.fragment) {
			fail("unexpected TLS rejection: %v", err)
		}
	}
	conn, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		fail("plaintext dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	payload := []byte("\x00\x03\x00\x00user\x00postgres\x00database\x00postgres\x00\x00")
	packet := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(packet[:4], uint32(len(packet)))
	copy(packet[4:], payload)
	if _, err := conn.Write(packet); err != nil {
		fail("plaintext startup: %v", err)
	}
	kind, body, err := readMessage(bufio.NewReader(conn))
	if err != nil || kind != 'E' || !strings.Contains(string(body), "no pg_hba.conf entry") {
		fail("plaintext was not rejected by hostssl: %c %q %v", kind, body, err)
	}
	legacy, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		fail("legacy TLS dial: %v", err)
	}
	defer legacy.Close()
	legacy.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := legacy.Write([]byte{0, 0, 0, 8, 4, 210, 22, 47}); err != nil {
		fail("legacy TLS request: %v", err)
	}
	var reply [1]byte
	if _, err := io.ReadFull(legacy, reply[:]); err != nil || reply[0] != 'S' {
		fail("legacy TLS negotiation: %q %v", reply, err)
	}
	certificate, _ := base64.StdEncoding.DecodeString(os.Getenv("PGCA_BASE64"))
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certificate)
	oldTLS := tls.Client(legacy, &tls.Config{
		RootCAs: roots, ServerName: "postgres.jerboa.test",
		MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11,
	})
	if err := oldTLS.Handshake(); err == nil || !strings.Contains(err.Error(), "protocol version") {
		fail("TLS below 1.2 not rejected with protocol-version alert: %v", err)
	}

	// Exercise context replacement while independent backend threads negotiate.
	errorsCh := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 4; j++ {
				c, r, err := connect(address, os.Getenv("PGPASSWORD"))
				if err != nil {
					errorsCh <- err
					return
				}
				_, err = query(c, r, "SELECT 1")
				c.Close()
				if err != nil {
					errorsCh <- err
					return
				}
			}
			errorsCh <- nil
		}()
	}
	reloader := open(host)
	for i := 0; i < 4; i++ {
		reloader.expect("SELECT pg_reload_conf()", "t")
		time.Sleep(50 * time.Millisecond)
	}
	reloader.conn.Close()
	for i := 0; i < 8; i++ {
		if err := <-errorsCh; err != nil {
			fail("concurrent TLS/reload: %v", err)
		}
	}
	fmt.Println("TLS_REJECT_OK")
}

func main() {
	host := os.Getenv("PGHOST")
	if host == "" {
		host = "postgres"
	}
	switch mode := os.Getenv("PGMODE"); mode {
	case "", "write":
		suite(open(host))
	case "verify":
		s := open(host)
		s.expect("SELECT value FROM jerboa_probe WHERE id = 1", "nanos-arm64")
		s.expect("SELECT count(*) FROM jerboa_sql", "18000")
		fmt.Println("VALUE=nanos-arm64")
	case "auth":
		authReject(host)
	case "tls":
		tlsReject(host)
	case "ack":
		ack(open(host))
	case "stats":
		statsSuite(open(host))
	case "signals":
		signalsSuite(open(host))
	case "pids":
		pidsReport(open(host))
	case "stats-restart":
		statsRestart(open(host))
	case "recover":
		s := open(host)
		statsRuntime(s)
		recoverCheck(s, true)
	case "report":
		recoverCheck(open(host), false)
	default:
		fail("unknown PGMODE %q", mode)
	}
	fmt.Println("SQL_OK")
}
