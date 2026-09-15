package vm

// Linux delegates networking to TAP and retains the guest DNS hook.
type nativeNetworkHost struct {
	guestDNS func([]byte, string) ([]byte, error)
}
