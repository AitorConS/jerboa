package scheduler

import (
	"fmt"
	"strings"

	"github.com/AitorConS/unikernel-engine/internal/vm"
)

// VMSource provides access to VMs for DNS-like name resolution.
type VMSource interface {
	List() []*vm.VM
}

// Record describes a resolvable VM name inside a network scope.
type Record struct {
	Name    string `json:"name"`
	Network string `json:"network"`
	IP      string `json:"ip"`
	VMID    string `json:"vm_id"`
}

// Resolver resolves VM names to IP addresses using in-memory VM state.
type Resolver struct {
	vms VMSource
}

// NewResolver creates a Resolver backed by a VM source.
func NewResolver(vms VMSource) *Resolver {
	return &Resolver{vms: vms}
}

// Resolve returns a DNS record for name and optional network.
// Accepted names are VM ID and VM name. If name is of the form host.network
// and network is empty, the network is inferred from the suffix.
func (r *Resolver) Resolve(name, network string) (Record, error) {
	if strings.TrimSpace(name) == "" {
		return Record{}, fmt.Errorf("name must not be empty")
	}
	host, scope := splitScopedName(name)
	if network == "" {
		network = scope
	}

	for _, rec := range r.records(network) {
		if rec.Name == host || rec.VMID == host {
			return rec, nil
		}
	}
	if network != "" {
		return Record{}, fmt.Errorf("record %q not found in network %q", host, network)
	}
	return Record{}, fmt.Errorf("record %q not found", host)
}

// List returns all resolvable records, optionally filtered by network.
func (r *Resolver) List(network string) []Record {
	return r.records(network)
}

func (r *Resolver) records(network string) []Record {
	vms := r.vms.List()
	out := make([]Record, 0, len(vms))
	for _, v := range vms {
		rec, ok := vmToRecord(v)
		if !ok {
			continue
		}
		if network != "" && rec.Network != network {
			continue
		}
		out = append(out, rec)
	}
	return out
}

func vmToRecord(v *vm.VM) (Record, bool) {
	if v == nil {
		return Record{}, false
	}
	if v.GetState() != vm.StateRunning {
		return Record{}, false
	}
	if v.Cfg.NetworkName == "" || v.Cfg.IPAddress == "" {
		return Record{}, false
	}
	name := v.Cfg.Name
	if name == "" {
		name = v.ID
	}
	return Record{
		Name:    name,
		Network: v.Cfg.NetworkName,
		IP:      v.Cfg.IPAddress,
		VMID:    v.ID,
	}, true
}

func splitScopedName(name string) (host, network string) {
	parts := strings.Split(name, ".")
	if len(parts) < 2 {
		return name, ""
	}
	host = strings.Join(parts[:len(parts)-1], ".")
	network = parts[len(parts)-1]
	return host, network
}
