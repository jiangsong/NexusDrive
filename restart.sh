#!/usr/bin/env bash
# Rebuild cloudfs, stop the running daemon, detach its mount, start it again.
#
#   ./restart.sh
#
# Order matters: build first, so a compile error leaves the running daemon and
# its mount untouched. The daemon is stopped with SIGTERM, which makes it
# unmount and close the journal cleanly; it is never SIGKILLed here, because
# that would leave a stale mount and skip the shutdown flush.
#
# Environment:
#   CLOUDFS_CONFIG  config path (default ~/.cloudfs/config.yaml); the control
#                   socket is looked up next to it, as the daemon does
#   CLOUDFS_LOG     where the daemon's stdout/stderr go (default /tmp/cloudfs-run.log)
#   STOP_TIMEOUT    seconds to wait for the old daemon to exit (default 30)
#   START_TIMEOUT   seconds to wait for the new mount to appear (default 30)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

CONFIG="${CLOUDFS_CONFIG:-$HOME/.cloudfs/config.yaml}"
CONFIG="${CONFIG/#\~/$HOME}"
SOCK="$(dirname "$CONFIG")/control.sock"
LOG="${CLOUDFS_LOG:-/tmp/cloudfs-run.log}"
STOP_TIMEOUT="${STOP_TIMEOUT:-30}"
START_TIMEOUT="${START_TIMEOUT:-30}"

log() { printf '==> %s\n' "$*"; }
die() { printf 'restart.sh: %s\n' "$*" >&2; exit 1; }

# Pids holding the control socket: the daemon, plus any short-lived CLI client.
socket_pids() { lsof -t -- "$SOCK" 2>/dev/null || true; }

# Mount points whose filesystem is named "cloudfs" (macOS and Linux formats).
cloudfs_mounts() {
  mount | sed -nE 's/^cloudfs on (.*) (\(|type ).*/\1/p'
}

# wait_gone PID SECONDS: succeed once PID no longer exists.
wait_gone() {
  local pid=$1 ticks=$(($2 * 10)) i=0
  while kill -0 "$pid" 2>/dev/null; do
    [ "$i" -ge "$ticks" ] && return 1
    sleep 0.1
    i=$((i + 1))
  done
}

# 1. Build to a side file, then swap it in. A rename gives the binary a new
# inode, so the still-running daemon keeps its old image undisturbed.
log "building"
CGO_ENABLED=0 ./gow build -buildvcs=false -o cloudfs.new ./cmd/cloudfs \
  || { rm -f cloudfs.new; die "build failed; the running daemon was left alone"; }
mv -f cloudfs.new cloudfs

# 2. Stop the old daemon.
pids="$(socket_pids)"
if [ -n "$pids" ]; then
  log "stopping daemon (pid $(echo $pids))"
  for pid in $pids; do
    [ "$pid" = "$$" ] && continue
    kill -TERM "$pid" 2>/dev/null || true
  done
  for pid in $pids; do
    [ "$pid" = "$$" ] && continue
    wait_gone "$pid" "$STOP_TIMEOUT" \
      || die "pid $pid still running after ${STOP_TIMEOUT}s; it is probably flushing. Check $LOG, then kill it by hand if you must"
  done
else
  log "no daemon running"
fi

# 3. Detach anything still mounted (a daemon that died without unmounting
# leaves a dead mount behind, which makes the mount point unreadable).
for mp in $(cloudfs_mounts); do
  log "detaching leftover mount $mp"
  ./cloudfs umount "$mp" || die "could not unmount $mp"
done

# 4. Start the new daemon in the background. This is the same no-argument
# entry point a person runs; --no-open keeps it from opening a browser.
log "starting daemon, log: $LOG"
start_line=0
[ -f "$LOG" ] && start_line="$(wc -l < "$LOG")"
printf '\n--- restart.sh %s ---\n' "$(date '+%F %T')" >> "$LOG"
nohup "$ROOT/cloudfs" --no-open >> "$LOG" 2>&1 < /dev/null &
new_pid=$!
disown "$new_pid" 2>/dev/null || true

# 5. Wait until the daemon reports it is serving. The kernel mount appears a
# moment before the daemon prints its summary, so the summary's last line is
# the readiness signal, with the mount as a second condition. Fail loudly if
# the daemon dies first.
started() {
  [ -n "$(cloudfs_mounts)" ] \
    && tail -n +"$((start_line + 1))" "$LOG" | grep -q '^press ctrl-c'
}
i=0
ticks=$((START_TIMEOUT * 10))
until started; do
  if ! kill -0 "$new_pid" 2>/dev/null; then
    tail -n +"$((start_line + 1))" "$LOG" >&2
    die "daemon exited during start-up"
  fi
  if [ "$i" -ge "$ticks" ]; then
    tail -n +"$((start_line + 1))" "$LOG" >&2
    die "no mount after ${START_TIMEOUT}s (daemon pid $new_pid still running; is the config usable?)"
  fi
  sleep 0.1
  i=$((i + 1))
done

log "up (pid $new_pid)"
tail -n +"$((start_line + 1))" "$LOG" | grep -v '^---' || true
