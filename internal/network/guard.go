//go:build linux || darwin

package network

// AllowUnguardedEnv is the daemon environment variable that lets guests join
// networks on Linux while the guest network guard is not loaded.
const AllowUnguardedEnv = "JERBOA_ALLOW_UNGUARDED_NETWORK"

// GuardOptions configures the guest network guard (Linux only).
type GuardOptions struct {
	// AllowUnguarded lets guests join networks while the guard is not
	// loaded ($JERBOA_ALLOW_UNGUARDED_NETWORK=1). Without it a VM on a
	// network does not start until the guard is loaded.
	AllowUnguarded bool
}

// GuardState is what `jerboa doctor` reports about the guard.
type GuardState struct {
	// Configured is set when the daemon installs a guard on this host
	// (Linux, WSL included).
	Configured bool
	// Err is why the guard is not loaded; nil when it is.
	Err error
	// Enforced is set when VMs on networks are refused while the guard is
	// not loaded.
	Enforced bool
	// AllowUnguarded mirrors GuardOptions.AllowUnguarded.
	AllowUnguarded bool
	// NoBridgeFamily is set on WSL, whose kernel has no nftables bridge
	// family: guests are not bound to their MAC and IPv4 address there.
	NoBridgeFamily bool
}
