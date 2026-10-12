// Package netconst holds small, dependency-free networking constants shared
// across packages that must not import each other (e.g. the image builder, the
// bridge/network layer, and the guest DNS server). Keeping them here avoids
// import cycles and platform build-tag coupling.
package netconst

// DNSAnycastIP is a reserved address the daemon assigns to the loopback
// interface and serves guest DNS on. Guests reach it through their default
// gateway (the bridge), so a single fixed address works for every network
// regardless of subnet. It is baked into each image's /etc/resolv.conf at build
// time, so it must stay stable. It lives in a range unlikely to collide with
// user-defined subnets.
const DNSAnycastIP = "10.53.0.53"

// DNSPort is the UDP port the guest DNS server listens on.
const DNSPort = 53

// NativeMTU is the MTU of the macOS userspace networks (gvproxy switch and the
// gVisor gateway stack) and of their guests. Every frame between two VMs
// crosses the guest, the VMM, its broker and the Go switch, each at a cost
// per frame: at 1500 bytes, VM-to-VM TCP saturated the sending vCPU and two
// host cores in the switch. Nothing leaves these networks as raw frames (the
// gateway terminates TCP and UDP and proxies them through host sockets), so
// a jumbo MTU stays internal.
const NativeMTU = 9000

// BridgeMTU is the MTU of Linux network bridges, their TAPs and guests.
// Frames between VMs stay on the bridge, and host traffic to a VM goes
// through userspace proxies (published ports) or local services (DNS), so
// the host's own interfaces keep their MTU.
const BridgeMTU = 9000
