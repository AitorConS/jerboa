# Jerboa Public Beta Draft

## Short Version

Jerboa is now public as a **beta unikernel engine** for building, running, and orchestrating VM-based application images.

It already includes:

- image builds from binaries and source projects
- QEMU and Firecracker execution paths
- managed networking, DNS, volumes, compose, and replicated services
- daemon-side stats, metrics, traces, and dashboard support
- Windows support through a dedicated WSL2 distro

## Positioning

This is a serious systems project, but it is not being presented as a fully stable general-purpose runtime yet.

The right expectations are:

- usable
- architecturally coherent
- interesting to advanced users and contributors
- still maturing in platform polish and operational hardening

## Known Rough Edges

- port publishing requires managed networking
- TCP forwarding works; UDP forwarding is not complete yet
- Windows support depends on WSL2
- the overall matrix is broader than the current hardening level

## Who This Is For

Jerboa is most relevant today for:

- systems engineers
- unikernel and microVM enthusiasts
- contributors interested in build/runtime orchestration
- people who want to experiment with VM-isolated application images

## Suggested Release Notes Blurb

> Jerboa is now available as a public beta. It provides a CLI and Linux daemon for building and running unikernel-oriented VM images, with support for QEMU, Firecracker, managed networking, volumes, compose-style stacks, replicated services, and observability endpoints. Windows support is available through a dedicated WSL2 distro. This release is intended for advanced users and contributors; some runtime and platform edges are still being hardened.
