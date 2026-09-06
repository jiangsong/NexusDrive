#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
    echo "usage: $0 vMAJOR.MINOR.PATCH" >&2
    exit 2
fi

release_version=${1#v}
case "$release_version" in
    ''|*[!0-9A-Za-z.-]*)
        echo "invalid release version: $1" >&2
        exit 2
        ;;
esac

release_dir=${DIST_DIR:-dist}
if [ -e "$release_dir" ]; then
    echo "$release_dir already exists; move or remove it before building a release" >&2
    exit 1
fi
mkdir -p "$release_dir"

build_one() {
    target_os=$1
    target_arch=$2
    output="$release_dir/cloudfs_${release_version}_${target_os}_${target_arch}"
    # The Windows static binary does everything except mount a FUSE filesystem;
    # a kernel mount needs the separate WinFsp build (-tags winfsp, cgo).
    if [ "$target_os" = "windows" ]; then
        output="${output}.exe"
    fi
    echo "building $target_os/$target_arch"
    CGO_ENABLED=0 GOOS=$target_os GOARCH=$target_arch go build \
        -buildvcs=false \
        -trimpath \
        -ldflags "-s -w -buildid= -X main.version=$release_version" \
        -o "$output" ./cmd/cloudfs
}

# The Windows WinFsp mount build. cgofuse loads winfsp-x64.dll at run time, so
# it needs no cgo and cross-compiles from Linux like every other target; it
# ships alongside the static binary as a second product a user installs only if
# they want a kernel mount (and have the WinFsp driver).
build_winfsp() {
    output="$release_dir/cloudfs_${release_version}_windows_amd64_mount.exe"
    echo "building windows/amd64 (WinFsp mount)"
    CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build \
        -buildvcs=false \
        -trimpath \
        -tags winfsp \
        -ldflags "-s -w -buildid= -X main.version=$release_version" \
        -o "$output" ./cmd/cloudfs
}

build_one linux amd64
build_one linux arm64
build_one darwin amd64
build_one darwin arm64
build_one windows amd64
build_winfsp

(
    cd "$release_dir"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum cloudfs_* > checksums.txt
    else
        shasum -a 256 cloudfs_* > checksums.txt
    fi
)

echo "release assets written to $release_dir"
