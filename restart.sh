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
NEW_BINARY="$ROOT/cloudfs.new"
TARGET_MOUNT=

# A refused preflight must not leave a side binary behind.
trap 'rm -f "$NEW_BINARY"' EXIT

log() { printf '==> %s\n' "$*"; }
die() { printf 'restart.sh: %s\n' "$*" >&2; exit 1; }

# Pids holding the control socket: the daemon, plus any short-lived CLI client.
socket_pids() { lsof -t -- "$SOCK" 2>/dev/null || true; }

# Read the mount this no-argument daemon will use from the same validated
# config parser as the daemon. A missing starter config has no mount yet.
configured_mount() {
  "$NEW_BINARY" config check --config "$CONFIG" 2>/dev/null \
    | sed -nE 's/^  mount 0 at (.*)$/\1/p' \
    | head -n 1
}

# List CloudFS mounts without losing spaces in their paths. findmnt is the
# authoritative Linux path; mount output is the portable fallback for macOS.
all_cloudfs_mounts() {
  if [ "$(uname -s)" = Linux ] && command -v findmnt >/dev/null 2>&1; then
    findmnt -rn -t fuse.cloudfs -o TARGET 2>/dev/null || true
    return
  fi
  mount | sed -nE \
    -e 's#^.* on (.*) type fuse\.cloudfs .*#\1#p' \
    -e 's#^(.*/)?cloudfs on (.*) \(.*\)$#\2#p'
}

# Only this configuration's first mount belongs to the daemon being restarted.
# Other configs may run from the same binary and must remain untouched.
cloudfs_mounts() {
  local mount_path
  [ -n "$TARGET_MOUNT" ] || return 0
  while IFS= read -r mount_path; do
    [ "$mount_path" = "$TARGET_MOUNT" ] && printf '%s\n' "$mount_path"
  done < <(all_cloudfs_mounts)
}

# Print processes whose current working directory belongs to a CloudFS mount.
# A process keeps a reference to the old FUSE mount, not merely its path text;
# unmounting and mounting the same path therefore leaves getcwd(2) returning
# ENOENT in that process. There is no safe way for this script to chdir another
# process after the fact, so refuse the restart while such holders exist.
mount_cwd_holders() {
  local mount_path=$1
  command -v lsof >/dev/null 2>&1 || return 0
  lsof -nP -a -d cwd -Fpcn 2>/dev/null | awk -v root="$mount_path" '
    /^p/ { pid = substr($0, 2) }
    /^c/ { command = substr($0, 2) }
    /^n/ {
      path = substr($0, 2)
      if (path == root || index(path, root "/") == 1)
        printf "  pid %-8s %-24s %s\n", pid, command, path
    }
  '
}

# Opening the directory forces a request to the FUSE server without listing
# remote entries. A dead mount fails here even when stat data is still cached.
mount_responds() {
  if { exec 9<"$1"; } 2>/dev/null; then
    exec 9<&-
    return 0
  fi
  return 1
}

refuse_busy_cwds() {
  local mount_path holders blocked=0
  while IFS= read -r mount_path; do
    [ -n "$mount_path" ] || continue
    # Holders of an already-dead mount are already stranded; refusing here
    # would prevent the lazy detach below from repairing the mount pathname.
    mount_responds "$mount_path" || continue
    holders="$(mount_cwd_holders "$mount_path")"
    if [ -n "$holders" ]; then
      printf 'restart.sh: processes have a working directory inside %s:\n%s\n' "$mount_path" "$holders" >&2
      blocked=1
    fi
  done < <(cloudfs_mounts)
  if [ "$blocked" -ne 0 ]; then
    die "restart refused: leave or close the listed working directories, then retry"
  fi
}

# Detach a mount left behind after its daemon has exited. Try the ordinary
# CLI first; a dead FUSE mount may still be busy because a shell or file
# manager holds its old cwd, in which case only a forced/lazy detach can free
# the pathname for the replacement daemon.
detach_leftover_mount() {
  local mount_path=$1 candidate
  if ./cloudfs umount "$mount_path"; then
    return 0
  fi

  log "normal unmount failed; detaching stale mount $mount_path"
  if [ "$(uname -s)" = Darwin ]; then
    for candidate in "umount -f" "diskutil unmount force"; do
      set -- $candidate
      command -v "$1" >/dev/null 2>&1 || continue
      "$@" "$mount_path" && return 0
    done
  else
    for candidate in "fusermount3 -uz" "fusermount -uz" "umount -l"; do
      set -- $candidate
      command -v "$1" >/dev/null 2>&1 || continue
      "$@" "$mount_path" && return 0
    done
  fi
  return 1
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
CGO_ENABLED=0 ./gow build -buildvcs=false -o "$NEW_BINARY" ./cmd/cloudfs \
  || die "build failed; the running daemon was left alone"
if [ -e "$CONFIG" ] || [ -L "$CONFIG" ]; then
  TARGET_MOUNT="$(configured_mount)" \
    || die "configuration check failed; the running daemon was left alone"
fi

# 2. Do not strand shells, editors or agents in a live FUSE mount. Dead mounts
# are skipped here so the stale-mount recovery below can proceed.
refuse_busy_cwds

# Every preflight passed. Swap the tested binary in only now, so a refused
# restart leaves both the running daemon and its matching CLI untouched.
mv -f "$NEW_BINARY" cloudfs

# 3. Stop the old daemon.
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

# 4. Detach anything still mounted (a daemon that died without unmounting
# leaves a dead mount behind, which makes the mount point unreadable).
while IFS= read -r mp; do
  [ -n "$mp" ] || continue
  log "detaching leftover mount $mp"
  detach_leftover_mount "$mp" || die "could not detach stale mount $mp"
done < <(cloudfs_mounts)

# 5. Start the new daemon in the background. This is the same no-argument
# entry point a person runs; --no-open keeps it from opening a browser.
log "starting daemon, log: $LOG"
start_line=0
[ -f "$LOG" ] && start_line="$(wc -l < "$LOG")"
printf '\n--- restart.sh %s ---\n' "$(date '+%F %T')" >> "$LOG"
nohup "$ROOT/cloudfs" --no-open >> "$LOG" 2>&1 < /dev/null &
new_pid=$!
disown "$new_pid" 2>/dev/null || true

# 6. Wait until the daemon reports it is serving. The kernel mount appears a
# moment before the daemon prints its summary, so the summary's last line is
# the readiness signal, with the mount as a second condition. A fresh install
# has no drives to mount and is ready when its setup page is listening. Fail
# loudly if the daemon dies first.
new_log() { tail -n +"$((start_line + 1))" "$LOG"; }
setup_ready() { new_log | grep -q '^setup is open at '; }
started() {
  [ -n "$(cloudfs_mounts)" ] \
    && new_log | grep -q '^press ctrl-c' \
    || setup_ready
}
i=0
ticks=$((START_TIMEOUT * 10))
until started; do
  if ! kill -0 "$new_pid" 2>/dev/null; then
    new_log >&2
    die "daemon exited during start-up"
  fi
  if [ "$i" -ge "$ticks" ]; then
    new_log >&2
    die "daemon was not ready after ${START_TIMEOUT}s (pid $new_pid still running; is the config usable?)"
  fi
  sleep 0.1
  i=$((i + 1))
done

if setup_ready; then
  log "setup is ready (pid $new_pid)"
else
  log "up (pid $new_pid)"
fi
new_log | grep -v '^---' || true
