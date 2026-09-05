#!/bin/bash
# Builds fio from source into a scratch directory. No root needed: fio only
# wants a C compiler and make. libaio is optional and the sync/psync engines
# the job files use do not need it.
set -euo pipefail
DEST="${1:-$PWD/.fio}"
VER="${FIO_VERSION:-3.38}"
mkdir -p "$DEST"
cd "$DEST"
if [ -x "$DEST/fio-fio-$VER/fio" ]; then echo "$DEST/fio-fio-$VER/fio"; exit 0; fi
curl -sSL -o fio.tar.gz "https://github.com/axboe/fio/archive/refs/tags/fio-$VER.tar.gz"
tar xzf fio.tar.gz
cd "fio-fio-$VER"
./configure >/dev/null
make -j"$(nproc)" >/dev/null
echo "$DEST/fio-fio-$VER/fio"
