#!/bin/sh
# The runner only accepts a job-started hook whose name ends in .sh, .ps1 or
# .js, and runs a .sh with bash. This runs the guard installed next to it
# under the same name without the extension (see kvm-runner-guard.py).
exec python3 "${0%.sh}"
