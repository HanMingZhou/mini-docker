#!/usr/bin/env bash
# 准备一个最小 rootfs，用于 mydocker Level 1 验收。
#
# 默认用 Docker 从 busybox 镜像导出 rootfs。如果没装 Docker，可以改用 debootstrap 等工具。
#
# 用法: ./scripts/prepare-rootfs.sh [target_dir] [image]
set -euo pipefail

TARGET="${1:-./rootfs-busybox}"
IMAGE="${2:-busybox:latest}"

if [[ -d "$TARGET" && -n "$(ls -A "$TARGET" 2>/dev/null || true)" ]]; then
    echo "target $TARGET already exists and is non-empty, remove it first or pick another dir" >&2
    exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
    echo "this script requires docker to export a rootfs" >&2
    exit 1
fi

mkdir -p "$TARGET"
CID=$(docker create "$IMAGE" true)
trap 'docker rm -f "$CID" >/dev/null 2>&1 || true' EXIT
docker export "$CID" | tar -C "$TARGET" -xf -
echo "rootfs exported to: $TARGET"
