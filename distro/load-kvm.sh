#!/bin/sh
# Load the KVM module so firecracker can open /dev/kvm. The Microsoft WSL2 kernel
# ships KVM as a module (CONFIG_KVM=m) and does not auto-load it, so the jerboa
# distro loads it from its wsl.conf [boot] command. Requires nested virtualization
# on the host: set nestedVirtualization=true in %USERPROFILE%\.wslconfig.
modprobe kvm_intel 2>/dev/null || modprobe kvm_amd 2>/dev/null
exit 0
