#!/usr/bin/env python3
"""Qualify clean PostgreSQL shutdown on `jerboa stop` over Firecracker/HVF.

Nanos turns the VM power button into a process-directed SIGTERM with a 30 s
guest timeout, and the host forces the VMM after 35 s. The threaded PostgreSQL
fork keeps signal handlers per thread, so without shutdown.patch that SIGTERM
runs in an arbitrary thread and the next boot needs WAL crash recovery.

The test builds the fixture from an isolated copy with shutdown.patch applied
after nanos.patch and openssl.patch, then checks:
  1. Idle and loaded (committing client) `jerboa stop`: the server log shows a
     fast shutdown ending in "database system is shut down" before the VM
     stops, well inside the guest timeout.
  2. The next boot logs "database system was shut down at" and runs no crash
     recovery; every acknowledged commit, checkpoint-era row, checksum and
     amcheck survives.
  3. `jerboa stop --force` still kills the VMM; that boot does run recovery, and
     a clean stop afterwards is clean again.
  4. A guest that ignores SIGTERM is still stopped by the guest timeout.
  5. Optional control: the same fixture without shutdown.patch, recorded.

It uses an isolated HOME, daemon socket, network, volumes and VM names. The
fixture copy is snapshotted at start and its file hashes are recorded.
"""

import atexit
import base64
import hashlib
import json
import os
from pathlib import Path
import random
import re
import secrets
import shutil
import subprocess
import tempfile
import time


ROOT = Path(__file__).resolve().parents[1]
if not os.environ.get("JERBOA_TEST_BIN"):
    raise SystemExit("Set JERBOA_TEST_BIN to the macOS bundle's bin directory")
BIN = Path(os.environ["JERBOA_TEST_BIN"]).resolve()
FC = Path(os.environ.get("JERBOA_FIRECRACKER_BIN", BIN / "firecracker")).resolve()
GO = Path(os.environ.get(
    "JERBOA_TEST_GO",
    ROOT / "../firecracker-macos/experiments/hvf/build/guest-tools/go/bin/go",
)).resolve()
FIXTURE = ROOT / "tests/fixtures/postgres-arm64"
EVIDENCE_ROOT = Path(os.environ.get("JERBOA_TEST_EVIDENCE", ROOT / "dist/claude-followup/shutdown")).resolve()
CLEAN_ROUNDS = int(os.environ.get("JERBOA_PG_CLEAN_ROUNDS", "3"))
CONTROL = os.environ.get("JERBOA_PG_SHUTDOWN_CONTROL", "1") == "1"
# Keep ID mode available for controls against historical daemon binaries.
STOP_BY_NAME = os.environ.get("JERBOA_PG_STOP_BY_NAME", "1") == "1"
SUBNET = os.environ.get("JERBOA_PG_SUBNET", "172.31.237")
# A clean stop must finish well before Nanos' 30 s SIGTERM timeout.
CLEAN_STOP_LIMIT = float(os.environ.get("JERBOA_PG_CLEAN_STOP_LIMIT", "20"))
PACKAGE = "postgresql-arm64:11-pthreads-nanos"
CONTROL_PACKAGE = "postgresql-arm64:11-pthreads-nanos-unpatched"

# Unix socket paths are length-limited, so the work directory stays short.
WORK = Path(tempfile.mkdtemp(prefix="jerboa-pgsd-", dir="/tmp"))
DEST = EVIDENCE_ROOT / ("acceptance-" + time.strftime("%Y%m%d-%H%M%S"))
EVIDENCE_ROOT.mkdir(parents=True, exist_ok=True)
RESULT = {"checks": [], "clean_stops": [], "boots": []}


def retain_evidence():
    shutil.copytree(
        WORK,
        DEST,
        dirs_exist_ok=True,
        ignore=shutil.ignore_patterns("home", "stage*", "project-*", "pgpass", "server.key", "*.sock", "stubborn-src"),
    )


atexit.register(retain_evidence)
(WORK / "home").mkdir()
TRANSCRIPT = (WORK / "commands.log").open("w")
ENV = dict(
    os.environ,
    HOME=str(WORK / "home"),
    DOCKER_CONFIG=os.environ.get("DOCKER_CONFIG", str(Path.home() / ".docker")),
    JERBOA_HOST="unix://" + str(WORK / "daemon.sock"),
    JERBOA_AUTH_TOKEN="postgres-shutdown-acceptance-" + secrets.token_hex(8),
)
if not ENV.get("DOCKER_HOST"):
    ENV["DOCKER_HOST"] = subprocess.check_output(
        ["docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"], text=True
    ).strip()
PASSWORD = secrets.token_urlsafe(24)


def redact(text):
    return text.replace(PASSWORD, "<redacted>")


def run(args, *, success=True, env=None, timeout=600):
    args = [str(arg) for arg in args]
    TRANSCRIPT.write(redact("$ " + " ".join(args)) + "\n")
    TRANSCRIPT.flush()
    result = subprocess.run(args, env=env or ENV, text=True, capture_output=True, timeout=timeout)
    TRANSCRIPT.write(redact(result.stdout + result.stderr))
    TRANSCRIPT.flush()
    if success and result.returncode:
        raise RuntimeError(redact(f"command failed ({result.returncode}): {' '.join(args)}\n{result.stdout}{result.stderr}"))
    return result.stdout.strip()


def cli(*args, **kwargs):
    return run([BIN / "jerboa", *args], **kwargs)


def check(label):
    RESULT["checks"].append(label)
    print("PASS:", label, flush=True)


def wait_for(fn, label, timeout=120, interval=0.25):
    deadline = time.monotonic() + timeout
    last = ""
    while time.monotonic() < deadline:
        try:
            value = fn()
            if value:
                return value
        except (OSError, RuntimeError, ValueError, json.JSONDecodeError) as error:
            last = str(error)
        time.sleep(interval)
    raise RuntimeError(f"timeout waiting for {label}: {last}")


def inspect(name):
    return json.loads(cli("inspect", name))


def wait_state(name, state, timeout=120, interval=0.25):
    return wait_for(lambda: inspect(name) if inspect(name).get("state") == state else None,
                    f"{name} {state}", timeout, interval)


def sha256(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def max_ack(logs):
    values = [int(match) for match in re.findall(r"^ACK (\d+)\r?$", logs, re.MULTILINE)]
    return max(values) if values else 0


def package_maps(stage):
    mapped = []
    for subtree in (stage / "db", stage / "usr/local/pgsql/share", stage / "usr/local/pgsql/lib"):
        for source in sorted(path for path in subtree.rglob("*") if path.is_file()):
            mapped.extend(["--map", f"{source}=/{source.relative_to(stage).as_posix()}"])
    for library in ("libgcc_s.so.1",):
        source = stage / "lib/aarch64-linux-gnu" / library
        mapped.extend(["--map", f"{source}=/lib/aarch64-linux-gnu/{library}"])
    # Newer fixtures resolve "localhost" for the statistics collector.
    for name in ("hosts", "nsswitch.conf"):
        source = stage / "etc" / name
        if source.is_file():
            mapped.extend(["--map", f"{source}=/etc/{name}"])
    return mapped


def snapshot_fixture():
    """Copy the fixture once; derive a patched and an unpatched build context."""
    snapshot = WORK / "fixture-snapshot"
    shutil.copytree(FIXTURE, snapshot)
    RESULT["fixture_sha256"] = {p.name: sha256(p) for p in sorted(snapshot.iterdir()) if p.is_file()}
    assert (snapshot / "shutdown.patch").is_file(), "tests/fixtures/postgres-arm64/shutdown.patch is missing"
    dockerfile = (snapshot / "Dockerfile").read_text()
    apply_lines = "COPY shutdown.patch /shutdown.patch\nRUN git -C /src apply /shutdown.patch\n"
    integrated = "shutdown.patch" in dockerfile
    RESULT["dockerfile_integrates_shutdown_patch"] = integrated

    patched = WORK / "fixture-patched"
    shutil.copytree(snapshot, patched)
    if not integrated:
        anchor = "RUN git -C /src apply /openssl.patch\n"
        assert anchor in dockerfile, "Dockerfile no longer applies openssl.patch where expected"
        (patched / "Dockerfile").write_text(dockerfile.replace(anchor, anchor + apply_lines, 1))
    unpatched = WORK / "fixture-unpatched"
    shutil.copytree(snapshot, unpatched)
    (unpatched / "Dockerfile").write_text(dockerfile.replace(apply_lines, ""))
    (unpatched / "shutdown.patch").unlink()
    assert "shutdown.patch" not in (unpatched / "Dockerfile").read_text()
    for name in ("fixture-patched", "fixture-unpatched"):
        shutil.copy(WORK / name / "Dockerfile", WORK / f"{name}.Dockerfile")
    return snapshot, patched, unpatched


def docker_build(context, stage, password_file):
    run([
        "docker", "build", "--platform", "linux/arm64", "--progress", "plain",
        "--secret", f"id=pgpass,src={password_file}",
        "--secret", f"id=pgcert,src={WORK / 'server.crt'}",
        "--secret", f"id=pgkey,src={WORK / 'server.key'}",
        "--build-arg", "CLUSTER_NONCE=" + secrets.token_hex(16),
        "--output", f"type=local,dest={stage}", context,
    ], timeout=3600)
    postgres = stage / "usr/local/pgsql/bin/postgres"
    elf = run(["file", postgres])
    assert "ARM aarch64" in elf, elf
    return postgres


class Cluster:
    def __init__(self):
        self.active = []
        # The daemon does not release a removed VM's IP immediately; never reuse one.
        self.next_host = 10
        self.server_ip = None

    def allocate_ip(self):
        ip = f"{SUBNET}.{self.next_host}"
        self.next_host += 1
        assert self.next_host < 250, "address pool exhausted"
        return ip

    def run_vm(self, *args):
        vm = cli("run", *args).splitlines()[0]
        self.active.append(vm)
        return vm

    def forget(self, name):
        cli("rm", name)
        self.active = [vm for vm in self.active if vm != name]

    def server(self, image, volume, name="pgsd-server"):
        ip = self.server_ip = self.allocate_ip()
        self.run_vm(
            image, "--name", name, "--network", "pgsd-net", "--ip", ip,
            "--memory", "768M", "--cpus", "2", "--health-check", "tcp:5432",
            "--volume", f"{volume}:/db",
        )
        wait_for(lambda: inspect(name).get("health") == "healthy", name + " healthy")

    def client(self, name, mode, *env):
        self.run_vm(
            "pgsd-client:latest", "--name", name, "--network", "pgsd-net", "--ip", self.allocate_ip(),
            "-e", f"PGHOST={self.server_ip}", "-e", f"PGPASSWORD={PASSWORD}", "-e", f"PGMODE={mode}",
            "-e", "PGCA_BASE64=" + base64.b64encode((WORK / "server.crt").read_bytes()).decode(),
            *[arg for pair in env for arg in ("-e", pair)],
        )

    def finished_client(self, name, *, expect):
        wait_state(name, "stopped", timeout=300)
        logs = cli("logs", name)
        (WORK / f"{name}.log").write_text(redact(logs) + "\n")
        self.forget(name)
        for marker in expect:
            assert marker in logs, f"{name}: missing {marker!r}\n{logs}"
        assert "FAIL " not in logs and "panic:" not in logs, logs
        return logs


def pg_lines(logs):
    return [line for line in logs.splitlines() if re.match(r"^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d+ UTC \[", line)]


def assert_no_storage_errors(logs, label):
    for forbidden in ("PANIC:", "could not fsync", "invalid page", "checksum mismatch", "could not flush dirty data"):
        assert forbidden not in logs, f"{label}: server log contains {forbidden!r}"


def graceful_stop(cluster, label, name="pgsd-server", strict=True):
    """jerboa stop; returns the pre-stop server log and timing."""
    # Name mode exercises the daemon's resolved-ID socket lookup.
    target = name if STOP_BY_NAME else inspect(name)["id"]
    started = time.monotonic()
    cli("stop", target, timeout=120)
    wait_state(name, "stopped", timeout=120, interval=0.1)
    seconds = time.monotonic() - started
    logs = cli("logs", name)
    (WORK / f"server-{label}-stop.log").write_text(logs + "\n")
    lines = pg_lines(logs)
    last_pg = lines[-1] if lines else ""
    record = {
        "label": label,
        "stop_seconds": round(seconds, 3),
        "power_button_logged": "power button shutdown requested" in logs,
        "fast_shutdown_logged": "received fast shutdown request" in logs,
        "shutting_down_logged": "LOG:  shutting down" in logs,
        "shut_down_logged": "database system is shut down" in logs,
        "shut_down_is_last_pg_line": "database system is shut down" in last_pg,
        "last_pg_line": last_pg,
    }
    if strict:
        RESULT["clean_stops"].append(record)
        assert record["power_button_logged"], f"{label}: guest did not log the power button request"
        assert record["fast_shutdown_logged"], f"{label}: postmaster did not receive the shutdown request\n{logs}"
        assert record["shut_down_logged"] and record["shut_down_is_last_pg_line"], \
            f"{label}: 'database system is shut down' is not the final PostgreSQL line\n{logs}"
        assert seconds < CLEAN_STOP_LIMIT, f"{label}: stop took {seconds:.1f}s (guest timeout path?)"
        assert_no_storage_errors(logs, label)
    cluster.forget(name)
    return record, logs


def boot_record(label, logs, expect_clean):
    (WORK / f"server-{label}-boot.log").write_text(logs + "\n")
    record = {
        "label": label,
        "was_shut_down_at": "database system was shut down at" in logs,
        "not_properly_shut_down": "not properly shut down" in logs,
        "was_interrupted": "was interrupted" in logs,
        "redo_starts": "redo starts" in logs,
        "expect_clean": expect_clean,
    }
    RESULT["boots"].append(record)
    assert_no_storage_errors(logs, label)
    if expect_clean is True:
        assert record["was_shut_down_at"], f"{label}: missing 'database system was shut down at'\n{logs}"
        assert not (record["not_properly_shut_down"] or record["was_interrupted"] or record["redo_starts"]), \
            f"{label}: crash recovery ran after a clean stop\n{logs}"
    elif expect_clean is False:
        assert record["not_properly_shut_down"] and record["redo_starts"], \
            f"{label}: expected crash recovery after a forced kill\n{logs}"
    return record


def ack_until(cluster, label, target):
    acker = f"pgsd-ack-{label}"
    cluster.client(acker, "ack")
    wait_for(lambda: max_ack(cli("logs", acker)) >= target, f"{acker} ACK {target}", timeout=300, interval=0.1)
    return acker


def collect_acker(cluster, acker):
    """The acker exits on the server's termination message; force it if it hangs."""
    try:
        wait_state(acker, "stopped", timeout=60)
    except RuntimeError:
        cli("stop", acker, "--force")
        wait_state(acker, "stopped", timeout=120)
    logs = cli("logs", acker)
    (WORK / f"{acker}.log").write_text(redact(logs) + "\n")
    cluster.forget(acker)
    return logs


def verify(cluster, label, acked):
    name = f"pgsd-verify-{label}"
    cluster.client(name, "recover", f"PGACK={acked}", "PGAMCHECK=1")
    logs = cluster.finished_client(name, expect=["SQL_OK", "DURABLE_OK", "CHECK amcheck bt_index_check passed"])
    found = re.search(r"RECOVERED acked=(\d+) present=(\d+) max=(\d+) bad_payload=(\d+)", logs)
    return {"acked": acked, "present": int(found.group(2)), "max_id": int(found.group(3)), "bad_payload": int(found.group(4))}


def build_stubborn():
    source = WORK / "stubborn-src"
    source.mkdir()
    (source / "main.go").write_text(
        "package main\n\nimport (\n\t\"fmt\"\n\t\"os/signal\"\n\t\"syscall\"\n\t\"time\"\n)\n\n"
        "func main() {\n\tsignal.Ignore(syscall.SIGTERM)\n\tfmt.Println(\"STUBBORN_READY\")\n"
        "\tfor {\n\t\ttime.Sleep(time.Hour)\n\t}\n}\n"
    )
    binary = WORK / "stubborn-arm64"
    run([GO, "build", "-trimpath", "-o", binary, source / "main.go"],
        env=dict(ENV, GOOS="linux", GOARCH="arm64", CGO_ENABLED="0", GOFLAGS="-mod=mod", GO111MODULE="off"))
    return binary


def main():
    for required in (BIN / "jerboa", BIN / "jerboad", BIN / "tools/kernel.img", FC, GO):
        if not required.exists():
            raise FileNotFoundError(required)
    print("Evidence:", WORK, "->", DEST, flush=True)
    RESULT["bundle_bin"] = str(BIN)
    RESULT["binary_sha256"] = {
        name: sha256(BIN / name) for name in ("jerboa", "jerboad", "firecracker", "tools/kernel.img") if (BIN / name).exists()
    }
    snapshot, patched, unpatched = snapshot_fixture()

    password_file = WORK / "pgpass"
    password_file.write_text(PASSWORD)
    password_file.chmod(0o600)
    run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
         "-keyout", WORK / "server.key", "-out", WORK / "server.crt",
         "-subj", "/CN=postgres.jerboa.test", "-addext", "subjectAltName=DNS:postgres.jerboa.test"])
    (WORK / "server.key").chmod(0o600)

    stages = {PACKAGE: (WORK / "stage", patched)}
    if CONTROL:
        stages[CONTROL_PACKAGE] = (WORK / "stage-unpatched", unpatched)
    postgres_bins = {}
    for package, (stage, context) in stages.items():
        postgres_bins[package] = docker_build(context, stage, password_file)
        RESULT.setdefault("postgres_sha256", {})[package] = sha256(postgres_bins[package])
        assert PASSWORD not in "".join(p.read_text(errors="ignore") for p in (stage / "db").glob("*.conf"))

    client = WORK / "postgres-client-arm64"
    run([GO, "build", "-trimpath", "-o", client, snapshot / "client.go"],
        env=dict(ENV, GOOS="linux", GOARCH="arm64", CGO_ENABLED="0"))
    stubborn = build_stubborn()

    daemon_log = (WORK / "daemon.log").open("w")
    daemon = subprocess.Popen(
        [str(arg) for arg in (
            BIN / "jerboad", "--host", ENV["JERBOA_HOST"], "--tools-dir", BIN / "tools",
            "--hypervisor", "firecracker", "--fc-bin", FC,
        )],
        env=ENV, stdout=daemon_log, stderr=daemon_log,
    )
    cluster = Cluster()
    try:
        wait_for(lambda: cli("status"), "isolated daemon")
        for package, (stage, _context) in stages.items():
            cli(
                "pkg", "create", package, postgres_bins[package], "--sysroot", stage,
                "--program-path", "/usr/local/pgsql/bin/postgres", "--platform", "linux/arm64",
                "--description", "PostgreSQL pthread fork for Nanos ARM64 (shutdown acceptance)",
                *package_maps(stage),
            )
            image = "pgsd-postgres" if package == PACKAGE else "pgsd-postgres-unpatched"
            project = WORK / f"project-{image}"
            project.mkdir()
            toml = (snapshot / "unikernel.toml").read_text()
            assert f'"{PACKAGE}"' in toml, "unikernel.toml no longer names the expected package"
            (project / "unikernel.toml").write_text(toml.replace(f'"{PACKAGE}"', f'"{package}"'))
            cli("build", project, "--platform", "linux/arm64", "--name", image)
        cli("build", client, "--platform", "linux/arm64", "--name", "pgsd-client")
        cli("build", stubborn, "--platform", "linux/arm64", "--name", "pgsd-stubborn")
        cli("network", "create", "pgsd-net", "--subnet", f"{SUBNET}.0/24")
        for volume, package in (("pgsd-data", PACKAGE), *((("pgsd-control", CONTROL_PACKAGE),) if CONTROL else ())):
            cli("volume", "create", volume, "--size", "512M", "--seed-pkg", package,
                "--pkg-source", "jerboa", "--src", "/db", timeout=300)

        image = "pgsd-postgres:latest"
        # Seed SQL data (includes CHECKPOINT) the recovery client verifies later.
        cluster.server(image, "pgsd-data")
        boot_record("first", cli("logs", "pgsd-server"), None)
        cluster.client("pgsd-sql", "write")
        cluster.finished_client("pgsd-sql", expect=["SQL_OK", "VALUE=nanos-arm64"])

        # Idle clean stop.
        graceful_stop(cluster, "idle")
        check("idle jerboa stop: fast shutdown ended with 'database system is shut down'")
        cluster.server(image, "pgsd-data")
        boot_record("after-idle", cli("logs", "pgsd-server"), True)
        acked = 0
        RESULT["verifications"] = []
        cluster.client("pgsd-verify-after-idle", "verify")
        cluster.finished_client("pgsd-verify-after-idle", expect=["SQL_OK", "VALUE=nanos-arm64"])
        check("boot after idle stop: 'was shut down at', no crash recovery; SQL data readable")

        # Clean stops while a client is committing and checkpointing.
        for number in range(1, CLEAN_ROUNDS + 1):
            label = f"loaded{number}"
            acker = ack_until(cluster, label, acked + random.randint(300, 700))
            record, _ = graceful_stop(cluster, label)
            ack_logs = collect_acker(cluster, acker)
            acked = max_ack(ack_logs)
            record.update(acked=acked, checkpoints_seen=ack_logs.count("CHECKPOINTED"),
                          client_stopped_by_server="ACK_STOPPED" in ack_logs)
            cluster.server(image, "pgsd-data")
            boot_record(f"after-{label}", cli("logs", "pgsd-server"), True)
            result = verify(cluster, label, acked)
            RESULT["verifications"].append(dict(result, label=label))
            assert result["present"] == acked and result["bad_payload"] == 0, result
            check(f"{label}: stop during commits was clean in {record['stop_seconds']}s; "
                  f"all {acked} acknowledged commits present without recovery")

        # Forced kill still works and still recovers.
        acker = ack_until(cluster, "kill", acked + random.randint(200, 500))
        started = time.monotonic()
        cli("stop", "pgsd-server", "--force")
        wait_state("pgsd-server", "stopped", timeout=60, interval=0.1)
        RESULT["force_stop_seconds"] = round(time.monotonic() - started, 3)
        killed = cli("logs", "pgsd-server")
        (WORK / "server-kill-before.log").write_text(killed + "\n")
        assert "database system is shut down" not in killed, "forced stop unexpectedly shut down cleanly"
        cluster.forget("pgsd-server")
        cli("stop", acker, "--force")
        ack_logs = collect_acker(cluster, acker)
        acked = max_ack(ack_logs)
        cluster.server(image, "pgsd-data")
        boot_record("after-kill", cli("logs", "pgsd-server"), False)
        result = verify(cluster, "kill", acked)
        RESULT["verifications"].append(dict(result, label="kill"))
        assert result["present"] == acked and result["bad_payload"] == 0, result
        check(f"jerboa stop --force killed the VMM in {RESULT['force_stop_seconds']}s; recovery kept all {acked} commits")

        graceful_stop(cluster, "after-recovery")
        cluster.server(image, "pgsd-data")
        boot_record("after-recovery-stop", cli("logs", "pgsd-server"), True)
        RESULT["verifications"].append(dict(verify(cluster, "after-recovery-stop", acked), label="after-recovery-stop"))
        graceful_stop(cluster, "final")
        check("clean stop after a crash-recovered boot is clean again")

        # Guest SIGTERM timeout still bounds a stop.
        cluster.run_vm("pgsd-stubborn:latest", "--name", "pgsd-stubborn", "--network", "pgsd-net",
                       "--ip", cluster.allocate_ip())
        wait_for(lambda: "STUBBORN_READY" in cli("logs", "pgsd-stubborn"), "stubborn guest ready")
        stubborn_target = "pgsd-stubborn" if STOP_BY_NAME else inspect("pgsd-stubborn")["id"]
        started = time.monotonic()
        cli("stop", stubborn_target, timeout=120)
        wait_state("pgsd-stubborn", "stopped", timeout=120)
        seconds = time.monotonic() - started
        stubborn_logs = cli("logs", "pgsd-stubborn")
        (WORK / "stubborn-guest.log").write_text(stubborn_logs + "\n")
        RESULT["timeout_stop_seconds"] = round(seconds, 3)
        assert 25 <= seconds <= 45, f"SIGTERM-ignoring guest stopped after {seconds:.1f}s"
        cluster.forget("pgsd-stubborn")
        check(f"SIGTERM-ignoring guest still stopped by the timeout after {seconds:.1f}s")

        # Control: identical fixture without shutdown.patch (recorded, not asserted).
        if CONTROL:
            cluster.server("pgsd-postgres-unpatched:latest", "pgsd-control")
            cluster.client("pgsd-control-sql", "write")
            cluster.finished_client("pgsd-control-sql", expect=["SQL_OK"])
            record, _ = graceful_stop(cluster, "control-unpatched", strict=False)
            cluster.server("pgsd-postgres-unpatched:latest", "pgsd-control")
            boot = boot_record("control-unpatched-after-stop", cli("logs", "pgsd-server"), None)
            graceful_stop(cluster, "control-unpatched-final", strict=False)
            RESULT["control_unpatched"] = {"stop": record, "boot": boot}
            print("INFO: unpatched control:", json.dumps(RESULT["control_unpatched"]), flush=True)

        RESULT["status"] = "PASS"
        print("PASS: PostgreSQL clean shutdown acceptance", flush=True)
    except Exception as error:
        RESULT["status"] = "FAIL"
        RESULT["error"] = redact(str(error))
        try:
            for vm in json.loads(cli("ps", "--output", "json")):
                (WORK / f"failure-{vm['name']}-{vm['id']}.log").write_text(redact(cli("logs", vm["id"])) + "\n")
        except Exception:
            pass
        raise
    finally:
        (WORK / "RESULT.json").write_text(json.dumps(RESULT, indent=2) + "\n")
        for vm in reversed(cluster.active):
            for command in (("stop", vm, "--force"), ("rm", vm)):
                try:
                    cli(*command)
                except Exception:
                    pass
        for command in (("volume", "rm", "pgsd-data"), ("volume", "rm", "pgsd-control"), ("network", "rm", "pgsd-net")):
            try:
                cli(*command)
            except Exception:
                pass
        daemon.terminate()
        try:
            daemon.wait(20)
        except subprocess.TimeoutExpired:
            daemon.kill()
            daemon.wait()
        daemon_log.close()
        TRANSCRIPT.close()


if __name__ == "__main__":
    main()
