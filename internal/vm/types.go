//go:build linux

package vm

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"
)

// State represents a VM lifecycle state.
type State string

const (
	// StateCreated is the initial state after registration.
	StateCreated State = "created"
	// StateStarting means the QEMU process is being launched.
	StateStarting State = "starting"
	// StateRunning means the QEMU process is alive.
	StateRunning State = "running"
	// StateStopping means a kill signal has been sent.
	StateStopping State = "stopping"
	// StateStopped means the QEMU process has exited.
	StateStopped State = "stopped"
)

// validTransitions defines the allowed state machine edges.
var validTransitions = map[State][]State{
	StateCreated:  {StateStarting},
	StateStarting: {StateRunning, StateStopped},
	StateRunning:  {StateStopping, StateStopped},
	StateStopping: {StateStopped},
	StateStopped:  {},
}

// VolumeMount describes a volume attached to a VM.
type VolumeMount struct {
	// DiskPath is the absolute path to the raw disk image on the host.
	DiskPath string
	// GuestPath is the mount point inside the VM (informational; used by kernel).
	GuestPath string
	// ReadOnly marks the volume as read-only.
	ReadOnly bool
}

// HealthStatus represents the result of a health check probe.
type HealthStatus string

const (
	// HealthHealthy means the probe succeeded.
	HealthHealthy HealthStatus = "healthy"
	// HealthUnhealthy means the probe failed.
	HealthUnhealthy HealthStatus = "unhealthy"
	// HealthStarting means the VM is within the grace period and not yet probed.
	HealthStarting HealthStatus = "starting"
	// HealthUnknown means no health check is configured.
	HealthUnknown HealthStatus = "unknown"
)

// RestartPolicy determines when a VM is automatically restarted after exiting.
type RestartPolicy string

const (
	// RestartNever means the VM is never automatically restarted.
	RestartNever RestartPolicy = "never"
	// RestartOnFailure means the VM is restarted only if it exits with a non-zero
	// exit code (crash).
	RestartOnFailure RestartPolicy = "on-failure"
	// RestartAlways means the VM is always restarted regardless of exit status,
	// unless explicitly stopped.
	RestartAlways RestartPolicy = "always"
)

// RestartConfig controls automatic VM restart behavior.
type RestartConfig struct {
	// Policy is the restart policy: "never", "on-failure", or "always".
	Policy RestartPolicy
	// MaxRetries is the maximum number of restart attempts. 0 means unlimited.
	MaxRetries int
}

// HealthCheckConfig defines how to probe a VM for liveness.
type HealthCheckConfig struct {
	// Type is "tcp" or "http". For HTTP, a GET request is made to Path.
	Type string
	// Port is the guest port to probe.
	Port int
	// Path is the HTTP path (only used when Type is "http").
	Path string
	// Interval between probes. Defaults to 10s if zero.
	Interval time.Duration
	// Timeout per probe. Defaults to 3s if zero.
	Timeout time.Duration
	// Retries is the number of consecutive failures before marking unhealthy.
	// Defaults to 3 if zero.
	Retries int
}

// Config holds the parameters used to create a VM.
type Config struct {
	// ImagePath is the raw disk image containing the kernel and application.
	ImagePath string
	// Memory is the QEMU memory string (e.g. "256M").
	Memory string
	// CPUs is the number of virtual CPUs; 0 uses QEMU default.
	CPUs int
	// NetworkName is the TAP interface name to attach; empty disables networking.
	// When PortMaps are set and NetworkName is empty, SLIRP user-mode networking
	// is used automatically so no TAP device is required.
	NetworkName string
	// PortMaps is the list of host-to-guest port forwarding rules.
	// Requires SLIRP or TAP networking; mutually exclusive with "-net none".
	PortMaps []PortMap
	// Env is a list of "KEY=VALUE" environment variable pairs injected at
	// boot time via QEMU fw_cfg. The kernel must read opt/uni/env to consume them.
	Env []string
	// Name is a human-readable identifier for the VM. If empty, the UUID is used.
	Name string
	// Volumes is the list of additional disk images to attach to the VM.
	Volumes []VolumeMount
	// Attach when true, creates a pipe for streaming serial console output.
	Attach bool
	// IPAddress is the static IP address to assign to the VM. Requires TAP
	// networking (NetworkName). If empty, no static IP is configured.
	IPAddress string
	// GatewayIP is the gateway IP for the VM's network. Derived from IPAddress
	// when using TAP networking. Used to assign an IP to the bridge interface.
	GatewayIP string
	// BridgeName is the Linux bridge interface name for the VM's network.
	// When set, the daemon creates/destroys this bridge on VM start/stop.
	BridgeName string
	// SubnetMask is the CIDR mask for the VM's network (e.g. "24").
	// Used to build the guest network configuration passed via fw_cfg.
	SubnetMask string
	// HealthCheck configures liveness probing for the VM. Nil disables probing.
	HealthCheck *HealthCheckConfig
	// Restart controls automatic restart behavior when the VM exits.
	Restart RestartConfig
	// CPUShares is the cgroup v2 CPU weight (1–10000). 0 means no limit.
	CPUShares uint64
	// MemoryMax is the cgroup v2 memory hard limit in bytes. 0 means no limit.
	MemoryMax int64
	// DiskIOPS is the maximum I/O operations per second for the boot disk (QEMU throttle). 0 means no limit.
	DiskIOPS uint64
	// DiskBPS is the maximum bytes per second for the boot disk (QEMU throttle). 0 means no limit.
	DiskBPS int64
}

// process abstracts an OS process for testability.
type process interface {
	kill() error
	signal(sig os.Signal) error
}

// safeBuffer is a concurrency-safe write-only byte buffer used for VM log capture.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	if err != nil {
		return n, fmt.Errorf("safe buffer write: %w", err)
	}
	return n, nil
}

func (b *safeBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]byte, b.buf.Len())
	copy(cp, b.buf.Bytes())
	return cp
}

// RuntimeStats holds runtime resource usage for a VM.
type RuntimeStats struct {
	ID         string
	State      string
	CPUPct     float64
	MemBytes   int64
	DiskBytes  int64
	NetRxBytes int64
	NetTxBytes int64
	Timestamp  time.Time
	Source     string
}

// Stats returns the current runtime stats for the VM.
// If no stats provider is available, it returns a minimal snapshot.
func (v *VM) Stats() RuntimeStats {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.statsProvider != nil {
		return v.statsProvider()
	}
	return RuntimeStats{
		ID:        v.ID,
		State:     string(v.State),
		Timestamp: time.Now(),
		Source:    "fallback",
	}
}

// SetStatsProvider sets the function that returns live VM stats.
func (v *VM) SetStatsProvider(fn func() RuntimeStats) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.statsProvider = fn
}

// VM is a managed unikernel instance. All exported fields are read-only after
// Start; internal mutation is guarded by mu.
type VM struct {
	// ID uniquely identifies the VM.
	ID string
	// Cfg is the configuration the VM was created with.
	Cfg Config
	// State is the current lifecycle state.
	State State
	// CreatedAt is when the VM was registered.
	CreatedAt time.Time
	// StartedAt is when the QEMU process started (nil until then).
	StartedAt *time.Time
	// StoppedAt is when the QEMU process exited (nil until then).
	StoppedAt *time.Time
	// DaemonRecovered is true when this VM was recovered from a previous
	// daemon run. The original QEMU process is gone; the VM is in StateStopped.
	DaemonRecovered bool
	// HealthStatus is the latest probe result. "unknown" if no health check.
	HealthStatus HealthStatus
	// RestartCount is the number of times this VM has been restarted.
	RestartCount int

	mu            sync.RWMutex
	proc          process
	done          chan struct{}
	logBuf        safeBuffer
	logPipeReader io.Reader
	logPipeWriter *io.PipeWriter
	explicitStop  bool
	statsProvider func() RuntimeStats
	cgroupMgr     *CgroupManager
	qmpAddr       string // TCP "host:port" of QMP socket; set at start, cleared when stopped
}

// Done returns a channel that is closed when the VM reaches StateStopped.
func (v *VM) Done() <-chan struct{} {
	return v.done
}

// GetState returns the current state under a read lock.
func (v *VM) GetState() State {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.State
}

// Logs returns a snapshot of captured QEMU serial console output.
func (v *VM) Logs() []byte {
	return v.logBuf.Bytes()
}

// AttachReader returns a reader that streams QEMU serial console output.
// Returns nil if no attach pipe was created (VM not started in attach mode).
func (v *VM) AttachReader() io.Reader {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.logPipeReader
}

// GetTimes returns the start and stop timestamps under a read lock.
func (v *VM) GetTimes() (startedAt, stoppedAt *time.Time) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.StartedAt, v.StoppedAt
}

// GetHealthStatus returns the current health status under a read lock.
func (v *VM) GetHealthStatus() HealthStatus {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.HealthStatus
}

// SetHealthStatus sets the health status under a write lock.
func (v *VM) SetHealthStatus(s HealthStatus) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.HealthStatus = s
}

// GetRestartCount returns the number of times this VM has been restarted.
func (v *VM) GetRestartCount() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.RestartCount
}

// SetExplicitStop marks the VM as explicitly stopped by the user.
// When true, the monitor goroutine will not attempt to restart the VM.
func (v *VM) SetExplicitStop() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.explicitStop = true
}

// IsExplicitStop returns whether the VM was explicitly stopped.
func (v *VM) IsExplicitStop() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.explicitStop
}

// transition atomically moves v to state to, validating the edge and logging.
func (v *VM) transition(to State) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !slices.Contains(validTransitions[v.State], to) {
		return fmt.Errorf("invalid transition %s → %s", v.State, to)
	}
	from := v.State
	v.State = to
	slog.Info("vm state transition", "vm_id", v.ID, "from", from, "to", to)
	if to == StateStopped {
		close(v.done)
	}
	return nil
}
