#!/usr/bin/env python3
"""Run the reported workloads against an isolated daemon and local packages.

Requires sysbench:1.0.20, iperf3:3.18 and fio-noshm:3.39 packages for the
native guest architecture. Linux requires permission to create TAP networks.
Each case must finish and satisfy its result checks; partial results fail.
"""
import argparse
import hashlib
import http.client
import tempfile
import re
import statistics
import json
import os
from pathlib import Path
import platform
import shutil
import socket
import subprocess
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("bin", "tools", "firecracker", "packages", "work"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--cases", default="memory,network,disk")
    parser.add_argument("--redis-benchmark", default="redis-benchmark", help="host client executable for the optional redis case")
    parser.add_argument("--docker-image", help="prepared native image for matching memory/startup cases")
    parser.add_argument("--startup-repetitions", type=int, default=10)
    parser.add_argument("--network-seconds", type=int, default=20)
    parser.add_argument("--udp-rates", default="1G", help="comma-separated iperf offered rates")
    parser.add_argument("--network-reverse", action="store_true", help="also measure reverse TCP/UDP")
    a = parser.parse_args()
    cases = a.cases.split(",")
    if set(cases) - {"memory", "network", "disk", "startup", "redis"}:
        parser.error("unknown case")
    if a.network_seconds < 2 or not all(re.fullmatch(r"[1-9][0-9]*[KMG]?", r) for r in a.udp_rates.split(",")):
        parser.error("invalid network duration or UDP rates")
    if a.startup_repetitions < 2:
        parser.error("startup repetitions must be at least two")
    if a.docker_image:
        subprocess.run(["docker", "image", "inspect", a.docker_image], check=True, stdout=subprocess.DEVNULL)
    required_tools = ["kernel.img", "mkfs", "boot.img"]
    for name in required_tools:
        if not (a.tools / name).is_file():
            parser.error("incomplete local toolchain: " + str(a.tools / name))
    a.work = a.work.resolve()
    a.work.mkdir(parents=True, exist_ok=False)
    kernel = a.work / "kernel.img"
    shutil.copyfile(a.tools / "kernel.img", kernel)
    home = a.work / "home"
    for name in ("sysbench", "iperf3", "fio-noshm") + (("redis",) if "redis" in cases else ()):
        shutil.copytree(a.packages / name, home / ".jerboa/packages" / name)
    arch = "arm64" if platform.machine() in ("arm64", "aarch64") else "amd64"
    endpoint = "unix://" + str(a.work / "api.sock")
    env = dict(os.environ, HOME=str(home), JERBOA_HOST=endpoint, JERBOA_AUTH_TOKEN="isolated-regression")
    active = []
    network = None
    results = {"metadata": {"platform": platform.platform(),
                           "kernel_sha256": hashlib.sha256(kernel.read_bytes()).hexdigest(),
                           "cases": cases, "docker_image": a.docker_image,
                           "firecracker_sha256": hashlib.sha256(a.firecracker.read_bytes()).hexdigest(),
                           "network_seconds": a.network_seconds,
                           "udp_rates": a.udp_rates.split(",")}}
    samples = []
    last_sample = 0.0

    def sample_network():
        nonlocal last_sample
        now = time.monotonic()
        if platform.system() != "Darwin" or "network" not in cases or now - last_sample < 1:
            return
        last_sample = now
        for vm in active:
            conn = http.client.HTTPConnection("localhost", timeout=1)
            sock = socket.socket(socket.AF_UNIX)
            sock.settimeout(1)
            try:
                sock.connect(str(Path(tempfile.gettempdir()) / ("jerboa-run-" + str(os.getuid())) / ("fc-" + vm + ".sock")))
                conn.sock = sock
                conn.request("GET", "/metrics")
                response = conn.getresponse()
                if response.status == 200:
                    samples.append({"monotonic_seconds": now, "vm": vm,
                                    "metrics": json.loads(response.read())})
            except (OSError, ValueError, http.client.HTTPException):
                pass  # The client can exit between inspect and sampling.
            finally:
                conn.close()
                sock.close()


    def cli(*args):
        p = subprocess.run([str(a.bin.resolve() / "jerboa"), *args], env=env,
                           capture_output=True, text=True, timeout=180)
        if p.returncode:
            raise RuntimeError(f"{args}: {p.stdout}\n{p.stderr}")
        return p.stdout.strip()

    def build(name, package, program, args):
        project = a.work / name
        project.mkdir()
        config = (f'[build]\nlang="raw"\npkgs=["{package}"]\npkg_source="jerboa"\n'
                  'disk_size="64M"\ndirs=["/data", "/tmp"]\n'
                  f'[program]\npath={json.dumps(program)}\nargs={json.dumps(args)}\n')
        (project / "unikernel.toml").write_text(config)
        cli("build", str(project), "--platform", "linux/" + arch, "--name", name)

    def start(name, *flags):
        vm = cli("run", name + ":latest", *flags)
        active.append(vm)
        return vm

    def remove(vm):
        if json.loads(cli("inspect", vm))["state"] != "stopped":
            cli("stop", vm)
        cli("rm", vm)
        active.remove(vm)

    def finish(name, vm, marker=None, json_result=False, timeout=360):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            sample_network()
            info = json.loads(cli("inspect", vm))
            if info["state"] == "stopped":
                break
            time.sleep(.2)
        output = cli("logs", vm)
        (a.work / (name + ".log")).write_text(output)
        if info["state"] != "stopped":
            raise RuntimeError(name + " timed out")
        if marker and marker not in output:
            raise RuntimeError(name + " did not complete: " + output[-2000:])
        if json_result:
            # Serial logs may surround the application's single JSON document.
            try:
                data = json.JSONDecoder().raw_decode(output[output.index("{\n"):])[0]
            except (ValueError, json.JSONDecodeError) as error:
                raise RuntimeError(f"{name}: missing complete JSON; see {a.work / (name + '.log')}") from error
            if "error" in data:
                raise RuntimeError(name + ": " + str(data["error"]))
            if name.startswith("fio"):
                assert len(data["jobs"]) == 4, data
                assert all(j["error"] == 0 for j in data["jobs"]), data
                direction = "read" if "read" in name else "write"
                assert all(j[direction]["runtime"] >= 59000 for j in data["jobs"]), data
            else:
                assert data["end"], data
                streams = data["end"]["streams"]
                assert streams, data
                assert data["intervals"][-1]["sum"]["end"] >= a.network_seconds - 1, data
                received = data["end"]["sum_received"]
                assert received["seconds"] >= a.network_seconds - 1 and received["bytes"] > 0, data
                if data["start"]["test_start"]["protocol"] == "UDP":
                    assert "lost_percent" in received and "packets" in received, data
            results[name] = data
        else:
            results[name] = {"completed": True}
            bandwidth = re.search(r"\(([0-9.]+) MiB/sec\)", output)
            if bandwidth:
                results[name]["mib_per_second"] = float(bandwidth.group(1))
        remove(vm)
        if json_result and name.startswith("iperf-"):
            received = results[name]["end"]["sum_received"]
            loss = received.get("lost_percent")
            detail = f", loss={loss:.4f}%" if loss is not None else ""
            stalls = sum(i["sum"]["bytes"] == 0 for i in results[name]["intervals"])
            print(f"{name} COMPLETED: received={received['bits_per_second'] / 1e6:.2f} Mbit/s"
                  f"{detail}, zero-progress intervals={stalls}", flush=True)
        else:
            print(name + " PASS", flush=True)

    daemon_log = (a.work / "daemon.log").open("w")
    daemon = subprocess.Popen([str(a.bin.resolve() / "jerboad"), "--host", endpoint,
                               "--tools-dir", str(a.tools.resolve()), "--hypervisor", "firecracker",
                               "--fc-bin", str(a.firecracker.resolve()), "--fc-kernel", str(kernel)],
                              env=env, stdout=daemon_log, stderr=daemon_log)
    try:
        for _ in range(100):
            if (a.work / "api.sock").exists():
                break
            if daemon.poll() is not None:
                raise RuntimeError("daemon failed: " + (a.work / "daemon.log").read_text())
            time.sleep(.1)
        if "memory" in cases:
            for block, access in (("1K", "seq"), ("1K", "rnd"), ("1M", "seq")):
                name = f"memory-{block.lower()}-{access}"
                args = ["memory", "--threads=1", "--memory-block-size=" + block,
                        "--memory-access-mode=" + access, "--memory-total-size=10T", "--time=10", "run"]
                build(name, "sysbench:1.0.20", "/usr/bin/sysbench", args)
                finish(name, start(name, "--cpus", "1", "--memory", "512M"), marker="Threads fairness:")
                if a.docker_image:
                    output = subprocess.check_output(["docker", "run", "--rm", "--pull=never", "--cpus=1",
                              "--memory=512m", a.docker_image, "/usr/bin/sysbench", *args], text=True, timeout=60)
                    (a.work / ("docker-" + name + ".log")).write_text(output)
                    if "Threads fairness:" not in output:
                        raise RuntimeError("Docker memory case did not finish")
                    match = re.search(r"\(([0-9.]+) MiB/sec\)", output)
                    results["docker-" + name] = {"mib_per_second": float(match.group(1))}
        if "startup" in cases:
            build("startup", "sysbench:1.0.20", "/usr/bin/sysbench", ["--version"])
            commands = {"jerboa": [str(a.bin.resolve() / "jerboa"), "run", "startup:latest",
                                   "--cpus", "1", "--memory", "512M", "--attach", "--rm"]}
            if a.docker_image:
                commands["docker"] = ["docker", "run", "--rm", "--pull=never", "--cpus=1",
                                       "--memory=512m", a.docker_image, "/usr/bin/sysbench", "--version"]
            for runtime, command in commands.items():
                samples = []
                for _ in range(a.startup_repetitions + 1):
                    begin = time.perf_counter()
                    result = subprocess.run(command, env=env if runtime == "jerboa" else None,
                                            capture_output=True, text=True, timeout=60, check=True)
                    samples.append((time.perf_counter() - begin) * 1000)
                    if "sysbench" not in result.stdout:
                        raise RuntimeError(runtime + " startup produced no application output")
                results[runtime + "-startup"] = {"prepared_first_ms": samples[0], "warm_ms": samples[1:],
                                                  "warm_median_ms": statistics.median(samples[1:])}
        if "network" in cases:
            build("iperf-server", "iperf3:3.18", "/usr/bin/iperf3", ["-s"])
            network = "reg-" + str(os.getpid())
            cli("network", "create", network, "--subnet", "172.30.79.0/24")
            flags = ["--network", network, "--cpus", "1", "--memory", "512M"]
            port = 5201
            server = start("iperf-server", "--name", "iperf-regression", *flags)
            address = json.loads(cli("inspect", server))["ip_address"]
            time.sleep(2)
            for reverse in ([False, True] if a.network_reverse else [False]):
                workloads = [("tcp", None)] + [("udp", rate) for rate in a.udp_rates.split(",")]
                for protocol, rate in workloads:
                    name = "iperf-" + protocol
                    if rate and a.udp_rates != "1G":
                        name += "-" + rate.lower()
                    if reverse:
                        name += "-reverse"
                    args = ["-c", address, "-p", str(port), "-t", str(a.network_seconds), "-J"]
                    if rate:
                        args += ["-u", "-b", rate]
                    if reverse:
                        args += ["-R"]
                    build(name, "iperf3:3.18", "/usr/bin/iperf3", args)
                    finish(name, start(name, *flags), json_result=True,
                           timeout=max(360, a.network_seconds + 120))
                    sample_network()
                    (a.work / "network-metrics.json").write_text(json.dumps(samples, indent=2))
            remove(server)
        if "redis" in cases:
            build("redis-server", "redis:8.0.2", "/usr/bin/redis-server",
                  ["--bind", "0.0.0.0", "--protected-mode", "no", "--save", "",
                   "--appendonly", "no", "--ignore-warnings", "ARM64-COW-BUG"])
            with socket.socket() as reservation:
                reservation.bind(("127.0.0.1", 0))
                port = reservation.getsockname()[1]
            if network is None:
                network = "reg-" + str(os.getpid())
                cli("network", "create", network, "--subnet", "172.30.79.0/24")
            vm = start("redis-server", "--cpus", "4", "--memory", "512M", "--network", network,
                       "-p", f"127.0.0.1:{port}:6379")
            for _ in range(100):
                try:
                    with socket.create_connection(("127.0.0.1", port), timeout=.5) as connection:
                        connection.sendall(b"*1\r\n$4\r\nPING\r\n")
                        if connection.recv(64).startswith(b"+PONG"):
                            break
                except OSError:
                    pass
                time.sleep(.1)
            else:
                raise RuntimeError("Redis did not become ready: " + cli("logs", vm))
            command = [a.redis_benchmark, "-h", "127.0.0.1", "-p", str(port),
                       "-n", "100000", "-c", "50", "-t", "set"]
            benchmark = subprocess.run(command, capture_output=True, text=True, timeout=120)
            output = benchmark.stdout
            (a.work / "redis-set.log").write_text(output + benchmark.stderr)
            (a.work / "redis-server.log").write_text(cli("logs", vm))
            benchmark.check_returncode()
            if "100000 requests completed" not in output:
                raise RuntimeError("Redis did not complete all requests")
            results["redis-set"] = {"completed": True, "requests": 100000, "command": command}
            remove(vm)
            print("redis-set PASS", flush=True)
        if "disk" in cases:
            for direction in ("randread", "randwrite"):
                name = "fio-" + direction
                cli("volume", "create", name, "--size", "2G")
                build(name, "fio-noshm:3.39", "/usr/local/bin/fio-noshm", ["--name=" + direction,
                      "--rw=" + direction, "--thread", "--ioengine=libaio", "--bs=4k", "--numjobs=4",
                      "--size=256M", "--time_based", "--runtime=60", "--directory=/data", "--output-format=json"])
                finish(name, start(name, "--cpus", "4", "--memory", "256M", "-v", name + ":/data"), json_result=True)
                cli("volume", "rm", name)
        (a.work / "results.json").write_text(json.dumps(results, indent=2))
    finally:
        if samples:
            (a.work / "network-metrics.json").write_text(json.dumps(samples, indent=2))
        for vm in active[:]:
            try:
                (a.work / (vm + ".log")).write_text(cli("logs", vm))
                remove(vm)
            except Exception as error:
                print("cleanup:", error, flush=True)
        if network:
            try:
                cli("network", "rm", network)
            except Exception as error:
                print("network cleanup:", error, flush=True)
        daemon.terminate()
        try:
            daemon.wait(timeout=10)
        except subprocess.TimeoutExpired:
            daemon.kill()
            daemon.wait()
        daemon_log.close()


if __name__ == "__main__":
    main()
