//go:build linux || (darwin && arm64)

package network

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AitorConS/jerboa/internal/durable"
	"github.com/AitorConS/jerboa/internal/durable/durabletest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What must hold when the network store's files are damaged or its writes
// fail: the daemon still starts, networks stay usable, and an address a VM
// holds is never handed to a second VM.

func openNet(t *testing.T, root string) *Store {
	t.Helper()
	s, err := NewStore(root)
	require.NoError(t, err)
	return s
}

func netNames(t *testing.T, s *Store) []string {
	t.Helper()
	list, err := s.List()
	require.NoError(t, err)
	out := make([]string, 0, len(list))
	for _, n := range list {
		out = append(out, n.Name)
	}
	return out
}

func TestFaults_CreateNetworkIsAllOrNothing(t *testing.T) {
	root := t.TempDir()
	writes := durabletest.Count(t, nil, func() {
		_, err := openNet(t, root).Create("probe", "10.50.0.0/24", "")
		require.NoError(t, err)
	})
	require.Equal(t, 2, writes, "a network is its metadata and its lease state")

	for nth := range writes {
		for _, f := range durabletest.Failures() {
			if nth == 1 && (f.Step == durable.StepSync || f.Step == durable.StepSyncDir) {
				continue // lease state is not flushed, so those steps never run
			}
			t.Run(f.Name+"/write"+string(rune('0'+nth)), func(t *testing.T) {
				root := t.TempDir()
				s := openNet(t, root)
				_, err := s.Create("keep", "10.60.0.0/24", "")
				require.NoError(t, err)

				inj := durabletest.Inject(t, f, nth, nil)
				_, err = s.Create("fresh", "10.61.0.0/24", "")
				require.True(t, inj.Fired())
				durable.SetFault(nil)

				s = openNet(t, root)
				require.NoError(t, s.RecoverReleases(nil), "the daemon must start after a failed creation")
				assert.Contains(t, netNames(t, s), "keep")
				if err != nil && !f.Crash() {
					assert.Equal(t, []string{"keep"}, netNames(t, s))
				}
				// Whatever was left, the name can be created (again) or is
				// already a working network.
				if _, gerr := s.Get("fresh"); gerr != nil {
					_, cerr := s.Create("fresh", "10.61.0.0/24", "")
					require.NoError(t, cerr, "a half-created network holds its name")
				}
				ip, err := s.AllocateIP("fresh")
				require.NoError(t, err)
				assert.Equal(t, "10.61.0.2", ip.String())
			})
		}
	}
}

func TestFaults_CreateReclaimsDirectoryWithoutMetadata(t *testing.T) {
	root := t.TempDir()
	s := openNet(t, root)
	require.NoError(t, os.Mkdir(filepath.Join(root, "ghost"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "ghost", stateFile), []byte("{}"), 0o600))

	assert.Empty(t, netNames(t, s))
	n, err := s.Create("ghost", "10.62.0.0/24", "")
	require.NoError(t, err)
	assert.Equal(t, "10.62.0.1", n.Gateway)

	_, err = s.Create("ghost", "10.63.0.0/24", "")
	require.Error(t, err, "a complete network still refuses a second creation")
}

func TestFaults_UnreadableLeaseStateIsRebuilt(t *testing.T) {
	damage := map[string]func(t *testing.T, path string){
		"deleted": func(t *testing.T, p string) { require.NoError(t, os.Remove(p)) },
		"empty":   func(t *testing.T, p string) { require.NoError(t, os.WriteFile(p, nil, 0o600)) },
		"truncated": func(t *testing.T, p string) {
			require.NoError(t, os.WriteFile(p, []byte(`{"allocated_ips":["10.7`), 0o600))
		},
		"garbage": func(t *testing.T, p string) { require.NoError(t, os.WriteFile(p, []byte{0xff, 0x00, 0x13}, 0o600)) },
		"zeroes":  func(t *testing.T, p string) { require.NoError(t, os.WriteFile(p, make([]byte, 64), 0o600)) },
	}
	for name, fn := range damage {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			s := openNet(t, root)
			_, err := s.Create("app", "10.70.0.0/24", "")
			require.NoError(t, err)
			_, err = s.Create("other", "10.71.0.0/24", "")
			require.NoError(t, err)
			first, err := s.AllocateIP("app")
			require.NoError(t, err)
			second, err := s.AllocateIP("app")
			require.NoError(t, err)
			require.NoError(t, s.ReserveIP("app", "10.70.0.200"))

			fn(t, filepath.Join(root, "app", stateFile))

			// A restarted daemon: recovery must not fail on the damaged file,
			// and the addresses its VMs hold go back in.
			s = openNet(t, root)
			require.NoError(t, s.RecoverReleases(map[string]bool{"vm1": true}))
			s.ReclaimLeases(map[string][]string{"app": {first.String(), second.String(), "10.70.0.200"}})

			seen := map[string]bool{first.String(): true, second.String(): true, "10.70.0.200": true, "10.70.0.1": true}
			for range 20 {
				ip, err := s.AllocateIP("app")
				require.NoError(t, err)
				require.False(t, seen[ip.String()], "address %s was handed out twice", ip)
				seen[ip.String()] = true
			}
			require.ErrorIs(t, s.ReserveIP("app", first.String()), ErrIPAlreadyAllocated)

			// The other network never noticed.
			ip, err := s.AllocateIP("other")
			require.NoError(t, err)
			assert.Equal(t, "10.71.0.2", ip.String())
		})
	}
}

func TestFaults_RebuiltLeaseStateNeverHandsOutTheGateway(t *testing.T) {
	root := t.TempDir()
	s := openNet(t, root)
	n, err := s.Create("app", "10.72.0.0/29", "")
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(root, "app", stateFile)))

	for {
		ip, err := s.AllocateIP("app")
		if err != nil {
			break // subnet exhausted
		}
		require.NotEqual(t, n.Gateway, ip.String())
	}
}

func TestFaults_ReclaimLeases(t *testing.T) {
	root := t.TempDir()
	s := openNet(t, root)
	_, err := s.Create("app", "10.73.0.0/24", "")
	require.NoError(t, err)

	// Addresses that cannot be recorded are skipped, not fatal: a network that
	// is gone, an address outside the subnet, something that is no address.
	s.ReclaimLeases(map[string][]string{
		"app":     {"10.73.0.5", "10.73.0.5", "192.168.1.1", "not-an-ip", "10.73.0.0"},
		"missing": {"10.99.0.5"},
	})
	require.ErrorIs(t, s.ReserveIP("app", "10.73.0.5"), ErrIPAlreadyAllocated)
	ip, err := s.AllocateIP("app")
	require.NoError(t, err)
	assert.Equal(t, "10.73.0.2", ip.String())
	s.ReclaimLeases(nil)
}

func TestFaults_LeaseWritesFailing(t *testing.T) {
	ops := map[string]func(s *Store) error{
		"allocate": func(s *Store) error { _, err := s.AllocateIP("app"); return err },
		"reserve":  func(s *Store) error { return s.ReserveIP("app", "10.74.0.50") },
		"release":  func(s *Store) error { return s.ReleaseIP("app", "10.74.0.2") },
		"prepare":  func(s *Store) error { return s.PrepareRelease("app", "vm1", "10.74.0.2") },
	}
	for opName, op := range ops {
		for _, f := range durabletest.Failures() {
			if f.Step == durable.StepSync || f.Step == durable.StepSyncDir {
				continue // lease state is not flushed, so those steps never run
			}
			t.Run(opName+"/"+f.Name, func(t *testing.T) {
				root := t.TempDir()
				s := openNet(t, root)
				_, err := s.Create("app", "10.74.0.0/24", "")
				require.NoError(t, err)
				held, err := s.AllocateIP("app")
				require.NoError(t, err)
				require.Equal(t, "10.74.0.2", held.String())

				inj := durabletest.Inject(t, f, 0, nil)
				opErr := op(s)
				require.True(t, inj.Fired())
				require.Error(t, opErr, "a lease change that was not saved must be reported")
				durable.SetFault(nil)

				// The state on disk is the one from before: the held address
				// is still held, and the store keeps working.
				s = openNet(t, root)
				require.NoError(t, s.RecoverReleases(map[string]bool{"vm1": true}))
				require.ErrorIs(t, s.ReserveIP("app", held.String()), ErrIPAlreadyAllocated)
				next, err := s.AllocateIP("app")
				require.NoError(t, err)
				assert.Equal(t, "10.74.0.3", next.String())
			})
		}
	}
}

func TestFaults_UnreadableMetadataHidesOnlyThatNetwork(t *testing.T) {
	root := t.TempDir()
	s := openNet(t, root)
	_, err := s.Create("broken", "10.75.0.0/24", "")
	require.NoError(t, err)
	_, err = s.Create("fine", "10.76.0.0/24", "")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "broken", metaFile), []byte("{"), 0o600))

	s = openNet(t, root)
	assert.Equal(t, []string{"fine"}, netNames(t, s))
	require.NoError(t, s.RecoverReleases(nil), "one unreadable network must not keep the daemon from starting")
	_, err = s.AllocateIP("fine")
	require.NoError(t, err)
	_, err = s.AllocateIP("broken")
	require.Error(t, err)
	// It can be removed and made again.
	require.NoError(t, s.Remove("broken"))
	_, err = s.Create("broken", "10.75.0.0/24", "")
	require.NoError(t, err)
}

func TestFaults_PendingReleaseSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	s := openNet(t, root)
	_, err := s.Create("app", "10.77.0.0/24", "")
	require.NoError(t, err)
	gone, err := s.AllocateIP("app")
	require.NoError(t, err)
	alive, err := s.AllocateIP("app")
	require.NoError(t, err)
	// The daemon died between recording the intent and removing the VMs.
	require.NoError(t, s.PrepareRelease("app", "vm-gone", gone.String()))
	require.NoError(t, s.PrepareRelease("app", "vm-alive", alive.String()))

	s = openNet(t, root)
	require.NoError(t, s.RecoverReleases(map[string]bool{"vm-alive": true}))
	require.NoError(t, s.ReserveIP("app", gone.String()), "the address of the removed VM is free again")
	require.ErrorIs(t, s.ReserveIP("app", alive.String()), ErrIPAlreadyAllocated, "the address of the surviving VM is still its own")
}
