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
    echo "building $target_os/$target_arch"
    CGO_ENABLED=0 GOOS=$target_os GOARCH=$target_arch go build \
        -buildvcs=false \
        -trimpath \
        -ldflags "-s -w -buildid= -X main.version=$release_version" \
        -o "$output" ./cmd/cloudfs
}

build_one linux amd64
build_one linux arm64
build_one darwin amd64
build_one darwin arm64

(
    cd "$release_dir"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum cloudfs_* > checksums.txt
    else
        shasum -a 256 cloudfs_* > checksums.txt
    fi
)

echo "release assets written to $release_dir"
