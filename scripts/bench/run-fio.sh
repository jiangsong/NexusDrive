#!/bin/bash
# Runs every fio job file against a directory and writes one JSON per job.
#   scripts/bench/run-fio.sh <fio-binary> <target-dir> <out-dir>
set -euo pipefail
FIO="$1"; DIR="$2"; OUT="$3"
HERE="$(cd "$(dirname "$0")" && pwd)"
mkdir -p "$OUT" "$DIR/scratch" "$DIR/scratch/fio-meta"   # the meta job creates its files here
# The metadata job creates ten thousand files, and their uploads drain in the
# background for a minute afterwards; anything measured meanwhile is
# measured against that. It runs last.
for job in "$HERE"/fio/seqread.fio "$HERE"/fio/seqread-4k.fio "$HERE"/fio/randread-4k.fio "$HERE"/fio/seqwrite.fio "$HERE"/fio/meta.fio; do
  name=$(basename "$job" .fio)
  echo "### fio $name"
  DIR="$DIR" "$FIO" --output-format=json --output="$OUT/$name.json" "$job" >/dev/null
  python3 - "$OUT/$name.json" <<'PY'
import json, sys
j = json.load(open(sys.argv[1]))
for job in j["jobs"]:
    r, w = job["read"], job["write"]
    print(f"  {job['jobname']:<20} read {r['bw_bytes']/2**20:8.1f} MB/s {r['iops']:9.0f} iops | write {w['bw_bytes']/2**20:8.1f} MB/s {w['iops']:9.0f} iops")
PY
done
