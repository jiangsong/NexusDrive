#!/usr/bin/env python3
"""Puts fio's numbers beside the built-in benchmark's for the same workloads.

Usage: cross-check.py <bench.jsonl> <fio-json-dir>

fio is run with --output-format=json into one file per job file. A pair that
differs by more than 25 % is flagged; the point is not that they agree to the
digit — fio and the built-in tool issue different syscall patterns — but that
the built-in numbers are not off by a class.
"""
import glob, json, os, sys

pairs = {  # built-in test name -> (fio json file stem, fio job name, metric)
    "seqread-1024k": ("seqread", "seqread-1m", "read"),
    "seqread-4k": ("seqread-4k", "seqread-4k", "read"),
    "randread-4k": ("randread-4k", "randread-4k-qd1", "read"),
    "seqwrite-64m": ("seqwrite", "seqwrite-64m", "write"),
}

def load_bench(path):
    out = {}
    for line in open(path):
        line = line.strip()
        if not line:
            continue
        r = json.loads(line)
        out.setdefault(r["test"], {})[r["label"]] = r
    return out

def load_fio(d):
    out = {}
    for f in glob.glob(os.path.join(d, "*.json")):
        stem = os.path.splitext(os.path.basename(f))[0]
        try:
            j = json.load(open(f))
        except Exception:
            continue
        for job in j.get("jobs", []):
            out[(stem, job["jobname"])] = job
    return out

def mbs(job, metric):
    return job[metric]["bw_bytes"] / (1 << 20)

def main():
    bench, fio = load_bench(sys.argv[1]), load_fio(sys.argv[2])
    label = sys.argv[3] if len(sys.argv) > 3 else None
    red = 0
    print(f"{'workload':<16} {'built-in MB/s':>14} {'fio MB/s':>10} {'diff':>8}")
    for test, (stem, jobname, metric) in pairs.items():
        b = bench.get(test, {})
        r = b.get(label) if label else (next(iter(b.values())) if b else None)
        f = fio.get((stem, jobname))
        if not r or not f:
            print(f"{test:<16} {'-':>14} {'-':>10}   (missing)")
            continue
        bi, fi = r.get("mb_s", 0), mbs(f, metric)
        diff = (bi - fi) / fi * 100 if fi else float("inf")
        flag = "  <-- >25%" if abs(diff) > 25 else ""
        red += bool(flag)
        print(f"{test:<16} {bi:>14.1f} {fi:>10.1f} {diff:>+7.0f}%{flag}")
    sys.exit(1 if red else 0)

if __name__ == "__main__":
    main()
