package vm

import (
	"context"
	"log/slog"
	"os"

	"github.com/AitorConS/jerboa/internal/network"
)

// restoreAdopted completes the re-adoption of the VMs that outlived the
// previous daemon. Their VMM kept running, but what that daemon held for
// them is gone with its process: the listeners and kernel translations of
// their published ports (a new daemon clears the translations it finds),
// the tap bindings the guard reinstalls from, the health checks and the
// statistics. Without this a re-adopted VM is listed as running while its
// published ports refuse connections.
//
// It also registers what the adopt monitor undoes when such a VMM exits,
// which the monitor of a VM this daemon started would have done: the ports,
// the tap, and files (the VMM's sockets, configuration, log and boot disk).
func restoreAdopted(ctx context.Context, vms []*VM, health *HealthChecker, files func(id string) []string) {
	for _, v := range vms {
		v.mu.RLock()
		adopted := v.State == StateRunning && v.proc != nil
		pid := v.pid
		v.mu.RUnlock()
		if !adopted {
			continue
		}
		cfg := v.Cfg
		if cfg.NetworkName != "" && cfg.tapDevice() == cfg.NetworkName {
			// As reconcileNetwork: a tap name that was not recorded is the
			// one derived from the VM.
			cfg.TapName = tapDeviceName(v.ID)
		}
		if cfg.NetworkName != "" && cfg.IPAddress != "" {
			if err := network.GuardTap(cfg.tapDevice(), guestMACFromIP(cfg.IPAddress), cfg.IPAddress); err != nil {
				slog.Warn("restore: tap of a re-adopted vm is not bound to its address", "vm_id", v.ID, "err", err)
			}
		}
		var fwd *network.Forwarder
		if len(cfg.PortMaps) > 0 {
			f, err := network.PublishPorts(cfg.IPAddress, cfg.bridge(), toNetworkPortForwards(cfg.PortMaps))
			if err != nil {
				// The guest keeps running; refusing to start the daemon over
				// it would take every other VM down with it.
				slog.Error("restore: published ports of a re-adopted vm could not be restored; stop and run it again", "vm_id", v.ID, "err", err)
			} else {
				fwd = f
			}
		}
		var leftovers []string
		if files != nil {
			leftovers = files(v.ID)
		}
		v.mu.Lock()
		v.portFwd = fwd
		v.hostCleanup = func() {
			v.mu.Lock()
			f := v.portFwd
			v.portFwd = nil
			v.mu.Unlock()
			if f != nil {
				f.Close()
			}
			if cfg.NetworkName != "" {
				teardownTAPNetwork(cfg, v.ID, "adopt monitor")
			}
			for _, p := range leftovers {
				_ = os.Remove(p)
			}
		}
		v.mu.Unlock()
		if newStatsCollector != nil {
			v.SetStatsProvider(newStatsCollector(pid, v).Collect)
		}
		if health != nil {
			health.Start(ctx, v)
		}
		slog.Info("restore: re-adopted vm has its ports and network back", "vm_id", v.ID, "ports", len(cfg.PortMaps))
	}
}
