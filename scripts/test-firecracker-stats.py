#!/usr/bin/env python3
"""Isolated, reproducible macOS Firecracker process-tree stats acceptance."""
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
OUT = Path(os.getenv("JERBOA_TEST_OUTPUT", str(ROOT / "dist/sol-closure/stats/evidence"))).resolve()
BIN = Path(os.getenv("JERBOA_TEST_BIN", str(ROOT / "dist/sol-closure/stats/bin"))).resolve()
FC = Path(os.getenv("JERBOA_FIRECRACKER_BIN", str(ROOT.parent / "firecracker-macos/experiments/hvf/build/astra-shared-network/review-fixes/package/bin/firecracker"))).resolve()
GO = Path(os.getenv("JERBOA_TEST_GO", str(ROOT.parent / "firecracker-macos/experiments/hvf/build/guest-tools/go/bin/go"))).resolve()
PROGRAM = r'''package main
import("fmt";"net/http";"os";"runtime";"strconv";"sync";"time")
var(mu sync.Mutex; held [][]byte)
func main(){
 http.HandleFunc("/",func(w http.ResponseWriter,r *http.Request){fmt.Fprintf(w,"ok %d",runtime.NumCPU())})
 http.HandleFunc("/ram",func(w http.ResponseWriter,r *http.Request){
  mib,_:=strconv.Atoi(r.URL.Query().Get("mib"));b:=make([]byte,mib<<20)
  for i:=0;i<len(b);i+=4096{b[i]=byte(i)};mu.Lock();held=append(held,b);mu.Unlock();fmt.Fprint(w,"held")})
 http.HandleFunc("/cpu",func(w http.ResponseWriter,r *http.Request){
  ms,_:=strconv.Atoi(r.URL.Query().Get("ms"));for i:=0;i<runtime.NumCPU();i++{go func(){end:=time.Now().Add(time.Duration(ms)*time.Millisecond);x:=uint64(1);for time.Now().Before(end){x=x*1664525+1013904223};runtime.KeepAlive(x)}()};fmt.Fprint(w,"busy")})
 http.HandleFunc("/fail",func(w http.ResponseWriter,r *http.Request){fmt.Fprint(w,"failing");go func(){time.Sleep(100*time.Millisecond);os.Exit(7)}()})
 fmt.Println("STATS_GUEST_READY");panic(http.ListenAndServe(":8080",nil))
}'''


def main():
    if os.uname().sysname != "Darwin" or os.uname().machine != "arm64":
        raise SystemExit("requires macOS arm64")
    required = [FC, GO, BIN / "jerboa", BIN / "jerboad", BIN / "tools/kernel.img"]
    if missing := [str(path) for path in required if not path.is_file()]:
        raise SystemExit("required test input missing: " + ", ".join(missing))
    OUT.mkdir(parents=True, exist_ok=True)
    results = {"inputs": {"bin": str(BIN), "firecracker": str(FC), "go": str(GO), "output": str(OUT)}, "semantics": {
        "cpu_pct": "sum of host CPU time deltas for the live supervisor/VMM descendant tree divided by wall time; 100% is one logical host CPU",
        "mem_bytes": "sum of current host resident bytes (RSS) for the supervisor/VMM descendant tree; not configured guest RAM and may include resident guest-memory mappings",
        "disk_bytes": "sum of cumulative host process disk I/O bytes for the currently live tree"
    }}
    with tempfile.TemporaryDirectory(prefix="jerboa-stats-", dir="/tmp") as td:
        work = Path(td)
        home = work / "home"
        app = work / "app"
        home.mkdir(); app.mkdir()
        (app / "main.go").write_text(PROGRAM)
        (app / "go.mod").write_text("module stats-guest\n\ngo 1.25\n")
        (app / "unikernel.toml").write_text('[build]\nlang="go"\n')
        sock = work / "daemon.sock"
        env = dict(os.environ, HOME=str(home), JERBOA_HOST="unix://" + str(sock), JERBOA_AUTH_TOKEN="stats-isolated")
        env["PATH"] = str(GO.parent) + os.pathsep + env.get("PATH", "")
        env["GOCACHE"] = subprocess.check_output([str(GO), "env", "GOCACHE"], env=os.environ, text=True).strip()
        env["GOMODCACHE"] = subprocess.check_output([str(GO), "env", "GOMODCACHE"], env=os.environ, text=True).strip()
        log = (OUT / "daemon.log").open("w")

        def cli(*args):
            p = subprocess.run([str(BIN / "jerboa"), "--output", "json", *args], env=env, text=True, capture_output=True, timeout=180)
            if p.returncode:
                raise RuntimeError(f"cli {args}: {p.stdout}\n{p.stderr}")
            return p.stdout.strip()

        def inspect(vm): return json.loads(cli("inspect", vm))
        def stats(vm): return json.loads(cli("stats", vm))
        def wait(check, label, timeout=50):
            end = time.monotonic() + timeout
            while time.monotonic() < end:
                try:
                    value = check()
                    if value: return value
                except Exception:
                    pass
                time.sleep(.1)
            raise RuntimeError("timeout: " + label)

        def start_daemon():
            return subprocess.Popen([str(BIN / "jerboad"), "--host", env["JERBOA_HOST"], "--tools-dir", str(BIN / "tools"),
                                     "--hypervisor", "firecracker", "--fc-bin", str(FC)], env=env, stdout=log, stderr=log)

        def host_tree(vm):
            actual_id = inspect(vm)["id"]
            rows = []
            output = subprocess.check_output(["/bin/ps", "-axo", "pid=,ppid=,rss=,%cpu=,command="], text=True)
            for line in output.splitlines():
                fields = line.strip().split(None, 4)
                if len(fields) == 5:
                    rows.append({"pid": int(fields[0]), "ppid": int(fields[1]), "rss_bytes": int(fields[2]) * 1024,
                                 "cpu_pct": float(fields[3]), "command": fields[4]})
            roots = [r for r in rows if "--api-sock " in r["command"] and ("fc-" + actual_id + ".sock") in r["command"]]
            if len(roots) != 1:
                raise RuntimeError(f"expected one supervisor, got {roots}")
            chosen = {roots[0]["pid"]}
            changed = True
            while changed:
                changed = False
                for row in rows:
                    if row["ppid"] in chosen and row["pid"] not in chosen:
                        chosen.add(row["pid"]); changed = True
            tree = [r for r in rows if r["pid"] in chosen]
            return {"pids": sorted(chosen), "rss_bytes": sum(r["rss_bytes"] for r in tree),
                    "cpu_pct": sum(r["cpu_pct"] for r in tree), "rows": tree}

        daemon = start_daemon()
        vm = None
        try:
            wait(sock.exists, "daemon socket")
            cli("build", str(app), "--platform", "linux/arm64", "--name", "stats-load")
            with socket.socket() as listener:
                listener.bind(("127.0.0.1", 0)); port = listener.getsockname()[1]
            cli("run", "stats-load:latest", "--name", "stats-vm", "--memory", "512M", "--cpus", "4", "-p", f"127.0.0.1:{port}:8080", "--restart", "on-failure:1")
            vm = "stats-vm"
            url = f"http://127.0.0.1:{port}"
            wait(lambda: urllib.request.urlopen(url, timeout=1).read().startswith(b"ok"), "guest HTTP")

            stats(vm)  # establish the persistent CPU baseline
            time.sleep(.5)
            idle = stats(vm); idle_host = host_tree(vm)
            if idle["source"] != "darwin-libproc-tree" or len(idle_host["pids"]) < 3:
                raise RuntimeError(f"not a real VMM process tree: {idle} {idle_host}")
            with urllib.request.urlopen(url + "/ram?mib=192", timeout=10) as response: response.read()
            time.sleep(.5)
            ram = stats(vm); ram_host = host_tree(vm)
            if ram["mem_bytes"] <= idle["mem_bytes"] + 128 * 1024 * 1024:
                raise RuntimeError(f"guest memory pressure not reflected: idle={idle} ram={ram}")
            tolerance = max(16 * 1024 * 1024, int(ram_host["rss_bytes"] * .12))
            if abs(ram["mem_bytes"] - ram_host["rss_bytes"]) > tolerance:
                raise RuntimeError(f"RSS disagrees with ps: api={ram} ps={ram_host}")

            with urllib.request.urlopen(url + "/cpu?ms=4000", timeout=2) as response: response.read()
            cpu_samples = []
            for _ in range(5):
                time.sleep(.5)
                cpu_samples.append({"api": stats(vm), "ps": host_tree(vm)})
            peak = max(x["api"]["cpu_pct"] for x in cpu_samples)
            ps_peak = max(x["ps"]["cpu_pct"] for x in cpu_samples)
            supervisor_peak = max(x["ps"]["rows"][0]["cpu_pct"] for x in cpu_samples)
            if peak < 50 or ps_peak < 50 or peak <= supervisor_peak:
                raise RuntimeError(f"CPU load not reflected by tree: api={peak} ps={ps_peak} supervisor={supervisor_peak}")

            old_tree = host_tree(vm)
            try:
                urllib.request.urlopen(url + "/fail", timeout=2).read()
            except Exception:
                pass
            wait(lambda: inspect(vm)["state"] == "running" and inspect(vm).get("restart_count") == 1, "failed guest restart")
            wait(lambda: urllib.request.urlopen(url, timeout=1).read().startswith(b"ok"), "restarted guest HTTP")
            restarted_tree = host_tree(vm)
            if restarted_tree["pids"] == old_tree["pids"]:
                raise RuntimeError("restart retained the old process identities")
            first_restart = stats(vm); time.sleep(.4); second_restart = stats(vm)
            if first_restart["source"] != "darwin-libproc-tree" or second_restart["source"] != "darwin-libproc-tree":
                raise RuntimeError("restart lost tree stats")

            # Adoption intentionally does not retain automatic restart policy, so
            # exercise the configured restart first and adopt that replacement tree.
            daemon.kill(); daemon.wait(timeout=10)
            daemon = start_daemon()
            wait(lambda: inspect(vm)["state"] == "running", "daemon adoption")
            stats(vm); time.sleep(.3)
            adopted = stats(vm); adopted_host = host_tree(vm)
            if adopted["source"] != "darwin-libproc-tree" or adopted_host["pids"] != restarted_tree["pids"]:
                raise RuntimeError(f"adoption lost process identity/tree: {adopted} {adopted_host}")

            results.update({"vm": vm, "idle": idle, "idle_host": idle_host, "ram": ram, "ram_host": ram_host,
                            "cpu_samples": cpu_samples, "cpu_peak_api_pct": peak, "cpu_peak_ps_pct": ps_peak,
                            "supervisor_peak_ps_pct": supervisor_peak, "adopted": adopted,
                            "adopted_host": adopted_host, "old_tree": old_tree,
                            "restarted_first": first_restart, "restarted_second": second_restart,
                            "restarted_host": restarted_tree})
        finally:
            if vm:
                subprocess.run([str(BIN / "jerboa"), "kill", vm], env=env, capture_output=True, timeout=10)
            daemon.terminate()
            try: daemon.wait(timeout=10)
            except subprocess.TimeoutExpired:
                daemon.kill(); daemon.wait(timeout=10)
            log.close()
    (OUT / "integration.json").write_text(json.dumps(results, indent=2) + "\n")
    print(json.dumps({"ok": True, "vm": results.get("vm"), "cpu_peak_api_pct": results.get("cpu_peak_api_pct"),
                      "cpu_peak_ps_pct": results.get("cpu_peak_ps_pct"), "ram_api": results.get("ram", {}).get("mem_bytes"),
                      "ram_ps": results.get("ram_host", {}).get("rss_bytes")}, indent=2))


if __name__ == "__main__":
    main()
