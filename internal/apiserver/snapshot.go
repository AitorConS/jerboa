//go:build linux || (darwin && arm64)

package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/network"
	"github.com/AitorConS/jerboa/internal/snapshot"
	"github.com/AitorConS/jerboa/internal/vm"
)

// snapshotManager is implemented only by backends with snapshot support
// (currently native macOS Firecracker/HVF); vm.Manager is not widened.
type snapshotManager interface {
	SnapshotCreate(ctx context.Context, id, name string) (snapshot.Info, error)
	SnapshotRestore(ctx context.Context, id, name string) error
	SnapshotList() ([]snapshot.Entry, error)
	SnapshotInspect(name string) (snapshot.Info, error)
	SnapshotRemove(name string) error
}

func (s *Server) snapshots() (snapshotManager, *api.RPCError) {
	sm, ok := s.mgr.(snapshotManager)
	if !ok {
		return nil, &api.RPCError{Code: -32000, Message: vm.ErrSnapshotUnsupported.Error() + ": snapshots require the native macOS Firecracker/HVF backend"}
	}
	return sm, nil
}

func snapshotParams(params json.RawMessage, needID bool) (api.SnapshotParams, *api.RPCError) {
	var p api.SnapshotParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, &api.RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	if p.Name == "" || (needID && p.ID == "") {
		return p, &api.RPCError{Code: -32602, Message: "invalid params: snapshot name and VM id are required"}
	}
	return p, nil
}

func (s *Server) handleSnapshotCreate(ctx context.Context, params json.RawMessage) (any, *api.RPCError) {
	sm, rerr := s.snapshots()
	if rerr != nil {
		return nil, rerr
	}
	p, rerr := snapshotParams(params, true)
	if rerr != nil {
		return nil, rerr
	}
	info, err := sm.SnapshotCreate(ctx, p.ID, p.Name)
	if err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	return toSnapshotInfo(info), nil
}

// handleSnapshotRestore serializes with VM.Run/VM.Start through resourceMu so
// the network registry checks below and the in-place restore cannot interleave
// with another VM claiming the same IP, name or alias.
func (s *Server) handleSnapshotRestore(ctx context.Context, params json.RawMessage) (any, *api.RPCError) {
	sm, rerr := s.snapshots()
	if rerr != nil {
		return nil, rerr
	}
	p, rerr := snapshotParams(params, true)
	if rerr != nil {
		return nil, rerr
	}
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	v, err := s.mgr.Get(p.ID)
	if err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	if st := v.GetState(); st != vm.StateStopped {
		return nil, &api.RPCError{Code: -32000, Message: fmt.Sprintf("snapshot restore %s: vm is %s; restore is in place and requires a stopped VM", v.ID, st)}
	}
	if err := s.checkRestoreNetwork(v); err != nil {
		return nil, &api.RPCError{Code: -32000, Message: fmt.Sprintf("snapshot restore %s: %v", v.ID, err)}
	}
	if err := sm.SnapshotRestore(ctx, v.ID, p.Name); err != nil {
		s.recordVMError()
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	if s.collectors != nil {
		s.collectors.VMStartsTotal.Inc()
	}
	return toInfo(v), nil
}

// checkRestoreNetwork requires the registered network to keep its subnet and
// gateway, the exact IP to remain this VM's, and the VM name and aliases to be
// unclaimed by other live VMs on that network. Restore never changes network.
func (s *Server) checkRestoreNetwork(v *vm.VM) error {
	c := v.Cfg
	if c.NetworkName == "" {
		return fmt.Errorf("%w: v1 snapshots require a VM attached to a named Jerboa network", vm.ErrSnapshotUnsupported)
	}
	n, err := s.netStore.Get(c.NetworkName)
	if err != nil {
		return fmt.Errorf("network %q is not registered: %w", c.NetworkName, err)
	}
	mask := c.SubnetMask
	if mask == "" {
		mask = "24"
	}
	_, want, err := net.ParseCIDR(c.IPAddress + "/" + mask)
	if err != nil {
		return fmt.Errorf("invalid VM address: %w", err)
	}
	_, have, err := net.ParseCIDR(n.Subnet)
	if err != nil || have.String() != want.String() || n.Gateway != c.GatewayIP {
		return fmt.Errorf("network %q changed subnet or gateway (now %s via %s; VM uses %s via %s)", c.NetworkName, n.Subnet, n.Gateway, want, c.GatewayIP)
	}
	names := slices.Clone(c.NetworkAliases)
	if c.Name != "" {
		names = append(names, c.Name)
	}
	for _, o := range s.mgr.List() {
		if o.ID == v.ID || o.Cfg.NetworkName != c.NetworkName {
			continue
		}
		if o.Cfg.IPAddress == c.IPAddress {
			return fmt.Errorf("IP %s is assigned to VM %s", c.IPAddress, o.ID)
		}
		switch o.GetState() {
		case vm.StateStarting, vm.StateRunning, vm.StateStopping, vm.StateRestoring:
		default:
			continue
		}
		for _, name := range names {
			if o.Cfg.Name == name || slices.Contains(o.Cfg.NetworkAliases, name) {
				return fmt.Errorf("name or alias %q is held by live VM %s on network %q", name, o.ID, c.NetworkName)
			}
		}
	}
	// The reservation belongs to the VM, not to the restore: a stopped VM
	// normally still holds it. Re-reserve if it was released.
	if err := s.netStore.ReserveIP(c.NetworkName, c.IPAddress); err != nil && !errors.Is(err, network.ErrIPAlreadyAllocated) {
		return fmt.Errorf("reserve ip: %w", err)
	}
	return nil
}

func (s *Server) handleSnapshotList() (any, *api.RPCError) {
	sm, rerr := s.snapshots()
	if rerr != nil {
		return nil, rerr
	}
	entries, err := sm.SnapshotList()
	if err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	out := make([]api.SnapshotInfo, 0, len(entries))
	for _, e := range entries {
		info := toSnapshotInfo(e.Info)
		if e.Err != nil {
			info.Error = e.Err.Error()
		}
		out = append(out, info)
	}
	return out, nil
}

func (s *Server) handleSnapshotInspect(params json.RawMessage) (any, *api.RPCError) {
	sm, rerr := s.snapshots()
	if rerr != nil {
		return nil, rerr
	}
	p, rerr := snapshotParams(params, false)
	if rerr != nil {
		return nil, rerr
	}
	info, err := sm.SnapshotInspect(p.Name)
	if err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	return toSnapshotInfo(info), nil
}

func (s *Server) handleSnapshotRemove(params json.RawMessage) (any, *api.RPCError) {
	sm, rerr := s.snapshots()
	if rerr != nil {
		return nil, rerr
	}
	p, rerr := snapshotParams(params, false)
	if rerr != nil {
		return nil, rerr
	}
	if err := sm.SnapshotRemove(p.Name); err != nil {
		return nil, &api.RPCError{Code: -32000, Message: err.Error()}
	}
	return map[string]string{"status": "ok"}, nil
}

func toSnapshotInfo(i snapshot.Info) api.SnapshotInfo {
	out := api.SnapshotInfo{
		Name:           i.Name,
		VMID:           i.VMID,
		VMName:         i.VMName,
		Backend:        i.Backend,
		Image:          i.Image,
		ImageDigest:    i.ImageDigest,
		Memory:         i.Memory,
		CPUs:           i.CPUs,
		Network:        i.Network.Name,
		Subnet:         i.Network.Subnet,
		Gateway:        i.Network.Gateway,
		IPAddress:      i.Network.IP,
		MAC:            i.Network.MAC,
		Aliases:        i.Network.Aliases,
		SizeBytes:      i.SizeBytes,
		ManifestSHA256: i.ManifestSHA256,
		Components:     len(i.Components),
	}
	if !i.CreatedAt.IsZero() {
		out.CreatedAt = i.CreatedAt.Format(time.RFC3339)
	}
	for _, p := range i.Ports {
		out.Ports = append(out.Ports, api.PortMapSpec{HostPort: p.HostPort, GuestPort: p.GuestPort, Protocol: p.Protocol, BindAddr: p.BindAddr})
	}
	return out
}
