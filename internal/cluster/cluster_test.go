package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewSwimCluster(t *testing.T) {
	c := NewSwimCluster("127.0.0.1:7946", 0, 0, 0)
	require.NotEmpty(t, c.LocalID())
	require.Equal(t, StatusAlive, c.local.Status)

	members := c.Members()
	require.Len(t, members, 1)
	require.Equal(t, c.LocalID(), members[0].ID)
}

func TestSwimCluster_Members_AliveMembers(t *testing.T) {
	c := NewSwimCluster("127.0.0.1:7946", 0, 0, 0)

	c.mu.Lock()
	c.members["remote-1"] = &Member{ID: "remote-1", Addr: "10.0.0.2:7946", Status: StatusAlive}
	c.members["remote-2"] = &Member{ID: "remote-2", Addr: "10.0.0.3:7946", Status: StatusDead}
	c.mu.Unlock()

	all := c.Members()
	require.Len(t, all, 3)

	alive := c.AliveMembers()
	require.Len(t, alive, 2)
	for _, m := range alive {
		require.Equal(t, StatusAlive, m.Status)
	}
}

func TestSwimCluster_UpdateLocal(t *testing.T) {
	c := NewSwimCluster("127.0.0.1:7946", 0, 0, 0)
	c.UpdateLocal(5, 8, 16*1024*1024*1024)

	members := c.Members()
	var local Member
	for _, m := range members {
		if m.ID == c.LocalID() {
			local = m
		}
	}
	require.Equal(t, 5, local.VMCount)
	require.Equal(t, 8, local.CPUCap)
	require.Equal(t, int64(16*1024*1024*1024), local.MemCap)
	require.False(t, local.LastSeen.IsZero())
}

func TestSwimCluster_HandleGossip_Discovery(t *testing.T) {
	c1 := NewSwimCluster("127.0.0.1:7946", 0, 0, 0)
	c2 := NewSwimCluster("127.0.0.1:7947", 3, 4, 8*1024*1024*1024)

	payload := gossipPayload{
		MemberID: c2.LocalID(),
		Members:  c2.toEntriesLocked(),
	}
	resp := c1.HandleGossip(payload)

	require.Equal(t, c1.LocalID(), resp.MemberID)

	members := c1.Members()
	require.Len(t, members, 2)

	var remote Member
	for _, m := range members {
		if m.ID == c2.LocalID() {
			remote = m
		}
	}
	require.Equal(t, StatusAlive, remote.Status)
	require.Equal(t, "127.0.0.1:7947", remote.Addr)
	require.Equal(t, 3, remote.VMCount)
}

func TestSwimCluster_HandleGossip_SuspectRecovery(t *testing.T) {
	c1 := NewSwimCluster("127.0.0.1:7946", 0, 0, 0)
	c2ID := "node-2"

	c1.mu.Lock()
	c1.members[c2ID] = &Member{ID: c2ID, Addr: "10.0.0.2:7946", Status: StatusSuspect}
	c1.mu.Unlock()

	payload := gossipPayload{
		MemberID: c2ID,
		Members:  []memberEntry{{ID: c2ID, Addr: "10.0.0.2:7946", Status: StatusAlive, LastSeen: time.Now().Format(time.RFC3339)}},
	}
	c1.HandleGossip(payload)

	members := c1.Members()
	var remote Member
	for _, m := range members {
		if m.ID == c2ID {
			remote = m
		}
	}
	require.Equal(t, StatusAlive, remote.Status)
}

func TestSwimCluster_HandleGossip_DeadPropagation(t *testing.T) {
	c1 := NewSwimCluster("127.0.0.1:7946", 0, 0, 0)
	c2ID := "node-2"

	c1.mu.Lock()
	c1.members[c2ID] = &Member{ID: c2ID, Addr: "10.0.0.2:7946", Status: StatusAlive, LastSeen: time.Now()}
	c1.mu.Unlock()

	payload := gossipPayload{
		MemberID: "relay-node",
		Members: []memberEntry{
			{ID: c2ID, Addr: "10.0.0.2:7946", Status: StatusDead, LastSeen: time.Now().Format(time.RFC3339)},
		},
	}
	c1.HandleGossip(payload)

	members := c1.Members()
	var remote Member
	for _, m := range members {
		if m.ID == c2ID {
			remote = m
		}
	}
	require.Equal(t, StatusDead, remote.Status)
}

func TestSwimCluster_Leave(t *testing.T) {
	c := NewSwimCluster("127.0.0.1:7946", 0, 0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)

	c.Leave()

	members := c.Members()
	var local Member
	for _, m := range members {
		if m.ID == c.LocalID() {
			local = m
		}
	}
	require.Equal(t, StatusLeft, local.Status)
}

func TestSwimCluster_Join_HTTP(t *testing.T) {
	c1 := NewSwimCluster("127.0.0.1:7946", 2, 4, 8*1024*1024*1024)
	c2 := NewSwimCluster("127.0.0.1:7947", 5, 8, 16*1024*1024*1024)

	mux := http.NewServeMux()
	RegisterGossipHandler(mux, c1)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	err := c2.Join(context.Background(), srv.Listener.Addr().String())
	require.NoError(t, err)

	members := c2.Members()
	require.Len(t, members, 2)
}

func TestSwimCluster_GossipLoop_Integration(t *testing.T) {
	c1 := NewSwimCluster("127.0.0.1:0", 0, 0, 0)
	c2 := NewSwimCluster("127.0.0.1:0", 1, 2, 4096)

	mux1 := http.NewServeMux()
	RegisterGossipHandler(mux1, c1)
	srv1 := httptest.NewServer(mux1)
	defer srv1.Close()

	mux2 := http.NewServeMux()
	RegisterGossipHandler(mux2, c2)
	srv2 := httptest.NewServer(mux2)
	defer srv2.Close()

	c1.mu.Lock()
	c1.local.Addr = srv1.Listener.Addr().String()
	c1.period = 200 * time.Millisecond
	c1.suspTimeout = 2 * time.Second
	c1.deadTimeout = 5 * time.Second
	c1.mu.Unlock()

	c2.mu.Lock()
	c2.local.Addr = srv2.Listener.Addr().String()
	c2.period = 200 * time.Millisecond
	c2.suspTimeout = 2 * time.Second
	c2.deadTimeout = 5 * time.Second
	c2.mu.Unlock()

	err := c1.Join(context.Background(), srv2.Listener.Addr().String())
	require.NoError(t, err)

	err = c2.Join(context.Background(), srv1.Listener.Addr().String())
	require.NoError(t, err)

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	c1.Start(ctx1)
	c2.Start(ctx2)

	require.Eventually(t, func() bool {
		return len(c1.Members()) >= 2 && len(c2.Members()) >= 2
	}, 3*time.Second, 100*time.Millisecond, "both clusters should know each other")
}

func TestSwimCluster_ConcurrentAccess(t *testing.T) {
	c := NewSwimCluster("127.0.0.1:7946", 0, 0, 0)
	c.mu.Lock()
	c.members["remote-1"] = &Member{ID: "remote-1", Addr: "10.0.0.2:7946", Status: StatusAlive}
	c.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Members()
		}()
	}
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.UpdateLocal(i, i, int64(i))
		}()
	}
	wg.Wait()
}

func TestRegisterGossipHandler_MethodNotAllowed(t *testing.T) {
	c := NewSwimCluster("127.0.0.1:7946", 0, 0, 0)
	mux := http.NewServeMux()
	RegisterGossipHandler(mux, c)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/cluster/gossip")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

func TestRegisterGossipHandler_BadRequest(t *testing.T) {
	c := NewSwimCluster("127.0.0.1:7946", 0, 0, 0)
	mux := http.NewServeMux()
	RegisterGossipHandler(mux, c)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/cluster/gossip", "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestRegisterGossipHandler_ValidGossip(t *testing.T) {
	c := NewSwimCluster("127.0.0.1:7946", 0, 0, 0)
	mux := http.NewServeMux()
	RegisterGossipHandler(mux, c)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	remote := NewSwimCluster("127.0.0.1:7947", 3, 4, 8*1024*1024*1024)
	payload := gossipPayload{
		MemberID: remote.LocalID(),
		Members:  remote.toEntriesLocked(),
	}
	data, err := json.Marshal(payload)
	require.NoError(t, err)

	resp, err := http.Post(srv.URL+"/cluster/gossip", "application/json", nil)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/cluster/gossip", nil)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Body = io.NopCloser(bytes.NewReader(data))
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result gossipPayload
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)
	require.Equal(t, c.LocalID(), result.MemberID)
	require.True(t, len(result.Members) >= 2)
}

func TestParseAddr(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"0.0.0.0:7946", "127.0.0.1:7946"},
		{"[::]:7946", "127.0.0.1:7946"},
		{"10.0.0.5:7946", "10.0.0.5:7946"},
		{"localhost:7946", "localhost:7946"},
	}
	for _, tt := range tests {
		got := ParseAddr(tt.input)
		require.Equal(t, tt.want, got)
	}
}

func TestMergeEntriesLocked_NewerData(t *testing.T) {
	c := NewSwimCluster("127.0.0.1:7946", 0, 0, 0)
	rid := "remote-1"
	oldTime := time.Now().Add(-1 * time.Minute)

	c.mu.Lock()
	c.members[rid] = &Member{ID: rid, Addr: "10.0.0.2:7946", Status: StatusAlive, VMCount: 1, LastSeen: oldTime}
	c.mu.Unlock()

	newTime := time.Now()
	entry := memberEntry{
		ID:       rid,
		Addr:     "10.0.0.2:7946",
		Status:   StatusAlive,
		VMCount:  5,
		LastSeen: newTime.Format(time.RFC3339),
	}
	c.HandleGossip(gossipPayload{MemberID: "relay", Members: []memberEntry{entry}})

	members := c.Members()
	var remote Member
	for _, m := range members {
		if m.ID == rid {
			remote = m
		}
	}
	require.Equal(t, 5, remote.VMCount)
}
