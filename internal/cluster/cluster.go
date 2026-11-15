package cluster

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type MemberStatus string

const (
	StatusAlive   MemberStatus = "alive"
	StatusSuspect MemberStatus = "suspect"
	StatusDead    MemberStatus = "dead"
	StatusLeft    MemberStatus = "left"
)

type Member struct {
	ID       string       `json:"id"`
	Addr     string       `json:"addr"`
	Status   MemberStatus `json:"status"`
	VMCount  int          `json:"vm_count"`
	CPUCap   int          `json:"cpu_capacity"`
	MemCap   int64        `json:"mem_capacity_bytes"`
	LastSeen time.Time    `json:"last_seen"`
}

// memberIDPattern is the shape a gossiped member ID must have to be merged:
// the hex quartets generateID produces, plus the punctuation operators use for
// hand-set node names. Anything else — control characters that would forge log
// lines, or an unbounded blob that would bloat the member table — is dropped.
var memberIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// maxGossipBody bounds every gossip message this node decodes: requests to the
// gossip handler and the replies to its own gossip and join requests alike. The
// limit applies to the decoded stream, after the HTTP transport has inflated a
// gzip-encoded reply, so a small compressed body cannot expand past it.
const maxGossipBody = 1 << 20 // 1 MiB

// errGossipTooLarge reports a gossip message over maxGossipBody bytes or with
// more entries than the member table can hold.
var errGossipTooLarge = errors.New("cluster: gossip message too large")

// decodeGossip decodes one gossip message from r, reading at most
// maxGossipBody bytes, and rejects a message carrying more than maxMembers
// entries: no honest node sends more entries than its own table can hold.
func decodeGossip(r io.Reader) (gossipPayload, error) {
	var p gossipPayload
	lr := &io.LimitedReader{R: r, N: maxGossipBody + 1}
	err := json.NewDecoder(lr).Decode(&p)
	if lr.N <= 0 || (err == nil && len(p.Members) > maxMembers) {
		return gossipPayload{}, errGossipTooLarge
	}
	if err != nil {
		return gossipPayload{}, err
	}
	return p, nil
}

// validEntries returns the well-formed entries of one gossip message. It reads
// only fields fixed at construction, so callers run it before taking c.mu, and
// it logs dropped entries once per message rather than once per entry, so a
// message full of junk costs one log line. A message with more entries than the
// member table can hold is rejected whole (ok is false).
func (c *SwimCluster) validEntries(entries []memberEntry) (valid []memberEntry, ok bool) {
	if len(entries) > maxMembers {
		slog.Warn("cluster: dropping oversized gossip message", "entries", len(entries))
		return nil, false
	}
	valid = make([]memberEntry, 0, len(entries))
	for _, e := range entries {
		if validEntry(e, c.allowLoopback) {
			valid = append(valid, e)
		}
	}
	if dropped := len(entries) - len(valid); dropped > 0 {
		// Counted rather than logged verbatim: the entries are unauthenticated
		// wire data unless a cluster token is configured.
		slog.Warn("cluster: dropping malformed gossip entries", "count", dropped)
	}
	return valid, true
}

// hostNamePattern is the shape a non-IP member host must have: DNS name
// characters only. It keeps "user@169.254.169.254" and "host/path" out of the
// gossip URL, where they would change which host is dialed.
var hostNamePattern = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)

// blockedGossipIP reports whether this node must never send gossip to ip:
// link-local (cloud metadata), unspecified, multicast, and loopback unless
// allowLoopback is set. The net.IP predicates also match the IPv4-mapped IPv6
// form of each IPv4 address.
func blockedGossipIP(ip net.IP, allowLoopback bool) bool {
	return ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || (ip.IsLoopback() && !allowLoopback)
}

// isLoopbackHost reports whether host is a loopback IP literal or the name
// localhost.
func isLoopbackHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	return h == "localhost" || strings.HasSuffix(h, ".localhost")
}

// validEntry reports whether a member entry received over gossip is well-formed
// enough to merge into the member table. allowLoopback is the node's loopback
// policy (see SwimCluster.allowLoopback). This is an early filter only: a host
// name can resolve to anything, so the gossip client checks every address it
// actually connects to as well (gossipDialControl).
func validEntry(e memberEntry, allowLoopback bool) bool {
	if !memberIDPattern.MatchString(e.ID) {
		return false
	}
	// SplitHostPort accepts "10.0.0.2:" (empty port) without an error, and that
	// address would be dialed for gossip, so check both halves explicitly.
	host, port, err := net.SplitHostPort(e.Addr)
	if err != nil || host == "" || port == "" {
		return false
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return false
	}
	// A member entry decides where this node sends requests, so never accept
	// one that points at an address the gossip client would refuse to dial.
	if ip := net.ParseIP(host); ip != nil {
		if blockedGossipIP(ip, allowLoopback) {
			return false
		}
	} else if !hostNamePattern.MatchString(host) || (!allowLoopback && isLoopbackHost(host)) {
		return false
	}
	switch e.Status {
	case StatusAlive, StatusSuspect, StatusDead, StatusLeft:
	default:
		return false
	}
	return true
}

// sanitizeForLog returns a copy of the entry with fields sanitized for safe logging.
// This function is called AFTER validEntry has accepted the entry, so the values
// already match memberIDPattern and are valid host:port pairs. The explicit copy
// ensures static analyzers recognize the sanitization boundary.
func sanitizeForLog(e memberEntry) (id, addr string) {
	if !memberIDPattern.MatchString(e.ID) {
		return "[invalid]", "[invalid]"
	}
	host, port, err := net.SplitHostPort(e.Addr)
	if err != nil || host == "" || port == "" {
		return stripLogControl(e.ID), "[invalid]"
	}
	return stripLogControl(e.ID), stripLogControl(net.JoinHostPort(host, port))
}

// stripLogControl removes carriage returns and newlines so a value cannot forge
// or split log lines (log injection). It is a plain string transformation rather
// than a regex guard: taint trackers model strings.ReplaceAll as a sanitizer that
// crosses function boundaries, whereas a MatchString guard only clears taint
// inside the function that performs it.
func stripLogControl(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	return s
}

type gossipPayload struct {
	MemberID string        `json:"member_id"`
	Members  []memberEntry `json:"members"`
}

type memberEntry struct {
	ID       string       `json:"id"`
	Addr     string       `json:"addr"`
	Status   MemberStatus `json:"status"`
	VMCount  int          `json:"vm_count"`
	CPUCap   int          `json:"cpu_capacity"`
	MemCap   int64        `json:"mem_capacity_bytes"`
	LastSeen string       `json:"last_seen"`
}

type SwimCluster struct {
	mu          sync.RWMutex
	local       Member
	members     map[string]*Member
	httpClient  *http.Client
	period      time.Duration
	suspTimeout time.Duration
	deadTimeout time.Duration
	cancel      context.CancelFunc
	// token is an optional shared secret. When non-empty, outbound gossip carries
	// it as a bearer token and the gossip handler rejects requests without it, so
	// a stranger who can merely reach the port cannot inject membership updates.
	// It is set once at construction and never mutated, so it is read lock-free.
	token string
	// allowLoopback lets gossip reach loopback addresses. It is set only when
	// this node itself advertises a loopback address (a single-host or test
	// cluster, e.g. --cluster-addr :7946), since such a node can only cluster
	// with nodes on the same host anyway. Fixed at construction; read lock-free.
	allowLoopback bool
}

// gossipDialControl is the net.Dialer Control hook of the gossip client. It
// runs for every connection attempt, after DNS resolution, so a member host
// name that resolves to a blocked address is refused just like the literal.
func gossipDialControl(allowLoopback bool) func(network, address string, _ syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || blockedGossipIP(ip, allowLoopback) {
			return fmt.Errorf("cluster: refusing to gossip to %s", host)
		}
		return nil
	}
}

// newGossipClient builds the HTTP client used for gossip and join requests. It
// never follows redirects (a 3xx reply is simply not a gossip reply), ignores
// proxy environment variables (a proxy would hide the real destination from
// the dial check) and checks every address it connects to.
func newGossipClient(allowLoopback bool) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = (&net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   gossipDialControl(allowLoopback),
	}).DialContext
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Option configures a SwimCluster at construction time.
type Option func(*SwimCluster)

// WithToken sets a shared secret that authenticates gossip between nodes. All
// nodes in a cluster must use the same token; an empty token (the default)
// leaves the gossip endpoint unauthenticated, preserving prior behavior.
func WithToken(token string) Option {
	return func(c *SwimCluster) { c.token = token }
}

func generateID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x", b[0:2], b[2:4], b[4:6], b[6:8])
}

func NewSwimCluster(addr string, vmCount int, cpuCap int, memCap int64, opts ...Option) *SwimCluster {
	id := generateID()
	c := &SwimCluster{
		local: Member{
			ID:      id,
			Addr:    addr,
			Status:  StatusAlive,
			VMCount: vmCount,
			CPUCap:  cpuCap,
			MemCap:  memCap,
		},
		members:     make(map[string]*Member),
		period:      5 * time.Second,
		suspTimeout: 15 * time.Second,
		deadTimeout: 30 * time.Second,
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		c.allowLoopback = isLoopbackHost(host)
	}
	c.httpClient = newGossipClient(c.allowLoopback)
	for _, opt := range opts {
		opt(c)
	}
	c.members[id] = &c.local
	return c
}

// setAuth adds the bearer token to an outbound gossip request when one is
// configured; with no token it is a no-op and the request goes out unauthenticated.
func (c *SwimCluster) setAuth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

func (c *SwimCluster) LocalID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.local.ID
}

// Join contacts each seed in turn and merges its reply. The HTTP exchange runs
// without holding c.mu, so a slow or hostile seed cannot stall the gossip
// handler or member listing; only the merge of an already validated reply takes
// the lock.
func (c *SwimCluster) Join(ctx context.Context, seedAddrs ...string) error {
	for _, addr := range seedAddrs {
		c.mu.RLock()
		if addr == c.local.Addr {
			c.mu.RUnlock()
			continue
		}
		payload := gossipPayload{
			MemberID: c.local.ID,
			Members:  c.toEntriesLocked(),
		}
		c.mu.RUnlock()
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("cluster join marshal: %w", err)
		}
		url := fmt.Sprintf("http://%s/cluster/gossip", addr)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("cluster join request build: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		c.setAuth(req)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			slog.Warn("cluster join failed", "seed", addr, "err", err)
			continue
		}
		if resp.StatusCode == http.StatusOK {
			c.mergeReply(resp.Body, addr)
		}
		resp.Body.Close()
	}
	return nil
}

func (c *SwimCluster) Start(ctx context.Context) {
	ctx, c.cancel = context.WithCancel(ctx)
	go c.gossipLoop(ctx)
	go c.suspicionLoop(ctx)
}

func (c *SwimCluster) Leave() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.local.Status = StatusLeft
	if m, ok := c.members[c.local.ID]; ok {
		m.Status = StatusLeft
	}
	if c.cancel != nil {
		c.cancel()
	}
}

func (c *SwimCluster) Members() []Member {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Member, 0, len(c.members))
	for _, m := range c.members {
		out = append(out, *m)
	}
	return out
}

func (c *SwimCluster) AliveMembers() []Member {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []Member
	for _, m := range c.members {
		if m.Status == StatusAlive {
			out = append(out, *m)
		}
	}
	return out
}

func (c *SwimCluster) UpdateLocal(vmCount int, cpuCap int, memCap int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.local.VMCount = vmCount
	c.local.CPUCap = cpuCap
	c.local.MemCap = memCap
	c.local.LastSeen = time.Now()
	if m, ok := c.members[c.local.ID]; ok {
		m.VMCount = vmCount
		m.CPUCap = cpuCap
		m.MemCap = memCap
		m.LastSeen = time.Now()
	}
}

func (c *SwimCluster) HandleGossip(payload gossipPayload) gossipPayload {
	entries, ok := c.validEntries(payload.Members)

	c.mu.Lock()
	defer c.mu.Unlock()

	if ok {
		c.mergeEntriesLocked(payload.MemberID, "", entries)

		if m, found := c.members[payload.MemberID]; found {
			m.LastSeen = time.Now()
			if m.Status == StatusSuspect {
				m.Status = StatusAlive
				slog.Debug("cluster member recovered from suspicion", "member", m.ID)
			}
		}
	}

	return gossipPayload{
		MemberID: c.local.ID,
		Members:  c.toEntriesLocked(),
	}
}

func (c *SwimCluster) gossipLoop(ctx context.Context) {
	ticker := time.NewTicker(c.period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.doGossip(ctx)
		}
	}
}

func (c *SwimCluster) doGossip(ctx context.Context) {
	c.mu.RLock()
	var targets []string
	for _, m := range c.members {
		if m.ID != c.local.ID && (m.Status == StatusAlive || m.Status == StatusSuspect) {
			targets = append(targets, m.Addr)
		}
	}
	payload := gossipPayload{
		MemberID: c.local.ID,
		Members:  c.toEntriesLocked(),
	}
	c.mu.RUnlock()

	if len(targets) == 0 {
		return
	}

	target := targets[0]
	data, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("cluster gossip marshal", "err", err)
		return
	}
	url := fmt.Sprintf("http://%s/cluster/gossip", target)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		slog.Warn("cluster gossip request build", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.markSuspect(target)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		c.mergeReply(resp.Body, target)
	}
}

// mergeReply decodes and validates a gossip reply read from peer (the address
// this node dialed) without holding c.mu, then merges it. A reply that is too
// large, malformed or carries too many entries changes no state.
func (c *SwimCluster) mergeReply(body io.Reader, peer string) {
	remote, err := decodeGossip(body)
	if err != nil {
		// The decode error is not logged: it can quote reply bytes.
		slog.Warn("cluster: discarding gossip reply", "peer", stripLogControl(peer), "too_large", errors.Is(err, errGossipTooLarge))
		return
	}
	entries, ok := c.validEntries(remote.Members)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mergeEntriesLocked(remote.MemberID, peer, entries)
	if m, found := c.members[remote.MemberID]; found {
		m.LastSeen = time.Now()
	}
}

func (c *SwimCluster) suspicionLoop(ctx context.Context) {
	ticker := time.NewTicker(c.period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.checkSuspicions()
		}
	}
}

func (c *SwimCluster) checkSuspicions() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for _, m := range c.members {
		if m.ID == c.local.ID {
			continue
		}
		if m.Status == StatusAlive && now.Sub(m.LastSeen) > c.suspTimeout {
			m.Status = StatusSuspect
			slog.Warn("cluster member suspected", "member", m.ID, "addr", m.Addr, "last_seen", m.LastSeen)
		}
		if m.Status == StatusSuspect && now.Sub(m.LastSeen) > c.deadTimeout {
			m.Status = StatusDead
			slog.Warn("cluster member declared dead", "member", m.ID, "addr", m.Addr)
		}
		// Forget members that have been gone for a while, so dead and left
		// entries do not accumulate.
		if (m.Status == StatusDead || m.Status == StatusLeft) && now.Sub(m.LastSeen) > 10*c.deadTimeout {
			delete(c.members, m.ID)
		}
	}
}

func (c *SwimCluster) markSuspect(addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.members {
		if m.Addr == addr && m.ID != c.local.ID && m.Status == StatusAlive {
			m.Status = StatusSuspect
			slog.Warn("cluster member suspected (gossip failed)", "member", m.ID, "addr", addr)
		}
	}
}

// maxMembers bounds the member table, so gossip cannot grow it without limit.
const maxMembers = 1024

// maxClockSkew is how far in the future a gossiped last-seen time may be; a
// later one is clamped to now, so an entry cannot pin itself as fresh forever.
const maxClockSkew = time.Minute

// mergeEntriesLocked folds gossiped entries from sender into the table.
// entries must already have passed validEntries, which callers run before
// taking c.mu.
//
// Every node holds the same cluster token, so the token proves membership but
// not identity, and sender (the member_id in the message body) is only a claim.
// The rules below therefore never let that claim alone change what this node
// records about another member:
//
//   - A known member's address never changes over gossip. Member IDs are
//     random per process and a node's advertised address is fixed for the life
//     of the process, so an honest node never moves; a restarted node joins
//     under a new ID and its old entry ages out.
//   - "left" is accepted only from the member itself, and only on a reply to a
//     request this node sent to the member's recorded address (dialed is that
//     address; it is empty for inbound gossip). The claim is then bound to
//     whoever answers at that address, not to a body field any peer can set.
//   - Any peer may still relay liveness and newer capacity figures, introduce
//     members this node has not seen, and declare a member dead, as before.
func (c *SwimCluster) mergeEntriesLocked(sender, dialed string, entries []memberEntry) {
	now := time.Now()
	for _, e := range entries {
		if e.ID == c.local.ID {
			continue
		}
		existing, ok := c.members[e.ID]
		entryTime, _ := time.Parse(time.RFC3339, e.LastSeen)
		if entryTime.After(now.Add(maxClockSkew)) {
			entryTime = now
		}
		if !ok {
			if len(c.members) >= maxMembers {
				slog.Warn("cluster: member table full; ignoring new member")
				continue
			}
			c.members[e.ID] = &Member{
				ID:       e.ID,
				Addr:     e.Addr,
				Status:   e.Status,
				VMCount:  e.VMCount,
				CPUCap:   e.CPUCap,
				MemCap:   e.MemCap,
				LastSeen: entryTime,
			}
			safeID, safeAddr := sanitizeForLog(e)
			slog.Info("cluster member discovered", "member", safeID, "addr", safeAddr)
			continue
		}

		// The member answered at its recorded address and speaks for itself.
		firstHand := e.ID == sender && dialed != "" && existing.Addr == dialed

		if e.Status == StatusDead || (e.Status == StatusLeft && firstHand) {
			existing.Status = e.Status
		} else if entryTime.After(existing.LastSeen) {
			existing.VMCount = e.VMCount
			existing.CPUCap = e.CPUCap
			existing.MemCap = e.MemCap
			existing.LastSeen = entryTime
		}

		if e.Status == StatusAlive && existing.Status == StatusSuspect {
			existing.Status = StatusAlive
		}
	}
}

func (c *SwimCluster) toEntriesLocked() []memberEntry {
	out := make([]memberEntry, 0, len(c.members))
	for _, m := range c.members {
		out = append(out, memberEntry{
			ID:       m.ID,
			Addr:     m.Addr,
			Status:   m.Status,
			VMCount:  m.VMCount,
			CPUCap:   m.CPUCap,
			MemCap:   m.MemCap,
			LastSeen: m.LastSeen.Format(time.RFC3339),
		})
	}
	return out
}

// authorized reports whether an inbound gossip request carries the configured
// shared token. When no token is configured it always returns true, so the
// endpoint stays open exactly as before; when one is set, a request must present
// a matching "Authorization: Bearer <token>" header (compared in constant time).
func (c *SwimCluster) authorized(r *http.Request) bool {
	if c.token == "" {
		return true
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(c.token)) == 1
}

func RegisterGossipHandler(mux *http.ServeMux, cluster *SwimCluster) {
	mux.HandleFunc("/cluster/gossip", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !cluster.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		payload, err := decodeGossip(http.MaxBytesReader(w, r.Body, maxGossipBody))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		resp := cluster.HandleGossip(payload)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			slog.Warn("cluster gossip encode response", "err", err)
		}
	})
}

func ParseAddr(hostPort string) string {
	h, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return hostPort
	}
	if h == "" || h == "0.0.0.0" || h == "::" {
		return "127.0.0.1" + hostPort[strings.LastIndex(hostPort, ":"):]
	}
	return hostPort
}

type MemberListerAdapter struct {
	Cluster *SwimCluster
}

func (a *MemberListerAdapter) Members() []MemberInfo {
	members := a.Cluster.Members()
	out := make([]MemberInfo, len(members))
	for i, m := range members {
		out[i] = MemberInfo{
			ID:       m.ID,
			Addr:     m.Addr,
			Status:   string(m.Status),
			VMCount:  m.VMCount,
			CPUCap:   m.CPUCap,
			MemCap:   m.MemCap,
			LastSeen: m.LastSeen,
		}
	}
	return out
}

type MemberInfo struct {
	ID       string
	Addr     string
	Status   string
	VMCount  int
	CPUCap   int
	MemCap   int64
	LastSeen time.Time
}
