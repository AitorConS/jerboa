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
