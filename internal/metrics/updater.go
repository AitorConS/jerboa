package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/AitorConS/unikernel-engine/internal/vm"
)

// VMStateUpdater periodically polls a vm.Manager and updates Prometheus gauges
// with the current count of VMs in each state.
type VMStateUpdater struct {
	collectors *Collectors
	mgr        vm.Manager
	interval   time.Duration
}

// NewVMStateUpdater creates an updater that polls the manager at the given interval.
func NewVMStateUpdater(collectors *Collectors, mgr vm.Manager, interval time.Duration) *VMStateUpdater {
	return &VMStateUpdater{
		collectors: collectors,
		mgr:        mgr,
		interval:   interval,
	}
}

// Run starts the polling loop. It blocks until ctx is canceled.
func (u *VMStateUpdater) Run(ctx context.Context) {
	u.update()
	ticker := time.NewTicker(u.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			u.update()
		}
	}
}

func (u *VMStateUpdater) update() {
	vms := u.mgr.List()
	var created, starting, running, stopping, stopped int
	for _, v := range vms {
		switch v.State {
		case vm.StateCreated:
			created++
		case vm.StateStarting:
			starting++
		case vm.StateRunning:
			running++
		case vm.StateStopping:
			stopping++
		case vm.StateStopped:
			stopped++
		default:
			slog.Warn("unknown vm state", "id", v.ID, "state", v.State)
		}
	}
	u.collectors.VMCreated.Set(float64(created))
	u.collectors.VMStarting.Set(float64(starting))
	u.collectors.VMRunning.Set(float64(running))
	u.collectors.VMStopping.Set(float64(stopping))
	u.collectors.VMStopped.Set(float64(stopped))
	u.collectors.ImagesTotal.Set(float64(len(vms)))
}
