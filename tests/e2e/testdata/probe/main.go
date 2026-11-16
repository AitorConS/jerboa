// probe is the guest the end-to-end suite boots. It reports what it sees
// from inside the VM (environment, volume contents, name resolution, what it
// can connect to) over HTTP on :8080 and on the serial console, and it can
// be told to misbehave: flood the console, fill its volume, exit with a
// status, or write records until the host kills it.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	dataDir    = "/data"
	bootsFile  = dataDir + "/boots"
	journal    = dataDir + "/journal"
	recordSize = 64
)

func main() {
	if code := os.Getenv("PROBE_EXIT"); code != "" {
		n, _ := strconv.Atoi(code)
		fmt.Printf("PROBE_EXIT %d\n", n)
		os.Exit(n)
	}

	boots := countBoot()
	valid, tail := readJournal()
	fmt.Printf("PROBE_JOURNAL valid=%d torn=%d\n", valid, tail)
	if os.Getenv("PROBE_JOURNAL") != "" {
		go writeJournal(valid)
	}
	if os.Getenv("PROBE_FLOOD") != "" {
		go flood()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"marker":  os.Getenv("PROBE_MARKER"),
			"boots":   boots,
			"journal": valid,
			"torn":    tail,
		})
	})
	mux.HandleFunc("/write", handleWrite)
	mux.HandleFunc("/read", handleRead)
	mux.HandleFunc("/fill", handleFill)
	mux.HandleFunc("/dial", handleDial)
	mux.HandleFunc("/resolve", handleResolve)
	mux.HandleFunc("/dnsflood", handleDNSFlood)
	mux.HandleFunc("/fetch", handleFetch)
	mux.HandleFunc("/exit", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("code"))
		fmt.Fprintln(w, "bye")
		go func() { time.Sleep(200 * time.Millisecond); os.Exit(n) }()
	})

	ln, err := net.Listen("tcp", ":8080")
	if err != nil {
		fmt.Println("PROBE_FATAL listen:", err)
		os.Exit(3)
	}
	fmt.Printf("PROBE_READY marker=%s boots=%d\n", os.Getenv("PROBE_MARKER"), boots)
	if err := http.Serve(ln, mux); err != nil {
		fmt.Println("PROBE_FATAL serve:", err)
		os.Exit(3)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// countBoot increments the boot counter kept on the volume. Without a volume
// (/data is then part of the read-only-per-boot root image) it returns 0.
func countBoot() int {
	b, _ := os.ReadFile(bootsFile)
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	n++
	if err := writeSynced(bootsFile, []byte(strconv.Itoa(n))); err != nil {
		return 0
	}
	return n
}

func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// record returns journal record n: fixed size, self-describing, so a reader
// can tell a complete record from a torn or foreign one.
func record(n int) []byte {
	head := fmt.Sprintf("rec %012d ", n)
	fill := strings.Repeat(string(rune('a'+n%26)), recordSize-len(head)-1)
	return []byte(head + fill + "\n")
}

// readJournal returns how many leading records of the journal are intact and
// how many bytes follow them (a torn tail, or damage).
func readJournal() (valid int, tail int64) {
	f, err := os.Open(journal)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, 0
	}
	r := bufio.NewReader(f)
	buf := make([]byte, recordSize)
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			break
		}
		if string(buf) != string(record(valid)) {
			break
		}
		valid++
	}
	return valid, fi.Size() - int64(valid)*recordSize
}

// writeJournal appends records after the first intact ones, syncing each and
// only then acknowledging it on the console: the host kills the VM at an
// arbitrary point and checks that every acknowledged record is still there.
func writeJournal(from int) {
	f, err := os.OpenFile(journal, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Println("PROBE_JOURNAL_ERR open:", err)
		return
	}
	if err := f.Truncate(int64(from) * recordSize); err != nil {
		fmt.Println("PROBE_JOURNAL_ERR truncate:", err)
		return
	}
	if _, err := f.Seek(int64(from)*recordSize, io.SeekStart); err != nil {
		fmt.Println("PROBE_JOURNAL_ERR seek:", err)
		return
	}
	for n := from; ; n++ {
		if _, err := f.Write(record(n)); err != nil {
			fmt.Println("PROBE_JOURNAL_ERR write:", err)
			return
		}
		if err := f.Sync(); err != nil {
			fmt.Println("PROBE_JOURNAL_ERR sync:", err)
			return
		}
		fmt.Printf("PROBE_ACK %d\n", n)
	}
}

// flood writes to the console as fast as it can, forever.
func flood() {
	line := strings.Repeat("x", 200)
	for i := 0; ; i++ {
		fmt.Printf("PROBE_FLOOD %d %s\n", i, line)
	}
}

func safeName(r *http.Request) (string, bool) {
	name := r.URL.Query().Get("name")
	if name == "" || name != filepath.Base(name) {
		return "", false
	}
	return filepath.Join(dataDir, name), true
}

func handleWrite(w http.ResponseWriter, r *http.Request) {
	path, ok := safeName(r)
	if !ok {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}
	if err := writeSynced(path, []byte(r.URL.Query().Get("data"))); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Fprintln(w, "ok")
}

func handleRead(w http.ResponseWriter, r *http.Request) {
	path, ok := safeName(r)
	if !ok {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	_, _ = w.Write(b)
}

// handleFill writes to the volume until a write fails and reports how far it
// got and why it stopped.
func handleFill(w http.ResponseWriter, _ *http.Request) {
	f, err := os.OpenFile(filepath.Join(dataDir, "fill"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = byte(i)
	}
	var written int64
	var werr error
	for written < 4<<30 {
		n, err := f.Write(chunk)
		written += int64(n)
		if err != nil {
			werr = err
			break
		}
	}
	if werr == nil {
		werr = f.Sync()
	}
	res := map[string]any{"written": written, "error": ""}
	if werr != nil {
		res["error"] = werr.Error()
	}
	writeJSON(w, res)
}

func handleDial(w http.ResponseWriter, r *http.Request) {
	conn, err := net.DialTimeout("tcp", r.URL.Query().Get("addr"), 3*time.Second)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	line, _ := bufio.NewReader(conn).ReadString('\n')
	writeJSON(w, map[string]any{"ok": true, "banner": strings.TrimSpace(line)})
}

func handleFetch(w http.ResponseWriter, r *http.Request) {
	c := http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(r.URL.Query().Get("url"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 1<<20))
}

func handleResolve(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	start := time.Now()
	addrs, err := net.DefaultResolver.LookupHost(ctx, r.URL.Query().Get("name"))
	res := map[string]any{"addrs": addrs, "error": "", "ms": time.Since(start).Milliseconds()}
	if err != nil {
		res["error"] = err.Error()
	}
	writeJSON(w, res)
}

// handleDNSFlood resolves count distinct names under suffix at once and
// reports how many lookups ended in an answer.
func handleDNSFlood(w http.ResponseWriter, r *http.Request) {
	count, _ := strconv.Atoi(r.URL.Query().Get("count"))
	suffix := r.URL.Query().Get("suffix")
	if count <= 0 || count > 2000 || suffix == "" {
		http.Error(w, "bad count or suffix", http.StatusBadRequest)
		return
	}
	var answered, failed atomic.Int64
	done := make(chan struct{}, count)
	for i := 0; i < count; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			_, err := net.DefaultResolver.LookupHost(ctx, fmt.Sprintf("flood-%d.%s", i, suffix))
			var dnsErr *net.DNSError
			if err == nil || (errors.As(err, &dnsErr) && dnsErr.IsNotFound) {
				answered.Add(1)
				return
			}
			failed.Add(1)
		}(i)
	}
	for i := 0; i < count; i++ {
		<-done
	}
	writeJSON(w, map[string]any{"answered": answered.Load(), "failed": failed.Load()})
}
