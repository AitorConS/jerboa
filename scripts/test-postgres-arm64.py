#!/usr/bin/env python3
"""Build and qualify real PostgreSQL over a two-guest Firecracker/HVF network.

Phases (all against one named volume unless stated):
  1. SQL suite, durability settings and SCRAM authentication checks; live
     statistics collector, exact table counters and automatic VACUUM/ANALYZE
     at controlled per-table thresholds.
  2. Cooperative stop, VM replacement and read-back; collector and autovacuum
     resume (counters persist only after a clean shutdown).
  3. Repeated abrupt stops (SIGKILL of the VMM process) while a client commits
     one row per transaction; after recovery every commit the client saw
     acknowledged must be present with an intact payload, full heap reads must
     pass page checksums, index and heap must agree, and amcheck must pass.
  4. Negative control: fsync=off on a separate volume, same abrupt stop. Loss
     is reported, not asserted; it shows whether the kill test can detect a
     durability violation at all.

The test uses an isolated HOME, daemon socket, network, volumes and VM names.
Evidence is retained below JERBOA_TEST_EVIDENCE even when the test fails.
Abrupt VMM termination is a guest/VMM crash model. The host kernel page cache
survives it, so this does NOT qualify host power loss.
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
    raise SystemExit("Set JERBOA_TEST_BIN to the updated macOS bundle's bin directory")
BIN = Path(os.environ["JERBOA_TEST_BIN"]).resolve()
FC = Path(os.environ.get("JERBOA_FIRECRACKER_BIN", BIN / "firecracker")).resolve()
GO = Path(os.environ.get(
    "JERBOA_TEST_GO",
    ROOT / "../firecracker-macos/experiments/hvf/build/guest-tools/go/bin/go",
)).resolve()
FIXTURE = ROOT / "tests/fixtures/postgres-arm64"
EVIDENCE_ROOT = Path(os.environ.get("JERBOA_TEST_EVIDENCE", ROOT / "dist/claude-closure/postgres")).resolve()
KILL_ROUNDS = int(os.environ.get("JERBOA_PG_KILL_ROUNDS", "3"))
NEGATIVE_CONTROL = os.environ.get("JERBOA_PG_NEGATIVE_CONTROL", "1") == "1"
STOP_BY_NAME = os.environ.get("JERBOA_PG_STOP_BY_NAME", "1") == "1"
SUBNET = os.environ.get("JERBOA_PG_SUBNET", "172.31.231")
# Unix socket paths are length-limited, so the work directory stays short.
WORK = Path(tempfile.mkdtemp(prefix="jerboa-pg-", dir="/tmp"))
DEST = EVIDENCE_ROOT / ("acceptance-" + time.strftime("%Y%m%d-%H%M%S"))
EVIDENCE_ROOT.mkdir(parents=True, exist_ok=True)
RESULT = {"rounds": [], "checks": []}


def retain_evidence():
    shutil.copytree(
        WORK,
        DEST,
        dirs_exist_ok=True,
        ignore=shutil.ignore_patterns("home", "stage", "project-*", "pgpass", "server.key", "*.sock"),
    )


atexit.register(retain_evidence)
(WORK / "home").mkdir()
TRANSCRIPT = (WORK / "commands.log").open("w")
ENV = dict(
    os.environ,
    HOME=str(WORK / "home"),
    DOCKER_CONFIG=os.environ.get("DOCKER_CONFIG", str(Path.home() / ".docker")),
    JERBOA_HOST="unix://" + str(WORK / "daemon.sock"),
    JERBOA_AUTH_TOKEN="postgres-arm64-acceptance-" + secrets.token_hex(8),
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
    result = subprocess.run(
        args, env=env or ENV, text=True, capture_output=True, timeout=timeout
    )
    TRANSCRIPT.write(redact(result.stdout + result.stderr))
    TRANSCRIPT.flush()
    if success and result.returncode:
        raise RuntimeError(redact(f"command failed ({result.returncode}): {' '.join(args)}\n{result.stdout}{result.stderr}"))
    if not success and not result.returncode:
        raise AssertionError("command unexpectedly succeeded: " + redact(" ".join(args)))
    return result.stdout.strip() if success else result.stderr.strip()


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


def wait_state(name, state, timeout=120):
    return wait_for(lambda: inspect(name) if inspect(name).get("state") == state else None, f"{name} {state}", timeout)


def wait_healthy(name):
    return wait_for(lambda: inspect(name) if inspect(name).get("health") == "healthy" else None, name + " healthy")


def package_maps(stage):
    mapped = []
    for subtree in (stage / "db", stage / "usr/local/pgsql/share", stage / "usr/local/pgsql/lib"):
        for source in sorted(path for path in subtree.rglob("*") if path.is_file()):
            guest = "/" + source.relative_to(stage).as_posix()
            mapped.extend(["--map", f"{source}={guest}"])
    libgcc = stage / "lib/aarch64-linux-gnu/libgcc_s.so.1"
    mapped.extend(["--map", f"{libgcc}=/lib/aarch64-linux-gnu/libgcc_s.so.1"])
    # The statistics collector needs getaddrinfo("localhost") to work.
    for name in ("hosts", "nsswitch.conf"):
        mapped.extend(["--map", f"{stage / 'etc' / name}=/etc/{name}"])
    return mapped


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

    def remove(self, name, force=False):
        # Exercise name resolution by default; ID mode remains available for controls.
        target = name if force or STOP_BY_NAME else inspect(name)["id"]
        cli("stop", target, *(["--force"] if force else []))
        wait_state(name, "stopped")
        cli("rm", name)
        self.active = [vm for vm in self.active if vm != name]

    def server(self, image, volume):
        ip = self.server_ip = self.allocate_ip()
        self.run_vm(
            image, "--name", "pgc-server", "--network", "pgc-net", "--ip", ip,
            "--memory", "768M", "--cpus", "2", "--health-check", "tcp:5432",
            "--volume", f"{volume}:/db",
        )
        wait_healthy("pgc-server")

    def client(self, name, mode, *env):
        ip = self.allocate_ip()
        self.run_vm(
            "pgc-client:latest", "--name", name, "--network", "pgc-net", "--ip", ip,
            "-e", f"PGHOST={self.server_ip}", "-e", f"PGPASSWORD={PASSWORD}", "-e", f"PGMODE={mode}",
            "-e", "PGCA_BASE64=" + base64.b64encode((WORK / "server.crt").read_bytes()).decode(),
            *[arg for pair in env for arg in ("-e", pair)],
        )

    def finished_client(self, name, *, expect):
        wait_state(name, "stopped", timeout=300)
        logs = cli("logs", name)
        (WORK / f"{name}.log").write_text(redact(logs) + "\n")
        cli("rm", name)
        self.active = [vm for vm in self.active if vm != name]
        for marker in expect:
            assert marker in logs, f"{name}: missing {marker!r}\n{logs}"
        assert "FAIL " not in logs and "panic:" not in logs, logs
        return logs

    def finished_client_after(self, name, mode, *, expect):
        self.client(name, mode)
        return self.finished_client(name, expect=["SQL_OK", *expect])


def server_log(label):
    logs = cli("logs", "pgc-server")
    (WORK / f"server-{label}.log").write_text(logs + "\n")
    return logs


def assert_server_log(logs, label):
    flush = logs.count("could not flush dirty data")
    assert flush == 0, f"{label}: {flush} 'could not flush dirty data' warnings"
    for forbidden in ("PANIC:", "could not fsync", "invalid page", "checksum mismatch"):
        assert forbidden not in logs, f"{label}: server log contains {forbidden!r}"
    # The statistics collector and autovacuum must be genuinely running.
    for forbidden in (
        "could not resolve \"localhost\"",
        "statistics collector",
        "autovacuum not started",
        "using stale statistics",
        "trying another address",
        "could not start autovacuum",
    ):
        assert forbidden not in logs, f"{label}: server log contains {forbidden!r}"


def autovacuum_runs(logs, table):
    return (
        logs.count(f'automatic vacuum of table "postgres.public.{table}"'),
        logs.count(f'automatic analyze of table "postgres.public.{table}"'),
    )


def max_ack(logs):
    values = [int(match) for match in re.findall(r"^ACK (\d+)\r?$", logs, re.MULTILINE)]
    return max(values) if values else 0


def abrupt_round(cluster, label, image, volume, strict, round_number):
    target = random.randint(150, 900)
    cluster.client(f"pgc-ack-{label}", "ack")
    acker = f"pgc-ack-{label}"
    start = wait_for(lambda: max_ack(cli("logs", acker)) >= target, f"{acker} ACK {target}", timeout=300, interval=0.1)
    assert start
    acked = max_ack(cli("logs", acker))
    killed_at = time.monotonic()
    cli("stop", "pgc-server", "--force")
    kill_seconds = time.monotonic() - killed_at
    wait_state("pgc-server", "stopped")
    (WORK / f"server-{label}-before-kill.log").write_text(cli("logs", "pgc-server") + "\n")
    # The killed server never sends a TCP reset over the userspace network, so
    # the acker can block forever in read(). Its ACK lines are already on the
    # console; capture them and stop it.
    ack_logs = cli("logs", acker)
    (WORK / f"{acker}.log").write_text(redact(ack_logs) + "\n")
    cli("stop", acker, "--force")
    wait_state(acker, "stopped", timeout=120)
    cli("rm", acker)
    cluster.active = [vm for vm in cluster.active if vm != acker]
    cli("rm", "pgc-server")
    cluster.active = [vm for vm in cluster.active if vm != "pgc-server"]

    cluster.server(image, volume)
    verifier = f"pgc-verify-{label}"
    cluster.client(verifier, "recover" if strict else "report", f"PGACK={acked}", "PGAMCHECK=1")
    logs = cluster.finished_client(verifier, expect=["SQL_OK", "DURABLE_OK" if strict else "REPORT_OK"])
    recovery = server_log(f"{label}-recovery")
    assert_server_log(recovery, label)
    found = re.search(r"RECOVERED acked=(\d+) present=(\d+) max=(\d+) bad_payload=(\d+)", logs)
    round_result = {
        "label": label,
        "round": round_number,
        "target_ack": target,
        "acked_before_kill": acked,
        "client_last_ack_in_log": max_ack(ack_logs),
        "present": int(found.group(2)),
        "max_id_after_recovery": int(found.group(3)),
        "bad_payload": int(found.group(4)),
        "checkpoints_seen": ack_logs.count("CHECKPOINTED"),
        "stop_force_seconds": round(kill_seconds, 3),
        "recovery_log_interrupted": "was interrupted" in recovery or "not properly shut down" in recovery,
        "recovery_redo": "redo starts" in recovery,
        "strict": strict,
    }
    RESULT["rounds"].append(round_result)
    return round_result


def main():
    global FIXTURE
    for required in (BIN / "jerboa", BIN / "jerboad", BIN / "tools/kernel.img", FC, GO):
        if not required.exists():
            raise FileNotFoundError(required)
    print("Evidence:", WORK, "->", DEST, flush=True)
    shutil.copytree(FIXTURE, WORK / "fixture")
    FIXTURE = WORK / "fixture"
    RESULT["fixture_sha256"] = {
        p.name: hashlib.sha256(p.read_bytes()).hexdigest()
        for p in sorted(FIXTURE.iterdir()) if p.is_file()
    }
    RESULT["stop_by_name"] = STOP_BY_NAME
    password_file = WORK / "pgpass"
    password_file.write_text(PASSWORD)
    password_file.chmod(0o600)
    run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
         "-keyout", WORK / "server.key", "-out", WORK / "server.crt",
         "-subj", "/CN=postgres.jerboa.test", "-addext", "subjectAltName=DNS:postgres.jerboa.test"])
    (WORK / "server.key").chmod(0o600)

    stage = WORK / "stage"
    run([
        "docker", "build", "--platform", "linux/arm64", "--progress", "plain",
        "--secret", f"id=pgpass,src={password_file}",
        "--secret", f"id=pgcert,src={WORK / 'server.crt'}",
        "--secret", f"id=pgkey,src={WORK / 'server.key'}",
        "--build-arg", "CLUSTER_NONCE=" + secrets.token_hex(16),
        "--output", f"type=local,dest={stage}", FIXTURE,
    ], timeout=2400)
    postgres = stage / "usr/local/pgsql/bin/postgres"
    elf = run(["file", postgres])
    assert "ARM aarch64" in elf, elf
    (WORK / "postgres-file.txt").write_text(elf + "\n")
    hba = (stage / "db/pg_hba.conf").read_text()
    assert "trust" not in hba and hba.count("scram-sha-256") == 3, hba
    assert PASSWORD not in "".join(p.read_text(errors="ignore") for p in (stage / "db").glob("*.conf"))
    (WORK / "pg_hba.conf").write_text(hba)
    check("pg_hba uses only scram-sha-256; password is absent from configuration files")

    cli(
        "pkg", "create", "postgresql-arm64:11-pthreads-nanos", postgres,
        "--sysroot", stage,
        "--program-path", "/usr/local/pgsql/bin/postgres",
        "--platform", "linux/arm64",
        "--description", "Pinned PostgreSQL pthread fork adapted for Nanos ARM64",
        *package_maps(stage),
    )
    packages = json.loads(cli("pkg", "list", "--source", "jerboa", "--output-json"))
    selected = next(pkg for pkg in packages if pkg["name"] == "postgresql-arm64")
    (WORK / "package.json").write_text(json.dumps(selected, indent=2) + "\n")

    client = WORK / "postgres-client-arm64"
    go_env = dict(ENV, GOOS="linux", GOARCH="arm64", CGO_ENABLED="0")
    run([GO, "build", "-trimpath", "-o", client, FIXTURE / "client.go"], env=go_env)
    client_elf = run(["file", client])
    assert "ARM aarch64" in client_elf and "statically linked" in client_elf, client_elf

    projects = {}
    for name, extra in (("pgc-postgres", []), ("pgc-postgres-nofsync", ["-c", "fsync=off"])):
        project = WORK / f"project-{name}"
        project.mkdir()
        toml = (FIXTURE / "unikernel.toml").read_text()
        if extra:
            toml = toml.replace('"-c", "fsync=on",', '"-c", "fsync=off",')
            assert '"fsync=off"' in toml
        (project / "unikernel.toml").write_text(toml)
        projects[name] = project

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
        for name, project in projects.items():
            cli("build", project, "--platform", "linux/arm64", "--name", name)
        cli("build", client, "--platform", "linux/arm64", "--name", "pgc-client")
        manifest = json.loads(cli("images", "inspect", "pgc-postgres:latest"))
        assert manifest["architecture"] == "arm64" and manifest["platform"] == "linux/arm64", manifest
        (WORK / "image-manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")

        cli("network", "create", "pgc-net", "--subnet", f"{SUBNET}.0/24")
        for volume in ("pgc-data", *(["pgc-nofsync"] if NEGATIVE_CONTROL else [])):
            cli(
                "volume", "create", volume, "--size", "512M",
                "--seed-pkg", "postgresql-arm64:11-pthreads-nanos", "--pkg-source", "jerboa", "--src", "/db",
                timeout=300,
            )

        # Phase 1: SQL, settings and authentication.
        cluster.server("pgc-postgres:latest", "pgc-data")
        cluster.client("pgc-sql", "write")
        cluster.finished_client("pgc-sql", expect=["SQL_OK", "VALUE=nanos-arm64", "CHECK SHOW data_checksums = on"])
        check("SQL suite, durability settings, checksums and rejected flush hint from a separate guest")
        cluster.client("pgc-auth", "auth")
        cluster.finished_client("pgc-auth", expect=["AUTH_REJECT_OK"])
        check("empty and wrong passwords rejected; client refuses non-SCRAM authentication")
        cluster.client("pgc-tls", "tls")
        cluster.finished_client("pgc-tls", expect=["TLS_REJECT_OK"])
        check("TLS verified; plaintext, TLS below 1.2, wrong name and untrusted CA rejected; concurrent TLS/reload passed")
        stats = cluster.finished_client_after("pgc-stats", "stats", expect=["STATS_OK"])
        cluster.finished_client_after("pgc-signals", "signals", expect=["BACKEND_SIGNALS_OK", "PROTOCOL_CANCEL_OK", "AUTOVACUUM_LOCK_CANCEL_OK", "PARALLEL_FALLBACK_OK"])
        first_boot = server_log("first-boot")
        assert "canceling autovacuum task" in first_boot, "blocking automatic worker was not canceled"
        check("SQL backend cancel/terminate use live thread identities; stale IDs rejected; blocking autovacuum canceled without crashing the unikernel")
        assert_server_log(first_boot, "first boot")
        vacuums, analyzes = autovacuum_runs(first_boot, "av_probe")
        assert vacuums >= 1 and analyzes >= 1, f"server log lacks automatic vacuum/analyze of av_probe: {vacuums}, {analyzes}"
        assert autovacuum_runs(first_boot, "av_control") == (0, 0), "autovacuum processed the disabled control table"
        RESULT["stats"] = {
            "before_autovacuum": re.search(r"STATS_BEFORE_AUTOVACUUM av_probe=(\S+)", stats).group(1),
            "after_autovacuum": re.search(r"STATS_AFTER_AUTOVACUUM av_probe=(\S+)", stats).group(1),
            "columns": "n_tup_ins,n_tup_upd,n_tup_del,n_live_tup,n_dead_tup,seq_scan,idx_scan,vacuum_count,autovacuum_count,analyze_count,autoanalyze_count",
            "server_log_autovacuum_av_probe": vacuums,
            "server_log_autoanalyze_av_probe": analyzes,
        }
        check("track_counts and collector live; exact table counters; automatic VACUUM and ANALYZE at controlled thresholds; disabled control untouched")

        # Phase 2: cooperative stop and replacement.
        started = time.monotonic()
        cluster.remove("pgc-server")
        RESULT["cooperative_stop_seconds"] = round(time.monotonic() - started, 3)
        cluster.server("pgc-postgres:latest", "pgc-data")
        cluster.client("pgc-verify", "verify")
        cluster.finished_client("pgc-verify", expect=["SQL_OK"])
        restart_stats = cluster.finished_client_after("pgc-stats-restart", "stats-restart", expect=["STATS_RESTART_OK"])
        restart = server_log("after-cooperative-stop")
        assert_server_log(restart, "cooperative restart")
        RESULT["cooperative_stop_was_clean_shutdown"] = "was shut down at" in restart
        RESULT["cooperative_stop_needed_recovery"] = "not properly shut down" in restart or "was interrupted" in restart
        assert RESULT["cooperative_stop_was_clean_shutdown"] and not RESULT["cooperative_stop_needed_recovery"], \
            "cooperative stop must shut PostgreSQL down cleanly, not rely on crash recovery"
        check("data readable after cooperative stop and VM replacement")
        persisted = "STATS_PERSISTED" in restart_stats
        RESULT["stats"]["after_restart_counters"] = "persisted" if persisted else "reset"
        RESULT["stats"]["after_restart"] = re.search(r"STATS_AFTER_RESTART av_probe=(\S+)", restart_stats).group(1)
        if RESULT["cooperative_stop_was_clean_shutdown"] and not RESULT["cooperative_stop_needed_recovery"]:
            # A clean shutdown writes the permanent stats file; losing it is a bug.
            assert persisted, "clean shutdown but statistics were reset"
        vacuums, _ = autovacuum_runs(restart, "av_probe")
        assert vacuums >= 1, "no automatic vacuum of av_probe after restart"
        assert autovacuum_runs(restart, "av_control") == (0, 0), "autovacuum processed the disabled control table after restart"
        check(f"after restart ({RESULT['stats']['after_restart_counters']} counters) collector and autovacuum resumed at controlled thresholds")

        # Phase 3: abrupt stops during committed writes and checkpoints.
        for number in range(1, KILL_ROUNDS + 1):
            result = abrupt_round(cluster, f"kill{number}", "pgc-postgres:latest", "pgc-data", True, number)
            check(f"abrupt stop {number}: all {result['acked_before_kill']} acknowledged commits recovered intact")
        cluster.remove("pgc-server")

        # Phase 4: fsync=off negative control on its own volume.
        # The control is not an acceptance criterion: any outcome, including a
        # replacement server that never recovers, is recorded rather than failing.
        if NEGATIVE_CONTROL:
            cluster.server("pgc-postgres-nofsync:latest", "pgc-nofsync")
            try:
                result = abrupt_round(cluster, "nofsync", "pgc-postgres-nofsync:latest", "pgc-nofsync", False, 1)
                lost = result["acked_before_kill"] - result["present"]
                RESULT["negative_control"] = {"outcome": "recovered", "lost_acknowledged_commits": lost}
                print(f"INFO: fsync=off negative control lost {lost} of {result['acked_before_kill']} acknowledged commits", flush=True)
            except Exception as error:
                RESULT["negative_control"] = {"outcome": "did not recover", "error": redact(str(error))}
                print(f"INFO: fsync=off negative control did not recover: {error}", flush=True)
                for vm in list(cluster.active):
                    try:
                        (WORK / f"negative-control-{vm}.log").write_text(redact(cli("logs", vm)) + "\n")
                    except Exception:
                        pass
            for vm in list(cluster.active):
                try:
                    cluster.remove(vm, force=True)
                except Exception:
                    pass

        RESULT["status"] = "PASS"
        print("PASS: PostgreSQL durability acceptance", flush=True)
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
        for command in (("volume", "rm", "pgc-data"), ("volume", "rm", "pgc-nofsync"), ("network", "rm", "pgc-net")):
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
