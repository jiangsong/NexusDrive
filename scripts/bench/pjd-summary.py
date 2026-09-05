#!/usr/bin/env python3
"""Summarises a pjdfstest TAP log, sorting failures into the reasons a cloud
mount has for them. Usage: pjd-summary.py <tap.log>

Categories:
  root      needs root (chown, mknod, setuid/setgid bits, sticky dirs)
  bydesign  documented differences: hard links, POSIX permission bits, and
            per-file mode/owner are not modelled by cloud drives
  real      everything else — these are bugs to look at
"""
import re, sys
from collections import Counter, defaultdict

# Switching uid/gid ("-u 65534") needs root, as do chown, mknod and the
# privileged mode bits.
root_words = ("-u ", "-g ", "chown", "mknod", "setuid", "setgid", "sticky", "EPERM", "flags", "chflags")
# Cloud drives have no hard links, symlinks, device or special files, and no
# POSIX permission model; these are documented differences, not defects.
design_words = ("link ", "symlink", "mode", "chmod", "EACCES", "umask", "ETXTBSY", "readlink", "lchmod",
                "mkfifo", "bind", "EOPNOTSUPP")

def classify(line):
    l = line.lower()
    if any(w.lower() in l for w in root_words):
        return "root"
    if any(w.lower() in l for w in design_words):
        return "bydesign"
    return "real"

def main(path):
    suite = None
    ok = Counter(); bad = Counter(); reasons = defaultdict(list)
    for line in open(path, errors="replace"):
        if line.startswith("###"):
            suite = line[4:].strip().split("/")[0]
            continue
        if line.startswith("ok"):
            ok[suite] += 1
        elif line.startswith("not ok"):
            bad[suite] += 1
            reasons[classify(line)].append((suite, line.strip()[:140]))
    print(f"{'suite':<12} {'ok':>6} {'not ok':>7}")
    for s in sorted(set(ok) | set(bad)):
        print(f"{s:<12} {ok[s]:>6} {bad[s]:>7}")
    print(f"{'total':<12} {sum(ok.values()):>6} {sum(bad.values()):>7}")
    print()
    for cat in ("real", "bydesign", "root"):
        items = reasons.get(cat, [])
        print(f"== {cat}: {len(items)}")
        seen = Counter(s for s, _ in items)
        for s, n in sorted(seen.items()):
            print(f"   {s:<12} {n}")
        if cat == "real":
            for s, l in items[:25]:
                print("   ", s, "|", l)

if __name__ == "__main__":
    main(sys.argv[1])
