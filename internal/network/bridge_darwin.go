package network

import "fmt"

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
