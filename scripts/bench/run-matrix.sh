#!/bin/bash
# Runs the built-in benchmark against a cloudfs mount under four link
# conditions — direct (no proxy at all), lan (through the proxy, unimpaired),
# wan (50 ms RTT, 1 % loss, 20 Mbit) and bad (200 ms, 5 %, 5 Mbit) — by
# placing the netem proxy between cloudfs and its SFTP backend. The direct
# tier is what separates the proxy's own cost from the link's. Output is one
# JSONL file per tier plus a combined file.
#
#   scripts/bench/run-matrix.sh <workdir> <sftp-host> <sftp-user> <remote-root> [tiers] [tests]
#
# workdir holds the binaries, config, cache and results. The mount lives at
# <workdir>/mnt and the daemon's control address is 127.0.0.1:9101.
set -euo pipefail
W="$1"; HOST="$2"; USER_="$3"; ROOT="$4"
TIERS="${5:-direct,lan,wan,bad}"
TESTS="${6:-walk,statstorm,seqread1m,seqread4k,randread,parread,seqwrite,randwrite,smallfiles,metadata,stress}"
REPEAT="${REPEAT:-3}"
# 10,000 metadata files per phase is the mdtest default, but through a
# write-back mount every create also queues an upload, and three cold plus
# three warm repetitions of that dominate the run. 2,000 keeps the ratio and
# the time honest.
META_FILES="${META_FILES:-2000}"
MET=127.0.0.1:9101
NETEM_LISTEN=127.0.0.1:2222
NETEM_CTL=127.0.0.1:2223
REPO="$(cd "$(dirname "$0")/../.." && pwd)"
GO="${GO:-$REPO/gow}"

mkdir -p "$W/etc" "$W/mnt" "$W/cache" "$W/results"
"$GO" build -o "$W/cloudfs" "$REPO/cmd/cloudfs"
"$GO" build -o "$W/netem"   "$REPO/cmd/netem"

write_config(){ # write_config <sftp-host:port> [alias]
  local alias=""
  [ -n "${2:-}" ] && alias="    host_key_alias: $2"
  cat > "$W/etc/config.yaml" <<EOF
cache:
  dir: $W/cache
  max_size: 2GiB
  min_free: 512MiB
  block_size: 4MiB
proxy:
  rules: [ "FINAL,direct" ]
remotes:
  lan:
    type: sftp
    host: $1
    user: $USER_
    root: $ROOT
$alias
mounts:
  - path: $W/mnt
    layout:
      /: { remote: lan, root: /, mode: writeback, dir_ttl: 5m }
control:
  metrics: $MET
EOF
}

stop_daemon(){
  if [ -f "$W/daemon.pid" ]; then
    kill -INT "$(cat "$W/daemon.pid")" 2>/dev/null || true
    for _ in $(seq 1 60); do kill -0 "$(cat "$W/daemon.pid")" 2>/dev/null || break; sleep 0.25; done
    kill -9 "$(cat "$W/daemon.pid")" 2>/dev/null || true; rm -f "$W/daemon.pid"
  fi
  fusermount3 -u "$W/mnt" 2>/dev/null || true
}
start_daemon(){
  rm -rf "$W/cache"; mkdir -p "$W/cache"
  "$W/cloudfs" mount --config "$W/etc/config.yaml" >> "$W/daemon.log" 2>&1 &
  echo $! > "$W/daemon.pid"
  for _ in $(seq 1 120); do
    curl -s -m 2 "http://$MET/readyz" >/dev/null 2>&1 && sleep 0.5 && return 0
    sleep 0.25
  done
  echo "daemon did not come up"; tail -5 "$W/daemon.log"; exit 1
}
stop_netem(){ [ -f "$W/netem.pid" ] && { kill "$(cat "$W/netem.pid")" 2>/dev/null || true; rm -f "$W/netem.pid"; }; return 0; }
start_netem(){
  "$W/netem" --listen "$NETEM_LISTEN" --target "$HOST:22" --control "$NETEM_CTL" >> "$W/netem.log" 2>&1 &
  echo $! > "$W/netem.pid"; sleep 0.5
}
# Large-file size per tier: a 256 MiB cold read at 5 Mbit would take seven
# minutes and say nothing a 16 MiB one does not.
big_for(){ case "$1" in direct|lan) echo 256;; wan) echo 64;; bad) echo 16;; esac; }
repeat_for(){ case "$1" in bad) echo 2;; *) echo "$REPEAT";; esac; }
set_tier(){
  case "$1" in
    # direct talks to the backend itself; the proxy is dialled as 127.0.0.1,
    # so its config verifies the host key under the real name.
    direct) write_config "$HOST:22" ;;
    lan) write_config "$NETEM_LISTEN" "$HOST"; curl -s -X POST "http://$NETEM_CTL/clear" >/dev/null ;;
    wan) write_config "$NETEM_LISTEN" "$HOST"; curl -s -X POST "http://$NETEM_CTL/set?latency=25ms&jitter=3ms&stall_prob=0.01&stall_for=200ms&bandwidth=20mbit" >/dev/null ;;
    bad) write_config "$NETEM_LISTEN" "$HOST"; curl -s -X POST "http://$NETEM_CTL/set?latency=100ms&jitter=20ms&stall_prob=0.05&stall_for=400ms&bandwidth=5mbit" >/dev/null ;;
    *) echo "unknown tier $1"; exit 2 ;;
  esac
}
trap 'stop_daemon; stop_netem' EXIT

# Every impaired tier goes through the proxy so the only variable is the
# impairment; set_tier writes the config for each.
start_netem
ALL="$W/results/matrix.jsonl"; : > "$ALL"
for tier in ${TIERS//,/ }; do
  set_tier "$tier"
  stop_daemon; start_daemon
  # The dataset is created through the mount once; later runs reuse it.
  BIG=$(big_for "$tier"); REP=$(repeat_for "$tier")
  "$W/cloudfs" bench "$W/mnt" --prepare-only --big "$BIG" --metrics "$MET"
  # Let the dataset upload settle before measuring.
  for _ in $(seq 1 600); do
    n=$(curl -s "http://$MET/metrics" | awk '/^cloudfs_uploads_pending /{p=$2} /^cloudfs_uploads_in_flight /{f=$2} END{print p+f}')
    [ "${n:-1}" = "0" ] && break; sleep 0.5
  done
  OUT="$W/results/$tier.jsonl"; : > "$OUT"
  echo "### tier=$tier"
  "$W/cloudfs" bench "$W/mnt" --tests "$TESTS" --cold --repeat "$REP" --big "$BIG" --meta-files "$META_FILES" --metrics "$MET" --label "cloudfs $tier cold" --no-prepare | tee -a "$OUT"
  "$W/cloudfs" bench "$W/mnt" --tests "$TESTS" --repeat "$REP" --big "$BIG" --meta-files "$META_FILES" --metrics "$MET" --label "cloudfs $tier warm" --no-prepare | tee -a "$OUT"
  # Drain time for the writes the warm pass queued.
  t0=$(date +%s.%N)
  for _ in $(seq 1 2400); do
    n=$(curl -s "http://$MET/metrics" | awk '/^cloudfs_uploads_pending /{p=$2} /^cloudfs_uploads_in_flight /{f=$2} END{print p+f}')
    [ "${n:-1}" = "0" ] && break; sleep 0.25
  done
  dead=$(curl -s "http://$MET/metrics" | awk '/^cloudfs_uploads_dead /{print $2}')
  if [ "${dead:-0}" != 0 ]; then
    echo "### dead uploads after tier=$tier" | tee -a "$W/run.log"
    "$W/cloudfs" uploads --config "$W/etc/config.yaml" 2>&1 | tee -a "$W/run.log" | head -20
  fi
  printf '{"test":"drain","label":"cloudfs %s","wall_ms":%s,"dead":%s,"netem":%s}\n' "$tier" \
    "$(echo "($(date +%s.%N) - $t0)*1000" | bc)" "${dead:-0}" "$(curl -s http://$NETEM_CTL/stats)" | tee -a "$OUT"
  cat "$OUT" >> "$ALL"
done
echo "results in $ALL"
