# Jerboa

Jerboa runs applications as unikernel microVMs: a Go CLI (`cmd/jerboa`), a
daemon (`cmd/jerboad`), a Nanos-derived kernel (`kernel/`), and the release
tooling that signs and publishes what users install. This file tells Claude
how to work in the repository and, above all, how to review a change to it.

## Commands

```bash
make build              # CLI and daemon
make test               # unit tests, -race, every package outside kernel/
make lint               # golangci-lint (.golangci.yml)
make tidy-check         # go.mod and go.sum are tidy
make test-integration   # needs Linux and /dev/kvm
make e2e                # needs Linux and /dev/kvm
make test-kernel        # kernel host tests
python3 -m unittest discover -s scripts/release -p 'test_*.py'   # release protocol
python3 scripts/release/changelog.py check
```

A single fuzz target: `go test ./internal/<pkg> -run '^$' -fuzz '^FuzzName$' -fuzztime 30s`.

## Layout

- `cmd/`: entry points. Logic lives in `internal/`, not here.
- `internal/durable`: the one way to write a state file. `internal/durable/durabletest` injects faults into it.
- `internal/network`, `internal/dnsserver`: host networking, nftables, NAT, DNS.
- `internal/package`, `internal/ociblob`, `internal/ociregistry`, `internal/registry`, `internal/image`: everything that reads bytes someone else produced.
- `internal/signing`, `internal/release`, `scripts/release/`, `.github/workflows/release.yml`, `promote.yml`: the release protocol.
- `internal/hypervisor`, `internal/vm`, `internal/snapshot`, `internal/volume`: VM lifecycle and its state on disk.
- `tests/`: integration, e2e, upgrade and registry suites. `docs/`: the public site.

## Code review

Review as the person who will be woken up when the change fails in
production. The question is not whether the code works on the path the author
tried. It is whether the change is correct, whether it is the right size, and
whether it can be trusted on the paths nobody tried.

Report a finding only when you can name the input or the sequence of events
that makes it go wrong and what the user then sees. Read the surrounding code
before reporting: a check that looks missing is often made by the caller. Do
not report style that `gofmt` and `golangci-lint` already decide, and do not
ask for changes outside the diff unless the diff breaks them.

Rank what you find in this order.

### 1. Correctness

- Every error is handled or returned with context (`fmt.Errorf("...: %w", err)`). A discarded error needs a reason in a comment.
- Concurrency: shared state has one owner or one lock; a goroutine has a way to stop; a context is passed down and respected; nothing sleeps to wait for another goroutine.
- Lifecycle operations (start, stop, restore, remove) are safe to run against each other and to repeat. Two of them racing must leave exactly one winner and a clear error for the other.
- Behaviour is the same on Linux, macOS and Windows/WSL unless the change says otherwise. Paths, signals, file modes and process trees differ; a change that only considers one platform is incomplete.
- Limits: sizes, counts and durations that come from outside are bounded before they are used to allocate, loop or wait.

### 2. Security

Treat as hostile everything the program did not produce itself: archives,
image manifests and layers, registry responses, compose files, package
recipes, CLI arguments, environment, and anything read back from disk that
another process could have written.

- Paths built from outside input stay inside their root. Archive entries are checked for `..`, absolute names, symlinks and hard links before anything is written, and extraction that fails leaves nothing behind.
- Digests and signatures are verified before the bytes are used, not after, and the comparison is of the whole value. Nothing is trusted because of its name, its tag or where it was downloaded from.
- No shell is built from strings. Commands are `exec.Command` with separate arguments; nftables and similar scripts are generated from validated values only.
- Secrets (tokens, keys, registry credentials) never reach logs, errors, process arguments, or files with a mode wider than `0600`. Check new log lines and new error messages for them.
- The daemon socket and the files under the data directory keep their permissions. A change that widens who can reach either needs to say why.
- Network exposure does not grow silently: a new listener, a wider bind address or a relaxed firewall rule is a finding unless the pull request states it as the purpose.
- New dependencies are a decision, not a detail. Ask what they replace, who maintains them, and whether the standard library would do.
- Workflows: third-party actions are pinned to a commit SHA; `permissions` are the minimum the job needs; nothing from a pull request (titles, branch names, file contents) is interpolated into a `run` script; secrets and protected environments are not reachable from code a fork controls. `scripts/release/test_workflows.py` holds these rules, and a change that weakens one of its assertions is a finding.

### 3. State and failure

Jerboa must survive a crash, a full disk and a killed daemon at any instant.

- State files are written with `internal/durable` (`WriteFile`), never with a hand-made rename. `os.WriteFile` is for files nobody reads back after a crash.
- After any interruption the state on disk is the old one or the new one, never a mix, and the daemon recovers on start without the user repairing anything. Temporary and staging files are cleaned up on recovery.
- A format that is written to disk is read back by the next version and by the previous one: the upgrade and downgrade path is part of the change (`tests/upgrade`).
- Cleanup runs on every exit path, including the error ones. A resource acquired before a failure (process, tap device, lock, mount, temp directory) is released.

### 4. Design: clean and able to grow

- The change solves the problem where it belongs. A special case added at the call site, a flag that only one caller sets, or a copy of existing logic is a sign the fix is one level too high or too low.
- No new abstraction without two real users, and no second way to do something the codebase already does one way. Look for the existing helper before accepting a new one.
- Cost grows with the work asked for, not with what is installed: watch for scans of every VM, image or layer on a hot path, unbounded maps and caches, reading whole files into memory, and work repeated per item that could be done once.
- A cache has an invalidation rule that is stated and tested, and a bound.
- Public behaviour (CLI flags, output formats, the API, files on disk, the registry protocol) changes only on purpose, compatibly where it can, and with the docs.
- The diff contains one change. Unrelated refactors, renames and reformatting belong in their own pull request.

### 5. Validation

A change is not done because it compiles. For each behaviour the pull request
adds or fixes, find the test that would fail without it.

- A bug fix comes with a test that reproduces the bug.
- Code that writes state, parses outside input or handles a lifecycle operation comes with failure-mode tests: the interrupted write (`durabletest`), the truncated or malformed input, the operation that races another. Parsers of outside input have a fuzz target, and an input a fuzzer found is kept as a seed.
- Tests assert what the user would observe, not how the code is arranged. A test that cannot fail, that asserts only "no error", or that sleeps to pass is a finding.
- Tests do not depend on the network, on wall-clock timing, on the order of a map, or on the developer's own `~/.jerboa`.
- A weakened or deleted assertion, a skipped test, a longer timeout or a retry added to make CI green needs a stated reason. Without one it is the most important finding in the review.
- The pull request says what was verified and what was not. Code that only runs under KVM, on Windows or on macOS and was not run there is called out.

### 6. What ships with the change

- `CHANGELOG.md` has an `Unreleased` entry when a user would notice the change.
- `docs/` matches the new behaviour for CLI flags, daemon, networking, build and Windows/WSL workflows.
- Log lines and errors tell an operator what happened and what to do, in the structured form the rest of the code uses.

## Areas that need a second look

A change here is reviewed against every section above, however small it is:

- the release protocol: `scripts/release/`, `internal/signing`, `internal/release`, `release.yml`, `promote.yml`, and anything that decides what gets signed or which runner may build it;
- host networking and firewall rules: `internal/network`, `internal/dnsserver`;
- archive extraction and image handling: `internal/package`, `internal/ociblob`, `internal/ociregistry`, `internal/image`;
- anything that runs as root or installs, upgrades or removes Jerboa from a machine;
- `kernel/`.

## Conventions

- Commit subjects follow `type(scope): summary`. Pull request bodies state what changed, why, what was verified and what remains unverified.
- Comments say why, not what, in plain sentences.
- Per-machine Claude settings (`.claude/`) are not committed.
