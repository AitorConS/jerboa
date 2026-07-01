//go:build linux

package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/AitorConS/jerboa/internal/image"
	"github.com/AitorConS/jerboa/internal/vm"
)

// VMStateUpdater periodically polls a vm.Manager and an image.Store and
// updates Prometheus gauges with the current VM state counts and image count.
type VMStateUpdater struct {
	collectors *Collectors
	mgr        vm.Manager
	imgStore   *image.Store
	interval   time.Duration
}

// NewVMStateUpdater creates an updater that polls the manager (and, if
// non-nil, the image store) at the given interval.
func NewVMStateUpdater(collectors *Collectors, mgr vm.Manager, imgStore *image.Store, interval time.Duration) *VMStateUpdater {
	return &VMStateUpdater{
		collectors: collectors,
		mgr:        mgr,
		imgStore:   imgStore,
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

	if u.imgStore != nil {
		manifests, err := u.imgStore.List()
		if err != nil {
			slog.Warn("metrics: list images", "err", err)
		} else {
			u.collectors.ImagesTotal.Set(float64(len(manifests)))
		}
	}
}
