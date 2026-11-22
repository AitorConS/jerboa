package vm

import "github.com/AitorConS/jerboa/internal/hypervisor"

// vmmName names the microVM backend in errors and logs: "spark" on macOS,
// where it is Spark, and "firecracker" on Linux.
const vmmName = hypervisor.MicroVM
