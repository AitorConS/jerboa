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
func InstallGuard(string) error       { return unsupportedBridge() }
func GuardBridge(string) error        { return unsupportedBridge() }
func GuardTap(_, _, _ string) error   { return unsupportedBridge() }
func UnguardTap(_, _, _ string)       {}

// WatchGuard has nothing to watch: macOS installs no nftables guard.
func WatchGuard(context.Context, time.Duration) {}

// GuardCheckInterval matches the Linux constant.
const GuardCheckInterval = 5 * time.Second
