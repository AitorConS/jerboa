#!/usr/bin/env bash
# Runs the end-to-end suite (tests/e2e) against the binaries in dist/ and the
# kernel in kernel/output/. Arguments are passed to the test binary, e.g.
#
#   scripts/run-e2e.sh -test.run TestCompose -test.v
#
# See run-kvm-tests.sh for the privileges the suite needs on Linux and how
# it gets them.
set -euo pipefail
exec "$(dirname "${BASH_SOURCE[0]}")/run-kvm-tests.sh" e2e ./tests/e2e "$@"
