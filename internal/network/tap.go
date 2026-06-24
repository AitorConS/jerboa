//go:build linux

package network

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	tunDevice = "/dev/net/tun"
	ifNameSz  = 16

	ioctlTUNSETIFF = 0x400454ca
	iffTAP         = 0x0002
	iffNOPI        = 0x1000
)

// TAPConfig holds the parameters for a TAP device.
type TAPConfig struct {
	// Name is the interface name (e.g. "jerboa-tap0"). Truncated to 15 chars.
	Name string
	// Bridge is the Linux bridge to attach to (e.g. "jerboa-br0").
	Bridge string
}

// TAP manages a TAP network interface.
type TAP struct {
	cfg TAPConfig
	fd  *os.File
}

// Create opens /dev/net/tun and creates a TAP interface with the given config.
// Requires CAP_NET_ADMIN.
func Create(cfg TAPConfig) (*TAP, error) {
	f, err := os.OpenFile(tunDevice, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", tunDevice, err)
	}
	if err := setTAP(f, cfg.Name); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("set tap %s: %w", cfg.Name, err)
	}
	return &TAP{cfg: cfg, fd: f}, nil
}

// Name returns the interface name.
func (t *TAP) Name() string { return t.cfg.Name }

// Destroy closes and removes the TAP interface.
func (t *TAP) Destroy() error {
	if err := t.fd.Close(); err != nil {
		return fmt.Errorf("destroy tap %s: %w", t.cfg.Name, err)
	}
	return nil
}

// ifreq is the Linux ifreq structure for ioctl calls.
type ifreq struct {
	name  [ifNameSz]byte
	flags uint16
	_     [22]byte // padding to match kernel struct size
}

func setTAP(f *os.File, name string) error {
	var req ifreq
	copy(req.name[:], name)
	req.flags = iffTAP | iffNOPI
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		ioctlTUNSETIFF,
		uintptr(unsafe.Pointer(&req)),
	)
	if errno != 0 {
		return fmt.Errorf("TUNSETIFF: %w", errno)
	}
	return nil
}
