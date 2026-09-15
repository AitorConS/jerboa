#!/usr/bin/env python3
"""Isolated macOS/HVF acceptance for Nanos graceful and forced shutdown."""
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_ART = ROOT / "dist/sol-closure/shutdown"
BIN = Path(os.getenv("JERBOA_SHUTDOWN_BIN_DIR", str(DEFAULT_ART)))
NEW_TOOLS = Path(os.getenv("JERBOA_SHUTDOWN_TOOLS", str(DEFAULT_ART / "tools")))
OLD_TOOLS = Path(os.getenv("JERBOA_SHUTDOWN_OLD_TOOLS", str(ROOT / "dist/macos-arm64/tools")))
FC = Path(os.getenv("JERBOA_SHUTDOWN_FC", str(DEFAULT_ART / "vmm/firecracker")))
EVIDENCE = Path(os.getenv("JERBOA_SHUTDOWN_EVIDENCE_DIR", str(DEFAULT_ART)))
PROGRAM = r'''package main
import("encoding/json";"fmt";"net/http";"os";"os/signal";"runtime";"syscall")
var held *os.File
func main(){
 if os.Getenv("IGNORE_SIGTERM")=="1" {signals:=make(chan os.Signal,1);signal.Notify(signals,syscall.SIGTERM);go func(){for range signals{fmt.Println("APP_SIGTERM_IGNORED")}}()}
 path:="/data/kernel-sync-marker"; marker:="persisted-by-kernel-storage-sync"
 b,err:=os.ReadFile(path); state:=string(b)
 if err!=nil {held,err=os.OpenFile(path,os.O_CREATE|os.O_WRONLY|os.O_TRUNC,0600);if err!=nil{panic(err)};if _,err=held.WriteString(marker);err!=nil{panic(err)};state="written-unsynced-open"}
 http.HandleFunc("/",func(w http.ResponseWriter,r *http.Request){json.NewEncoder(w).Encode(map[string]any{"state":state,"arch":runtime.GOARCH})})
 fmt.Println("GUEST_READY");panic(http.ListenAndServe(":8080",nil))
}'''


def main():
    if os.uname().sysname != "Darwin" or os.uname().machine != "arm64":
        raise SystemExit("requires macOS arm64")
    go = ROOT.parent / "firecracker-macos/experiments/hvf/build/guest-tools/go/bin/go"
    if not (go.is_file() and FC.is_file()):
        raise SystemExit("reference Go or signed Firecracker missing")
    EVIDENCE.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="jsh-", dir="/tmp") as td:
        work = Path(td)
        home = work / "home"
        home.mkdir()
        project = work / "app"
        project.mkdir()
        (project / "main.go").write_text(PROGRAM)
        (project / "go.mod").write_text("module shutdown-acceptance\n\ngo 1.25\n")
        (project / "unikernel.toml").write_text('[build]\nlang="go"\ndirs=["/data"]\n')
        evidence = EVIDENCE / "acceptance-evidence.json"
        result = {"workspace": str(work), "firecracker": str(FC), "events": []}
        env = dict(os.environ, HOME=str(home), JERBOA_AUTH_TOKEN="shutdown-isolated")
        env["PATH"] = str(go.parent) + os.pathsep + env.get("PATH", "")
        env["GOCACHE"] = subprocess.check_output([str(go), "env", "GOCACHE"], text=True).strip()
        env["GOMODCACHE"] = subprocess.check_output([str(go), "env", "GOMODCACHE"], text=True).strip()

        daemon = None
        log_handle = None

        def start_daemon(tools, name):
            nonlocal daemon, log_handle
            stop_daemon()
            sock = work / f"{name}.sock"
            env["JERBOA_HOST"] = "unix://" + str(sock)
            log_handle = (EVIDENCE / f"daemon-{name}.log").open("w")
            daemon = subprocess.Popen([str(BIN / "jerboad"), "--host", env["JERBOA_HOST"],
                "--tools-dir", str(tools), "--hypervisor", "firecracker", "--fc-bin", str(FC)],
                env=env, stdout=log_handle, stderr=log_handle)
            wait_for(lambda: sock.exists(), f"daemon {name}", 10)

        def stop_daemon():
            nonlocal daemon, log_handle
            if daemon is not None and daemon.poll() is None:
                daemon.terminate()
                try:
                    daemon.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    daemon.kill()
                    daemon.wait(timeout=5)
            daemon = None
            if log_handle is not None:
                log_handle.close()
                log_handle = None

        def cli(*args, timeout=180):
            run = subprocess.run([str(BIN / "jerboa"), *args], env=env, text=True,
                                 capture_output=True, timeout=timeout)
            if run.returncode:
                raise RuntimeError(f"{args}: {run.stdout}\n{run.stderr}")
            return run.stdout.strip()

        def wait_for(check, label, timeout=45):
            deadline = time.monotonic() + timeout
            while time.monotonic() < deadline:
                try:
                    value = check()
                    if value:
                        return value
                except (OSError, ValueError, RuntimeError):
                    pass
                time.sleep(.1)
            raise RuntimeError("timed out: " + label)

        def free_port():
            with socket.socket() as s:
                s.bind(("127.0.0.1", 0))
                return s.getsockname()[1]

        def http_json(port):
            with urllib.request.urlopen(f"http://127.0.0.1:{port}", timeout=1) as response:
                return json.load(response)

        def fc_state(vm_id):
            path = Path(tempfile.gettempdir()) / f"jerboa-run-{os.getuid()}" / f"fc-{vm_id}.sock"
            with socket.socket(socket.AF_UNIX) as client:
                client.settimeout(1)
                client.connect(str(path))
                client.sendall(b"GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
                data = b""
                while chunk := client.recv(65536):
                    data += chunk
            return json.loads(data.split(b"\r\n\r\n", 1)[1])

        active = []
        try:
            start_daemon(NEW_TOOLS, "cooperative")
            cli("build", str(project), "--platform", "linux/arm64", "--name", "shutdown-proof")
            cli("volume", "create", "shutdown-data", "--size", "32M")
            port = free_port()
            vm = cli("run", "shutdown-proof:latest", "--name", "cooperative", "-p",
                     f"127.0.0.1:{port}:8080", "-v", "shutdown-data:/data", "--restart", "on-failure:1")
            active.append(vm)
            def cooperative_ready():
                info = json.loads(cli("inspect", vm))
                if info["state"] == "stopped":
                    guest_logs = cli("logs", vm)
                    (EVIDENCE / "cooperative-crash.log").write_text(guest_logs + "\n")
                    raise AssertionError(guest_logs)
                return http_json(port)
            first = wait_for(cooperative_ready, "cooperative guest ready")
            assert first == {"state": "written-unsynced-open", "arch": "arm64"}, first
            boot_logs = cli("logs", vm)
            (EVIDENCE / "cooperative-boot.log").write_text(boot_logs + "\n")
            assert "virtio input: power button attached" in boot_logs, boot_logs

            begin = time.monotonic()
            stop = subprocess.Popen([str(BIN / "jerboa"), "stop", vm], env=env,
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            observed = []
            while stop.poll() is None:
                try:
                    state = fc_state(vm)
                    observed.append({"state": state.get("state"),
                                     "shutdown_delivered": state.get("shutdown_delivered"),
                                     "last_error": state.get("last_error")})
                except OSError:
                    pass
                time.sleep(.1)
            stdout, stderr = stop.communicate()
            duration = time.monotonic() - begin
            assert stop.returncode == 0, (stdout, stderr)
            info = json.loads(cli("inspect", vm))
            assert info["state"] == "stopped" and info.get("restart_count", 0) == 0, info
            logs = cli("logs", vm)
            assert "virtio input: power button shutdown requested" in logs, logs
            (EVIDENCE / "cooperative-states.json").write_text(json.dumps(observed, indent=2) + "\n")
            control_ack_observed = any(x["shutdown_delivered"] for x in observed)
            time.sleep(2)
            assert json.loads(cli("inspect", vm))["state"] == "stopped"
            result["events"].append({"case": "cooperative", "seconds": duration,
                "control_ack_observed": control_ack_observed, "guest_log_marker": True,
                "state": "stopped", "restart_count": info.get("restart_count", 0)})
            cli("rm", vm)
            active.remove(vm)

            port = free_port()
            vm2 = cli("run", "shutdown-proof:latest", "--name", "persistence-reboot", "-p",
                      f"127.0.0.1:{port}:8080", "-v", "shutdown-data:/data")
            active.append(vm2)
            persisted = wait_for(lambda: http_json(port), "persisted data after reboot")
            assert persisted == {"state": "persisted-by-kernel-storage-sync", "arch": "arm64"}, persisted
            begin = time.monotonic()
            cli("stop", "--force", vm2, timeout=10)
            kill_duration = time.monotonic() - begin
            wait_for(lambda: json.loads(cli("inspect", vm2))["state"] == "stopped", "kill state", 5)
            assert kill_duration < 5, kill_duration
            result["events"].append({"case": "persistence-and-kill", "persisted": True,
                "kill_seconds": kill_duration, "state": "stopped"})
            cli("rm", vm2)
            active.remove(vm2)

            cli("volume", "create", "stubborn-data", "--size", "32M")
            port = free_port()
            vm_stubborn = cli("run", "shutdown-proof:latest", "--name", "stubborn", "-p",
                f"127.0.0.1:{port}:8080", "-v", "stubborn-data:/data", "-e", "IGNORE_SIGTERM=1")
            active.append(vm_stubborn)
            assert wait_for(lambda: http_json(port), "stubborn guest ready")["state"] == "written-unsynced-open"
            begin = time.monotonic()
            cli("stop", vm_stubborn, timeout=45)
            stubborn_duration = time.monotonic() - begin
            stubborn_logs = cli("logs", vm_stubborn)
            (EVIDENCE / "stubborn-guest.log").write_text(stubborn_logs + "\n")
            assert "virtio input: power button shutdown requested" in stubborn_logs
            assert "APP_SIGTERM_IGNORED" in stubborn_logs
            assert 25 <= stubborn_duration < 35, stubborn_duration
            assert json.loads(cli("inspect", vm_stubborn))["state"] == "stopped"
            cli("rm", vm_stubborn)
            active.remove(vm_stubborn)

            port = free_port()
            vm_stubborn_reboot = cli("run", "shutdown-proof:latest", "--name", "stubborn-reboot", "-p",
                f"127.0.0.1:{port}:8080", "-v", "stubborn-data:/data")
            active.append(vm_stubborn_reboot)
            stubborn_persisted = wait_for(lambda: http_json(port), "stubborn persisted data")
            assert stubborn_persisted["state"] == "persisted-by-kernel-storage-sync", stubborn_persisted
            cli("stop", "--force", vm_stubborn_reboot, timeout=10)
            wait_for(lambda: json.loads(cli("inspect", vm_stubborn_reboot))["state"] == "stopped",
                     "stubborn reboot kill state", 5)
            cli("rm", vm_stubborn_reboot)
            active.remove(vm_stubborn_reboot)
            result["events"].append({"case": "sigterm-ignored-kernel-sync", "seconds": stubborn_duration,
                "guest_request": True, "app_sigterm_ignored": True, "persisted_after_reboot": True,
                "state": "stopped"})

            start_daemon(OLD_TOOLS, "noncooperative")
            vm3 = cli("run", "shutdown-proof:latest", "--name", "noncooperative")
            active.append(vm3)
            wait_for(lambda: "GUEST_READY" in cli("logs", vm3), "old-kernel guest ready")
            begin = time.monotonic()
            cli("stop", vm3, timeout=50)
            forced_duration = time.monotonic() - begin
            info = json.loads(cli("inspect", vm3))
            assert info["state"] == "stopped", info
            assert 30 <= forced_duration < 45, forced_duration
            assert "virtio input: power button shutdown requested" not in cli("logs", vm3)
            result["events"].append({"case": "noncooperative-timeout", "seconds": forced_duration,
                "state": "stopped", "guest_log_marker": False, "expected": "forced after 35s"})
            cli("rm", vm3)
            active.remove(vm3)
            evidence.write_text(json.dumps(result, indent=2) + "\n")
            print(json.dumps(result, indent=2))
        finally:
            for vm_id in active:
                try:
                    cli("stop", "--force", vm_id, timeout=10)
                except Exception:
                    pass
            stop_daemon()


if __name__ == "__main__":
    main()
