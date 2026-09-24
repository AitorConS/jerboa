# Kernel publication checklist

- [x] Trace candidate inputs, kernel builds, rootfs staging and promotion guards.
- [x] Select source-built or explicitly reused kernel tools without an implicit stable fallback.
- [x] Validate the exact staged QEMU and Firecracker kernels and retain hashes and provenance.
- [x] Package the selected tools in the rootfs and signed product candidate; promote without rebuilding.
- [x] Reject missing, changed, mismatched or downgraded kernel artifacts and immutable version collisions.
- [x] Add release/workflow regression tests and update the release instructions.
- [x] Run protocol tests, workflow lint and native boot checks; record limits.

## Evidence

- 22 release protocol/workflow tests passed, including source-built selection,
  signed stable reuse, altered/missing files, foreign provenance, rootfs mismatch,
  new-kernel promotion ordering, downgrade rejection and version collisions.
- `actionlint v1.7.7`, Python compilation, shell syntax and diff checks passed.
- The new staging and boot validator ran against the existing isolated Linux
  test kernel on the native x86_64/KVM server. QEMU and Firecracker completed
  successfully; evidence is in `release-kernel-check/kernel-candidate/boot-validation.json`
  under the existing isolated test tree. This exercises the actual staging/boot
  commands, not a dispatched product release.
- No release candidate was dispatched, no stable objects were written and no
  installed runtime was replaced. Full candidate packaging/signing still runs
  in Actions, followed by native Mac app validation and owner-approved promotion.
