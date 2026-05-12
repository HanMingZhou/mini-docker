#!/usr/bin/env bash
# Level 1 冒烟测试：检查隔离是否生效。
# 在 Linux 宿主机上 `sudo ./scripts/smoke-l1.sh` 运行。
set -euo pipefail

ROOTFS="${ROOTFS:-./rootfs-busybox}"
BIN="${BIN:-./bin/mydocker}"

if [[ ! -x "$BIN" ]]; then
    echo "build first: make build-linux  (or: go build -o bin/mydocker ./cmd/mydocker)" >&2
    exit 1
fi
if [[ ! -d "$ROOTFS" ]]; then
    echo "rootfs not found at $ROOTFS; run scripts/prepare-rootfs.sh first" >&2
    exit 1
fi

echo "== 1) PID isolation: 'ps' inside container should see very few processes =="
"$BIN" run --rootfs "$ROOTFS" --hostname l1-test /bin/sh -c 'ps -ef || ps'

echo
echo "== 2) UTS isolation: hostname inside should be 'l1-test' =="
"$BIN" run --rootfs "$ROOTFS" --hostname l1-test /bin/sh -c 'hostname'

echo
echo "== 3) Mount isolation: / should be the busybox rootfs, not the host =="
"$BIN" run --rootfs "$ROOTFS" --hostname l1-test /bin/sh -c 'ls /'
