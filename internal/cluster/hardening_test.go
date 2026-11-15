package cluster

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func entry(id, addr string, seen time.Time) memberEntry {
	return memberEntry{ID: id, Addr: addr, Status: StatusAlive, LastSeen: seen.Format(time.RFC3339)}
}

func TestGossip_MemberTableIsBounded(t *testing.T) {
	c := NewSwimCluster("127.0.0.1:7946", 0, 0, 0)
	entries := make([]memberEntry, 0, 2*maxMembers)
	for i := range 2 * maxMembers {
		entries = append(entries, entry(fmt.Sprintf("node-%d", i), "10.0.0.2:7946", time.Now()))
	}
	c.HandleGossip(gossipPayload{MemberID: "node-0", Members: entries})
	require.LessOrEqual(t, len(c.Members()), maxMembers)
}

func TestGossip_FutureLastSeenIsClamped(t *testing.T) {
	c := NewSwimCluster("127.0.0.1:7946", 0, 0, 0)
	c.HandleGossip(gossipPayload{MemberID: "other", Members: []memberEntry{entry("victim", "10.0.0.2:7946", time.Now().AddDate(100, 0, 0))}})
	for _, m := range c.Members() {
		if m.ID == "victim" {
			require.False(t, m.LastSeen.After(time.Now().Add(maxClockSkew)))
		}
	}
}

func memberByID(c *SwimCluster, id string) (Member, bool) {
	for _, m := range c.Members() {
		if m.ID == id {
			return m, true
		}
	}
	return Member{}, false
}

func TestGossip_AddressOfAKnownMemberNeverMoves(t *testing.T) {
	c := NewSwimCluster("127.0.0.1:7946", 0, 0, 0)
	start := time.Now().Add(-time.Minute)
	c.HandleGossip(gossipPayload{MemberID: "node-b", Members: []memberEntry{entry("node-b", "10.0.0.2:7946", start)}})
	// A third party relays a newer entry for node-b with another address.
	c.HandleGossip(gossipPayload{MemberID: "node-c", Members: []memberEntry{entry("node-b", "10.0.0.66:7946", time.Now())}})
	m, _ := memberByID(c, "node-b")
	require.Equal(t, "10.0.0.2:7946", m.Addr)
	// The same entry with member_id set to node-b: the token is shared, so the
	// body field proves nothing and the address stays put.
	c.HandleGossip(gossipPayload{MemberID: "node-b", Members: []memberEntry{entry("node-b", "10.0.0.66:7946", time.Now().Add(time.Second))}})
	m, _ = memberByID(c, "node-b")
	require.Equal(t, "10.0.0.2:7946", m.Addr)
}

// leftReplyServer answers gossip as node-b announcing that it left.
func leftReplyServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		e := entry("node-b", "10.0.0.2:7946", time.Now())
		e.Status = StatusLeft
		_ = json.NewEncoder(w).Encode(gossipPayload{MemberID: "node-b", Members: []memberEntry{e}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGossip_LeftNeedsTheMemberAtItsAddress(t *testing.T) {
	left := entry("node-b", "10.0.0.2:7946", time.Now())
	left.Status = StatusLeft

	c := NewSwimCluster("127.0.0.1:7946", 0, 0, 0)
	c.HandleGossip(gossipPayload{MemberID: "node-b", Members: []memberEntry{entry("node-b", "10.0.0.2:7946", time.Now())}})

	// Relayed by a third party, or claimed by any token holder in inbound
	// gossip: ignored.
	c.HandleGossip(gossipPayload{MemberID: "node-c", Members: []memberEntry{left}})
	c.HandleGossip(gossipPayload{MemberID: "node-b", Members: []memberEntry{left}})
	m, _ := memberByID(c, "node-b")
	require.Equal(t, StatusAlive, m.Status)

	// A reply from some other address claiming to be node-b: ignored.
	impostor := leftReplyServer(t)
	require.NoError(t, c.Join(context.Background(), impostor.Listener.Addr().String()))
	m, _ = memberByID(c, "node-b")
	require.Equal(t, StatusAlive, m.Status)

	// The reply from node-b's recorded address: accepted.
	genuine := leftReplyServer(t)
	c.mu.Lock()
	c.members["node-b"].Addr = genuine.Listener.Addr().String()
	c.mu.Unlock()
	require.NoError(t, c.Join(context.Background(), genuine.Listener.Addr().String()))
	m, _ = memberByID(c, "node-b")
	require.Equal(t, StatusLeft, m.Status)
}

func TestGossip_RejectsLinkLocalAddresses(t *testing.T) {
	require.False(t, validEntry(entry("node-x", "169.254.169.254:80", time.Now())))
	require.False(t, validEntry(entry("node-x", "0.0.0.0:7946", time.Now())))
	require.True(t, validEntry(entry("node-x", "10.0.0.2:7946", time.Now())))
}

// gzipJSONServer answers every request with body, gzip-encoded, so the Go
// HTTP client inflates it transparently, as it would a hostile peer's reply.
func gzipJSONServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(body)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGossip_OversizedReplyIsDiscarded(t *testing.T) {
	// Well over maxGossipBody once inflated, a few KiB on the wire. Each
	// entry is valid, so only the size limit can stop the merge.
	var body bytes.Buffer
	body.WriteString(`{"member_id":"evil","members":[`)
	for i := 0; body.Len() <= 2*maxGossipBody; i++ {
		if i > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, `{"id":"n-%d","addr":"10.0.0.9:7946","status":"alive"}`, i)
	}
	body.WriteString(`]}`)
	srv := gzipJSONServer(t, body.Bytes())

	c := NewSwimCluster("127.0.0.1:7946", 0, 0, 0)
	c.mu.Lock()
	c.members["peer"] = &Member{ID: "peer", Addr: srv.Listener.Addr().String(), Status: StatusAlive, LastSeen: time.Now()}
	c.mu.Unlock()

	c.doGossip(context.Background())
	require.Len(t, c.Members(), 2, "an oversized reply must not change the member table")

	// Join takes the same path.
	require.NoError(t, c.Join(context.Background(), srv.Listener.Addr().String()))
	require.Len(t, c.Members(), 2)
}

func TestGossip_TooManyEntriesIsRejected(t *testing.T) {
	entries := make([]memberEntry, maxMembers+1)
	for i := range entries {
		entries[i] = entry(fmt.Sprintf("n-%d", i), "10.0.0.9:7946", time.Now())
	}
	data, err := json.Marshal(gossipPayload{MemberID: "evil", Members: entries})
	require.NoError(t, err)
	require.Less(t, len(data), maxGossipBody, "the test must exercise the entry cap, not the byte cap")

	// As a reply: discarded whole.
	srv := gzipJSONServer(t, data)
	c := NewSwimCluster("127.0.0.1:7946", 0, 0, 0)
	require.NoError(t, c.Join(context.Background(), srv.Listener.Addr().String()))
	require.Len(t, c.Members(), 1)

	// As a request: refused before it reaches the member table.
	mux := http.NewServeMux()
	RegisterGossipHandler(mux, c)
	hs := httptest.NewServer(mux)
	defer hs.Close()
	resp, err := http.Post(hs.URL+"/cluster/gossip", "application/json", bytes.NewReader(data))
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Len(t, c.Members(), 1)
}
