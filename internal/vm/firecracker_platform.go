package vm

// platformInitFC is a no-op on every platform. Firecracker only runs where the
// daemon runs — Linux with KVM. On Windows the daemon lives inside WSL2 (see
// internal/wslboot), so the client never drives Firecracker directly and needs
// no platform-specific hooks.
func platformInitFC(_ *FirecrackerManager) {}
