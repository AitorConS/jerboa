#!/usr/bin/env python3
"""Compare two builds with benchmark-regressions.py and fail on a regression.

Two ways to use it:

  # Run both builds, interleaved, and compare them.
  benchmark-compare.py --packages PACKAGES \\
      --base-bin BASE/dist --base-firecracker BASE/firecracker \\
      --new-bin NEW/dist --new-firecracker NEW/firecracker [--rounds 3]

  # Compare results.json files that already exist.
  benchmark-compare.py --base base1.json base2.json --new new1.json new2.json

A build is its CLI and daemon directory (with tools/ inside, or --*-tools) and
its VMM. Rounds alternate which build goes first, so drift on the host does
not favor one. A metric regresses when the new median is worse than the base
median by more than its threshold and, with two or more rounds per build, no
new round is as good as the worst base round. Exit status 1 on a regression.
"""
import argparse
import json
import statistics
import subprocess
import sys
import tempfile
from pathlib import Path

# name -> (unit, higher is better, threshold in percent, how to read it)
# The thresholds are what repeated runs of one build differ by on a quiet
# host: random write moves by 10 to 15 percent, the rest by about 5.
METRICS = {
    "startup": ("ms", False, 10, lambda r: r["jerboa-startup"]["warm_median_ms"]),
    "tcp vm-vm": ("Mbit/s", True, 10, lambda r: r["iperf-tcp"]["end"]["sum_received"]["bits_per_second"] / 1e6),
    "randread": ("MiB/s", True, 10, lambda r: sum(j["read"]["bw"] for j in r["fio-randread"]["jobs"]) / 1024),
    "randwrite": ("MiB/s", True, 20, lambda r: sum(j["write"]["bw"] for j in r["fio-randwrite"]["jobs"]) / 1024),
}


def read(paths):
    """The value of every metric each results file has, per metric."""
    values = {}
    for path in paths:
        results = json.loads(Path(path).read_text())
        for name, (_, _, _, get) in METRICS.items():
            try:
                values.setdefault(name, []).append(float(get(results)))
            except (KeyError, IndexError, TypeError):
                pass  # the case was not run
    return values


def compare(base, new, threshold_scale=1.0):
    """Rows of (name, unit, base median, new median, change %, verdict)."""
    rows = []
    for name, (unit, higher, threshold, _) in METRICS.items():
        b, n = base.get(name), new.get(name)
        if not b or not n:
            continue
        bm, nm = statistics.median(b), statistics.median(n)
        change = (nm - bm) / bm * 100
        worse = -change if higher else change
        # With repeated rounds, a regression must also clear the base's spread.
        separated = len(b) < 2 or len(n) < 2 or (max(n) < min(b) if higher else min(n) > max(b))
        if worse > threshold * threshold_scale and separated:
            verdict = "REGRESSION"
        elif worse < -threshold * threshold_scale and (len(b) < 2 or len(n) < 2 or (min(n) > max(b) if higher else max(n) < min(b))):
            verdict = "better"
        else:
            verdict = "same"
        rows.append((name, unit, bm, nm, change, verdict))
    return rows


def run(script, build, args, work):
    command = [sys.executable, str(script), "--bin", str(build["bin"]), "--tools", str(build["tools"]),
               "--firecracker", str(build["firecracker"]), "--packages", str(args.packages), "--work", str(work),
               "--cases", args.cases, "--network-seconds", str(args.network_seconds)]
    with open(str(work) + ".log", "w") as log:
        done = subprocess.run(command, stdout=log, stderr=subprocess.STDOUT)
    if done.returncode:
        raise SystemExit(f"benchmark failed for {build['bin']}: see {work}.log")
    return work / "results.json"


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--base", nargs="+", type=Path, help="results.json files of the base build")
    parser.add_argument("--new", nargs="+", type=Path, help="results.json files of the new build")
    for side in ("base", "new"):
        parser.add_argument(f"--{side}-bin", type=Path)
        parser.add_argument(f"--{side}-tools", type=Path, help="default: BIN/tools")
        parser.add_argument(f"--{side}-firecracker", type=Path)
    parser.add_argument("--packages", type=Path)
    parser.add_argument("--rounds", type=int, default=3)
    parser.add_argument("--cases", default="startup,network,disk")
    parser.add_argument("--network-seconds", type=int, default=10)
    parser.add_argument("--threshold-scale", type=float, default=1.0, help="multiply every threshold, for a noisy host")
    parser.add_argument("--out", type=Path, help="write the comparison as JSON")
    # Unix socket paths are short: the default temporary directory on macOS is not.
    parser.add_argument("--work", type=Path, help="directory for the runs (default: a new one under /tmp, kept)")
    a = parser.parse_args()

    if a.base and a.new:
        base, new = read(a.base), read(a.new)
    else:
        builds = {}
        for side in ("base", "new"):
            binary, vmm = getattr(a, side + "_bin"), getattr(a, side + "_firecracker")
            if not binary or not vmm or not a.packages:
                parser.error("give --base and --new results, or both builds and --packages")
            builds[side] = {"bin": binary, "tools": getattr(a, side + "_tools") or binary / "tools", "firecracker": vmm}
        if a.rounds < 1:
            parser.error("rounds must be at least one")
        work = a.work or Path(tempfile.mkdtemp(prefix="jbc-", dir="/tmp"))
        work.mkdir(parents=True, exist_ok=True)
        script = Path(__file__).with_name("benchmark-regressions.py")
        files = {"base": [], "new": []}
        for i in range(a.rounds):
            for side in ("base", "new") if i % 2 == 0 else ("new", "base"):
                print(f"round {i + 1} of {a.rounds}: {side}", flush=True)
                files[side].append(run(script, builds[side], a, work / f"{side}{i + 1}"))
        base, new = read(files["base"]), read(files["new"])
        print(f"runs kept in {work}")

    rows = compare(base, new, a.threshold_scale)
    if not rows:
        raise SystemExit("no metric is present in both builds")
    print(f"{'metric':<12}{'base':>12}{'new':>12}{'change':>10}  verdict")
    for name, unit, bm, nm, change, verdict in rows:
        print(f"{name:<12}{bm:>12.1f}{nm:>12.1f}{change:>+9.1f}%  {verdict} ({unit}, {len(base[name])} and {len(new[name])} runs)")
    if a.out:
        a.out.write_text(json.dumps({"base": base, "new": new, "rows": [
            {"metric": n, "unit": u, "base": b, "new": w, "change_percent": c, "verdict": v} for n, u, b, w, c, v in rows]}, indent=2) + "\n")
    return 1 if any(row[5] == "REGRESSION" for row in rows) else 0


if __name__ == "__main__":
    raise SystemExit(main())
