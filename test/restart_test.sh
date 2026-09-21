#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'pkill -f "$TMP/cloudfs" 2>/dev/null || true; rm -rf "$TMP"' EXIT

cp "$ROOT/restart.sh" "$TMP/restart.sh"

cat > "$TMP/gow" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
out=
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then
    out=$2
    shift 2
  else
    shift
  fi
done
cat > "$out" <<'SCRIPT'
#!/usr/bin/env bash
if [ "${1:-}" = config ] && [ "${2:-}" = check ]; then
  if [ "${MOCK_CONFIG_FAIL:-}" = 1 ]; then
    printf 'invalid mock configuration\n' >&2
    exit 1
  fi
  [ -n "${MOCK_CONFIG_MOUNT:-}" ] || exit 1
  printf 'ok: mock config\n  mount 0 at %s\n' "$MOCK_CONFIG_MOUNT"
  exit 0
fi
if [ "${1:-}" = umount ]; then
  [ "${MOCK_UMOUNT_FAIL:-}" != 1 ]
  exit
fi
printf 'wrote a starter configuration at /tmp/config.yaml\n'
printf 'setup is open at http://127.0.0.1:9101/#/setup\n'
printf '  add your drives there, then let it restart the daemon to mount them\n'
sleep 5
SCRIPT
chmod +x "$out"
EOF
chmod +x "$TMP/gow"

mkdir -p "$TMP/bin" "$TMP/config"
cat > "$TMP/bin/lsof" <<'EOF'
#!/usr/bin/env bash
if [ "${MOCK_CWD_HOLDER:-}" = 1 ] && [ "${1:-}" = -nP ]; then
  printf 'p4242\nccodex\nn%s/project\n' "$MOCK_MOUNT_POINT"
  exit 0
fi
exit 1
EOF
cat > "$TMP/bin/findmnt" <<'EOF'
#!/usr/bin/env bash
if [ -n "${MOCK_MOUNT_POINT:-}" ]; then
  printf '%s\n' "$MOCK_MOUNT_POINT"
fi
if [ -n "${MOCK_OTHER_MOUNT:-}" ]; then
  printf '%s\n' "$MOCK_OTHER_MOUNT"
fi
exit 0
EOF
cat > "$TMP/bin/fusermount3" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" > "$MOCK_DETACH_LOG"
EOF
chmod +x "$TMP/bin/lsof" "$TMP/bin/findmnt" "$TMP/bin/fusermount3"

output="$({
  cd "$TMP"
  PATH="$TMP/bin:$PATH" \
    CLOUDFS_CONFIG="$TMP/config/config.yaml" \
    CLOUDFS_LOG="$TMP/cloudfs.log" \
    START_TIMEOUT=1 \
    ./restart.sh
} 2>&1)" || {
  printf '%s\n' "$output" >&2
  exit 1
}

grep -q '^==> setup is ready (pid ' <<<"$output"

: > "$TMP/config/config.yaml"
invalid="$({
  cd "$TMP"
  PATH="$TMP/bin:$PATH" \
    CLOUDFS_CONFIG="$TMP/config/config.yaml" \
    CLOUDFS_LOG="$TMP/cloudfs.log" \
    MOCK_CONFIG_FAIL=1 \
    ./restart.sh
} 2>&1)" && {
  printf 'restart unexpectedly proceeded with an invalid configuration\n' >&2
  exit 1
}
grep -q 'configuration check failed; the running daemon was left alone' <<<"$invalid"
if grep -q '^==> stopping daemon' <<<"$invalid" || grep -q '^==> starting daemon' <<<"$invalid"; then
  printf 'restart touched the daemon after configuration validation failed\n' >&2
  exit 1
fi
if [ -e "$TMP/cloudfs.new" ]; then
  printf 'invalid configuration left cloudfs.new behind\n' >&2
  exit 1
fi

mkdir -p "$TMP/mount"
blocked="$({
  cd "$TMP"
  PATH="$TMP/bin:$PATH" \
    CLOUDFS_CONFIG="$TMP/config/config.yaml" \
    CLOUDFS_LOG="$TMP/cloudfs.log" \
    MOCK_CWD_HOLDER=1 \
    MOCK_CONFIG_MOUNT="$TMP/mount" \
    MOCK_MOUNT_POINT="$TMP/mount" \
    ./restart.sh
} 2>&1)" && {
  printf 'restart unexpectedly proceeded with a cwd holder\n' >&2
  exit 1
}
grep -q 'pid 4242.*codex.*project' <<<"$blocked"
grep -q 'restart refused: leave or close the listed working directories' <<<"$blocked"
if grep -q '^==> starting daemon' <<<"$blocked"; then
  printf 'restart reached daemon start despite a cwd holder\n' >&2
  exit 1
fi
if [ -e "$TMP/cloudfs.new" ]; then
  printf 'refused restart left cloudfs.new behind\n' >&2
  exit 1
fi

# A dead mount cannot be opened, so cwd holders no longer block recovery. The
# ordinary unmount fails and the script lazily detaches only this config's
# mount, leaving another CloudFS instance alone.
detach_log="$TMP/detach.log"
stale="$TMP/stale mount"
recovered="$({
  cd "$TMP"
  PATH="$TMP/bin:$PATH" \
    CLOUDFS_CONFIG="$TMP/config/config.yaml" \
    CLOUDFS_LOG="$TMP/cloudfs.log" \
    MOCK_CONFIG_MOUNT="$stale" \
    MOCK_MOUNT_POINT="$stale" \
    MOCK_OTHER_MOUNT="$TMP/other-cloudfs" \
    MOCK_UMOUNT_FAIL=1 \
    MOCK_DETACH_LOG="$detach_log" \
    START_TIMEOUT=1 \
    ./restart.sh
} 2>&1)" || {
  printf '%s\n' "$recovered" >&2
  exit 1
}
grep -q "normal unmount failed; detaching stale mount $stale" <<<"$recovered"
grep -Fxq -- "-uz $stale" "$detach_log"
if grep -q 'other-cloudfs' "$detach_log"; then
  printf 'restart detached another config mount\n' >&2
  exit 1
fi
