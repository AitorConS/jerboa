//go:build linux || (darwin && arm64)

package network

import "fmt"

// PrepareRelease records the removal intent before the VM registry is changed.
// If the daemon exits between deletion and release, recovery can finish the
// release without reclaiming an address still held by a registered VM.
func (s *Store) PrepareRelease(name, owner, ip string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.readState(name)
	if err != nil {
		return err
	}
	found := false
	for _, allocated := range st.AllocatedIPs {
		if allocated == ip {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	if st.PendingReleases == nil {
		st.PendingReleases = make(map[string]string)
	}
	st.PendingReleases[owner] = ip
	dir, err := s.networkDir(name)
	if err != nil {
		return err
	}
	return writeNetworkState(dir, st)
}

// RecoverReleases runs before accepting API requests. Unknown legacy leases and
// explicitly allocated addresses have no removal intent and remain reserved.
func (s *Store) RecoverReleases(live map[string]bool) error {
	networks, err := s.List()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range networks {
		st, err := s.readState(n.Name)
		if err != nil {
			return err
		}
		if len(st.PendingReleases) == 0 {
			continue
		}
		for owner, ip := range st.PendingReleases {
			if !live[owner] {
				for i, allocated := range st.AllocatedIPs {
					if allocated == ip {
						st.AllocatedIPs = append(st.AllocatedIPs[:i], st.AllocatedIPs[i+1:]...)
						st.NextIndex = 2
						break
					}
				}
			}
			delete(st.PendingReleases, owner)
		}
		dir, err := s.networkDir(n.Name)
		if err != nil {
			return err
		}
		if err := writeNetworkState(dir, st); err != nil {
			return fmt.Errorf("recover network %s: %w", n.Name, err)
		}
	}
	return nil
}
