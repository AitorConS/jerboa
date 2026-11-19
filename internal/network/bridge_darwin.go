package network

import (
	"context"
	"fmt"
	"time"
)

type BridgeConfig struct{ Name, CIDR string }

func unsupportedBridge() error {
	return fmt.Errorf("kernel TAP operations are unavailable on macOS; managed networks use the native userspace switch")
}
func CreateBridge(BridgeConfig) error { return unsupportedBridge() }
func EnsureBridge(BridgeConfig) error { return unsupportedBridge() }
func DestroyBridge(string) error      { return nil } // userspace switches close with their last VM
func EnsureDNSAddress(string) error   { return unsupportedBridge() }
func CreateTAPDevice(string) error    { return unsupportedBridge() }
func DeleteTAPDevice(string) error    { return unsupportedBridge() }
func AttachTAP(string, string) error  { return unsupportedBridge() }
func DetachTAP(string) error          { return unsupportedBridge() }
func GuardBridge(string) error        { return unsupportedBridge() }
func GuardTap(_, _, _ string) error   { return unsupportedBridge() }
func UnguardTap(_, _, _ string)       {}

// InstallGuard has nothing to install: macOS uses no nftables guard.
func InstallGuard(string, GuardOptions) error { return unsupportedBridge() }

// RequireGuard has nothing to require: macOS guests use the native userspace
// switch, which applies its own egress policy.
func RequireGuard() error { return nil }

// CheckGuard reports no guard: macOS installs none.
func CheckGuard() GuardState { return GuardState{} }

// WatchGuard has nothing to watch: macOS installs no nftables guard.
func WatchGuard(context.Context, time.Duration) {}

// GuardCheckInterval matches the Linux constant.
const GuardCheckInterval = 5 * time.Second
