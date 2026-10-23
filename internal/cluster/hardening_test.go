package cluster

import (
	"fmt"
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

func TestGossip_OnlyAMemberMovesItsAddress(t *testing.T) {
	c := NewSwimCluster("127.0.0.1:7946", 0, 0, 0)
	start := time.Now().Add(-time.Minute)
	c.HandleGossip(gossipPayload{MemberID: "node-b", Members: []memberEntry{entry("node-b", "10.0.0.2:7946", start)}})
	// A third party relays a newer entry for node-b with another address.
	c.HandleGossip(gossipPayload{MemberID: "node-c", Members: []memberEntry{entry("node-b", "10.0.0.66:7946", time.Now())}})
	addr := func() string {
		for _, m := range c.Members() {
			if m.ID == "node-b" {
				return m.Addr
			}
		}
		return ""
	}
	require.Equal(t, "10.0.0.2:7946", addr())
	c.HandleGossip(gossipPayload{MemberID: "node-b", Members: []memberEntry{entry("node-b", "10.0.0.3:7946", time.Now().Add(time.Second))}})
	require.Equal(t, "10.0.0.3:7946", addr())
}

func TestGossip_RejectsLinkLocalAddresses(t *testing.T) {
	require.False(t, validEntry(entry("node-x", "169.254.169.254:80", time.Now())))
	require.False(t, validEntry(entry("node-x", "0.0.0.0:7946", time.Now())))
	require.True(t, validEntry(entry("node-x", "10.0.0.2:7946", time.Now())))
}
