#!/usr/bin/env python3
"""Simulated host power loss for PostgreSQL inside Nanos on the macOS HVF VMM.

This is NOT a physical power-cut test and does not edit the PostgreSQL
acceptance harness. It reuses the frozen, reviewed PostgreSQL fixture with a
test-only VMM built with -DHVF_CRASH_JOURNAL. See test-power-loss-sim.py for the
journal and replay models. For each round:

  1. A seeded volume containing the SQL-phase tables is restored.
  2. A client commits one row per transaction, printing ACK only after COMMIT.
  3. The server VMM is killed after a random number of acknowledgements.
  4. The journal is replayed: "full" must equal the killed disk; "durable" and
     seeded "subset" models drop the writes a host cache loss could drop.
  5. PostgreSQL recovers each image; every acknowledged row must be present
     with intact payload, checksums, heap/index agreement and amcheck.
  6. Seeded earlier flush boundaries (writes up to flush k plus a subset of the
     sync then in flight) must recover with the same integrity checks. Which
     commits were acknowledged at that point is unknown, so PGACK=0 is used and
     only crash consistency is asserted there.

Negative controls: fsync=off and a VMM that acknowledges FLUSH without a
durability point. Both must be detected by the "durable" model.

Environment:
  JERBOA_TEST_BIN, DURA_FC_JOURNAL, JERBOA_TEST_GO
  DURA_PG_PREP     directory with pgpass, server.crt, server.key and the Docker
                   stage/ built from DURA_PG_FIXTURE (all kept private)
  DURA_PG_FIXTURE  frozen fixture directory (client.go, unikernel.toml)
  DURA_PG_ROUNDS (2), DURA_SUBSETS (2), DURA_SEED, DURA_PG_SUBNET (172.31.251)
"""

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
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
REPLAY = (ROOT / "../firecracker-macos/experiments/hvf/durability/crash_replay.py").resolve()
BIN = Path(os.environ["JERBOA_TEST_BIN"]).resolve()
FC = Path(os.environ["DURA_FC_JOURNAL"]).resolve()
GO = Path(os.environ["JERBOA_TEST_GO"]).resolve()
PREP = Path(os.environ["DURA_PG_PREP"]).resolve()
FIXTURE = Path(os.environ["DURA_PG_FIXTURE"]).resolve()
ROUNDS = int(os.environ.get("DURA_PG_ROUNDS", "2"))
SUBSETS = int(os.environ.get("DURA_SUBSETS", "2"))
BOUNDARIES = int(os.environ.get("DURA_BOUNDARIES", "3"))
SEED = int(os.environ.get("DURA_SEED", str(secrets.randbits(31))))
SUBNET = os.environ.get("DURA_PG_SUBNET", "172.31.251")
EVIDENCE_ROOT = Path(os.environ.get("DURA_EVIDENCE", ROOT / "dist/claude-followup/durability/postgres-powerloss-sim")).resolve()
WORK = Path(tempfile.mkdtemp(prefix="jdpg-", dir="/tmp"))
DEST = EVIDENCE_ROOT / ("run-" + time.strftime("%Y%m%d-%H%M%S"))
JOURNAL = WORK / "journal"
RNG = random.Random(SEED)
PASSWORD = (PREP / "pgpass").read_text().strip()
RESULT = {"seed": SEED, "work": str(WORK), "rounds": [], "controls": {}, "checks": []}
(WORK / "home").mkdir()
JOURNAL.mkdir()
TRANSCRIPT = (WORK / "commands.log").open("w")
ENV = dict(
    os.environ,
    HOME=str(WORK / "home"),
    JERBOA_HOST="unix://" + str(WORK / "daemon.sock"),
    JERBOA_AUTH_TOKEN="pg-power-loss-" + secrets.token_hex(8),
)


def redact(text):
    return text.replace(PASSWORD, "<redacted>")


def run(args, *, success=True, env=None, timeout=900):
    args = [str(a) for a in args]
    TRANSCRIPT.write(redact("$ " + " ".join(args)) + "\n")
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


def wait_for(fn, label, timeout=180, interval=0.25):
    deadline = time.monotonic() + timeout
    last = ""
    while time.monotonic() < deadline:
        try:
            value = fn()
            if value:
                return value
        except (OSError, RuntimeError, ValueError) as error:
            last = str(error)
        time.sleep(interval)
    raise TimeoutError(f"timeout waiting for {label}: {redact(last)}")


def inspect(name):
    return json.loads(cli("inspect", name))


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


def max_ack(logs):
    values = [int(v) for v in re.findall(r"^ACK (\d+)\r?$", logs, re.MULTILINE)]
    return max(values) if values else 0


def package_maps(stage):
    # Identical to the reviewed acceptance script used for this fixture.
    mapped = []
    for subtree in (stage / "db", stage / "usr/local/pgsql/share", stage / "usr/local/pgsql/lib"):
        for source in sorted(p for p in subtree.rglob("*") if p.is_file()):
            mapped.extend(["--map", f"{source}=/{source.relative_to(stage).as_posix()}"])
    mapped.extend(["--map", f"{stage / 'lib/aarch64-linux-gnu/libgcc_s.so.1'}=/lib/aarch64-linux-gnu/libgcc_s.so.1"])
    # Newer fixtures resolve localhost for the statistics collector; map these
    # only when the stage provides them, so the frozen fixture is unchanged.
    for name in ("hosts", "nsswitch.conf"):
        if (stage / "etc" / name).is_file():
            mapped.extend(["--map", f"{stage / 'etc' / name}=/etc/{name}"])
    return mapped


class Daemon:
    process = None

    def start(self, label, **extra):
        self.log = (WORK / f"daemon-{label}.log").open("w")
        self.process = subprocess.Popen(
            [str(a) for a in (BIN / "jerboad", "--host", ENV["JERBOA_HOST"], "--tools-dir", BIN / "tools",
                              "--hypervisor", "firecracker", "--fc-bin", FC)],
            env=dict(ENV, HVF_CRASH_JOURNAL_DIR=str(JOURNAL), **extra), stdout=self.log, stderr=self.log,
        )
        wait_for(lambda: cli("status"), "daemon " + label)

    def stop(self):
        if self.process:
            self.process.terminate()
            try:
                self.process.wait(20)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait()
            self.log.close()
            self.process = None


DAEMON = Daemon()


class Cluster:
    def __init__(self):
        self.active = []
        self.next_host = 10
        self.server_ip = None

    def ip(self):
        value = f"{SUBNET}.{self.next_host}"
        self.next_host += 1
        if self.next_host >= 250:
            raise RuntimeError("address pool exhausted")
        return value

    def run_vm(self, *args):
        cli("run", *args)
        # Track by name: every call passes --name, and all later commands use it.
        name = args[args.index("--name") + 1]
        self.active.append(name)
        return name

    def drop(self, name):
        self.active = [vm for vm in self.active if vm != name]

    def remove(self, name, force=False):
        try:
            try:
                current = inspect(name).get("state")
            except RuntimeError as error:
                if "not found" in str(error):
                    return
                raise
            if current != "stopped":
                # Stop by resolved ID: older jerboad builds send the cooperative
                # power-button request only for IDs, not names.
                cli("stop", inspect(name).get("id") or name, *(["--force"] if force else []))
                wait_for(lambda: inspect(name).get("state") == "stopped", name + " stopped", timeout=120)
        finally:
            cli("rm", name, success=False)
            self.drop(name)

    def server(self, image, volume, timeout=240):
        self.server_ip = self.ip()
        self.run_vm(image, "--name", "pgd-server", "--network", "pgd-net", "--ip", self.server_ip,
                    "--memory", "768M", "--cpus", "2", "--health-check", "tcp:5432", "--volume", f"{volume}:/db")
        def ready():
            vm = inspect("pgd-server")
            if vm.get("state") == "stopped":
                # Do not swallow this in wait_for's transient-error retries.
                raise AssertionError("PostgreSQL VM exited during startup")
            return vm.get("health") == "healthy"

        wait_for(ready, "pgd-server healthy", timeout=timeout)

    def client(self, name, mode, *env):
        self.run_vm("pgd-client:latest", "--name", name, "--network", "pgd-net", "--ip", self.ip(),
                    "-e", f"PGHOST={self.server_ip}", "-e", f"PGPASSWORD={PASSWORD}", "-e", f"PGMODE={mode}",
                    "-e", "PGCA_BASE64=" + base64.b64encode((PREP / "server.crt").read_bytes()).decode(),
                    *[a for pair in env for a in ("-e", pair)])

    def finished(self, name, timeout=300):
        wait_for(lambda: inspect(name).get("state") == "stopped", name + " stopped", timeout=timeout)
        logs = redact(cli("logs", name))
        (WORK / f"{name}.log").write_text(logs + "\n")
        self.remove(name)
        return logs


def locate_disk(name):
    for meta in (WORK / "home").rglob("*.json"):
        disk = meta.parent / "disk.img"
        if disk.exists() and f'"{name}"' in meta.read_text(errors="ignore"):
            return disk
    raise FileNotFoundError(f"disk for volume {name}")


def clone(src, dst):
    run(["cp", "-c", src, dst])


def replay(round_dir, inode, mode, seed=0, compare=None, flush=0):
    out = round_dir / f"{mode}-{seed}.img"
    summary = json.loads(run([sys.executable, REPLAY, "--base", round_dir / "base.img", "--journals",
                              round_dir / "journal", "--inode", inode, "--mode", mode, "--seed", seed, "--flush", flush, "--out", out,
                              *(["--compare", compare] if compare else [])]))
    return out, summary


def server_log_flags(logs):
    return {
        "panic": "PANIC:" in logs,
        "could_not_fsync": "could not fsync" in logs,
        "invalid_page": "invalid page" in logs,
        "checksum_failure": "checksum mismatch" in logs or "checksum verification failed" in logs,
        "redo": "redo starts" in logs,
    }


def recover_model(cluster, label, image_name, acked, strict_client=True):
    outcome = {}
    try:
        cluster.server(image_name, "pgd-data")
    except Exception as error:
        outcome.update(ok=False, failure="server did not become healthy: " + redact(str(error))[:300])
    else:
        verifier = f"pgd-verify-{label}"
        cluster.client(verifier, "recover" if strict_client else "report", f"PGACK={acked}", "PGAMCHECK=1")
        try:
            logs = cluster.finished(verifier)
        except Exception as error:
            logs = ""
            outcome["failure"] = "verifier did not finish: " + redact(str(error))[:300]
        found = re.search(r"RECOVERED acked=(\d+) present=(\d+) max=(\d+) bad_payload=(\d+)", logs)
        if found:
            outcome.update(present=int(found.group(2)), max_id=int(found.group(3)), bad_payload=int(found.group(4)))
        failure = re.search(r"^FAIL (.*)$", logs, re.MULTILINE)
        if failure:
            outcome["failure"] = failure.group(1)[:300]
        outcome["ok"] = "DURABLE_OK" in logs
    try:
        slog = cli("logs", "pgd-server")
        (WORK / f"server-{label}.log").write_text(slog + "\n")
        flags = server_log_flags(slog)
        outcome["server_log"] = flags
        if flags["panic"] or flags["could_not_fsync"] or flags["invalid_page"] or flags["checksum_failure"]:
            outcome["ok"] = False
            outcome.setdefault("failure", "server log contains PANIC/fsync/page/checksum errors")
    except Exception:
        pass
    for vm in list(cluster.active):
        cluster.remove(vm, force=True)
    return outcome


def crash_round(cluster, label, image_name, fresh, disk, *, models, strict_client=True):
    round_dir = WORK / label
    (round_dir / "journal").mkdir(parents=True)
    shutil.copyfile(fresh, disk)
    clone(disk, round_dir / "base.img")
    inode = disk.stat().st_ino
    for stale in JOURNAL.glob("journal-*.bin"):
        stale.unlink()
    target = RNG.randint(150, 600)
    cluster.server(image_name, "pgd-data")
    acker = f"pgd-ack-{label}"
    cluster.client(acker, "ack")
    def acked():
        if max_ack(cli("logs", acker)) >= target:
            return True
        for vm in (acker, "pgd-server"):
            if inspect(vm).get("state") == "stopped":
                raise AssertionError(f"{vm} stopped before ACK {target}:\n{redact(cli('logs', vm))[-2000:]}")
        return False

    wait_for(acked, f"{acker} ACK {target}", timeout=420, interval=0.2)
    acked = max_ack(cli("logs", acker))
    cli("stop", "pgd-server", "--force")
    wait_for(lambda: inspect("pgd-server").get("state") == "stopped", "server killed", timeout=120)
    (WORK / f"server-{label}-before-kill.log").write_text(cli("logs", "pgd-server") + "\n")
    ack_logs = redact(cli("logs", acker))
    (WORK / f"{acker}.log").write_text(ack_logs + "\n")
    for vm in (acker, "pgd-server"):
        cluster.remove(vm, force=True)
    for journal in JOURNAL.glob("journal-*.bin"):
        shutil.move(journal, round_dir / "journal" / journal.name)
    clone(disk, round_dir / "killed.img")
    result = {"label": label, "image": image_name, "target_ack": target, "acked_before_kill": acked,
              "checkpoints_seen": ack_logs.count("CHECKPOINTED"), "killed_sha256": sha256(round_dir / "killed.img"),
              "models": {}}
    full, full_summary = replay(round_dir, inode, "full", compare=round_dir / "killed.img")
    result["journal"] = full_summary
    result["full_sha256"] = sha256(full)
    result["journal_complete"] = full_summary["compare"]["complete"]
    plan = [("full", 0)] + [(m, s) for m in models if m != "boundary" for s in (range(1, SUBSETS + 1) if m == "subset" else [0])]
    for mode, seed in plan:
        image, summary = (full, full_summary) if mode == "full" else replay(round_dir, inode, mode, seed)
        shutil.copyfile(image, disk)
        outcome = recover_model(cluster, f"{label}-{mode}-{seed}", image_name, acked, strict_client)
        outcome["replay"] = summary
        result["models"][f"{mode}-{seed}"] = outcome
        print(f"INFO: {label} {mode}-{seed}: {json.dumps(outcome)}", flush=True)
        if mode != "full":
            image.unlink()
    if "boundary" in models and full_summary["write_bearing_flushes"]:
        candidates = full_summary["write_bearing_flushes"]
        points = sorted(RNG.sample(candidates, min(BOUNDARIES, len(candidates))))
        for point in points:
            image, summary = replay(round_dir, inode, "boundary", seed=point, flush=point)
            shutil.copyfile(image, disk)
            outcome = recover_model(cluster, f"{label}-boundary-{point}", image_name, 0, strict_client)
            outcome["replay"] = summary
            result["models"][f"boundary-{point}"] = outcome
            print(f"INFO: {label} boundary-{point}: {json.dumps(outcome)}", flush=True)
            image.unlink()
    # Keep the images for diagnosis when the journal did not match the killed disk.
    if result["journal_complete"]:
        for img in round_dir.glob("*.img"):
            img.unlink()
    return result


def retain():
    DEST.mkdir(parents=True, exist_ok=True)
    shutil.copytree(WORK, DEST, dirs_exist_ok=True, ignore=shutil.ignore_patterns(
        "home", "*.img", "journal", "journal-*.bin", "*.sock", "project-*", "postgres-client-arm64"))
    (DEST / "RESULT.json").write_text(json.dumps(RESULT, indent=2) + "\n")


def main():
    stage = PREP / "stage"
    inputs = [BIN / "jerboa", BIN / "jerboad", BIN / "tools/kernel.img", FC, REPLAY, FIXTURE / "client.go",
              FIXTURE / "unikernel.toml", stage / "usr/local/pgsql/bin/postgres"]
    RESULT["inputs"] = {str(p): sha256(p) for p in inputs}
    print("Work:", WORK, "evidence:", DEST, "seed:", SEED, flush=True)
    cluster = Cluster()
    try:
        client = WORK / "postgres-client-arm64"
        run([GO, "build", "-trimpath", "-o", client, FIXTURE / "client.go"],
            env=dict(ENV, GOOS="linux", GOARCH="arm64", CGO_ENABLED="0"))
        projects = {}
        for name, fsync in (("pgd-postgres", "on"), ("pgd-nofsync", "off")):
            project = WORK / f"project-{name}"
            project.mkdir()
            toml = (FIXTURE / "unikernel.toml").read_text()
            if fsync == "off":
                toml = toml.replace('"-c", "fsync=on",', '"-c", "fsync=off",')
                assert '"fsync=off"' in toml
            (project / "unikernel.toml").write_text(toml)
            projects[name] = project

        DAEMON.start("journal")
        cli("pkg", "create", "postgresql-arm64:11-pthreads-nanos", stage / "usr/local/pgsql/bin/postgres",
            "--sysroot", stage, "--program-path", "/usr/local/pgsql/bin/postgres", "--platform", "linux/arm64",
            "--description", "Frozen reviewed PostgreSQL fixture for power-loss simulation", *package_maps(stage))
        for name, project in projects.items():
            cli("build", project, "--platform", "linux/arm64", "--name", name)
        cli("build", client, "--platform", "linux/arm64", "--name", "pgd-client")
        cli("network", "create", "pgd-net", "--subnet", f"{SUBNET}.0/24")
        cli("volume", "create", "pgd-data", "--size", "512M", "--seed-pkg", "postgresql-arm64:11-pthreads-nanos",
            "--pkg-source", "jerboa", "--src", "/db", timeout=300)
        disk = locate_disk("pgd-data")

        # SQL-phase tables required by the strict recovery check.
        cluster.server("pgd-postgres:latest", "pgd-data")
        cluster.client("pgd-sql", "write")
        logs = cluster.finished("pgd-sql")
        assert "SQL_OK" in logs and "VALUE=nanos-arm64" in logs, logs
        # Create durability_probe (row 1) in the baseline, so an early flush-boundary
        # image cannot fail recovery merely because the ack table was not yet durable.
        cluster.client("pgd-seed-ack", "ack", "PGACK_MAX=1")
        logs = cluster.finished("pgd-seed-ack")
        assert "ACK 1" in logs and "ACK_DONE" in logs, logs
        cluster.remove("pgd-server")
        fresh = WORK / "fresh.img"
        clone(disk, fresh)
        check("SQL phase on the journal VMM; baseline volume captured")

        for number in range(1, ROUNDS + 1):
            r = crash_round(cluster, f"round{number}", "pgd-postgres:latest", fresh, disk,
                            models=("durable", "subset", "boundary"))
            RESULT["rounds"].append(r)
            assert r["journal_complete"], f"{r['label']}: replayed full image differs from killed disk"
            assert r["journal"]["flushes"] > 0, r
            for model, outcome in r["models"].items():
                assert outcome.get("ok"), f"{r['label']} {model}: {outcome}"
            boundaries = sum(1 for model in r["models"] if model.startswith("boundary-"))
            check(f"{r['label']}: {r['acked_before_kill']} acknowledged commits survive full, durable and "
                  f"{SUBSETS} partial-writeback models with checksums, heap/index and amcheck; "
                  f"{boundaries} earlier flush-boundary cuts recover consistently")

        r = crash_round(cluster, "nofsync", "pgd-nofsync:latest", fresh, disk, models=("durable",), strict_client=True)
        RESULT["controls"]["fsync_off"] = r
        assert r["journal_complete"], r
        assert not r["models"]["durable-0"].get("ok"), f"fsync=off durable model did not detect loss: {r}"
        check("negative control fsync=off detected by durable model (full model recorded, not asserted)")

        DAEMON.stop()
        DAEMON.start("flush-lie", HVF_CRASH_JOURNAL_LIE="1")
        r = crash_round(cluster, "flushlie", "pgd-postgres:latest", fresh, disk, models=("durable",))
        RESULT["controls"]["flush_lie"] = r
        assert r["journal_complete"] and r["journal"]["flushes"] == 0, r
        assert r["models"]["full-0"].get("ok"), f"flush-lie full model should recover like a VMM kill: {r}"
        assert not r["models"]["durable-0"].get("ok"), f"flush-lie durable model did not detect loss: {r}"
        check("negative control flush-lie: invisible to VMM kill, detected by durable model")
        RESULT["status"] = "PASS"
        print("PASS: PostgreSQL simulated power-loss durability", flush=True)
    except Exception as error:
        RESULT["status"] = "FAIL"
        RESULT["error"] = redact(str(error))
        raise
    finally:
        for vm in reversed(cluster.active):
            try:
                cluster.remove(vm, force=True)
            except Exception:
                pass
        for command in (("volume", "rm", "pgd-data"), ("network", "rm", "pgd-net")):
            cli(*command, success=False)
        DAEMON.stop()
        TRANSCRIPT.close()
        retain()


if __name__ == "__main__":
    main()
