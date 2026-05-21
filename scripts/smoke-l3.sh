#!/usr/bin/env bash
# Level 3 冒烟：sandbox + image + container + exec。
#
# 在 Linux 宿主机上 `sudo ./scripts/smoke-l3.sh` 运行。
# 前置：
#   - bin/mydocker 和 bin/mydocker-cri 均已构建（make build-linux）
#   - 已安装 crictl
#   - 能访问 docker hub（或通过 --platform / mirror 指到可达 registry）
#   - 可选：sudo ./scripts/install-cni.sh 装 CNI 插件让 Pod 拿到 IP；
#          不装时 Status 的 NetworkReady=false，沙箱启动不做网络分配，
#          crictl 仍然能跑完所有其它流程。
#
# 运行后 /var/run/mydocker-cri.sock 和 streaming :10350 会被创建；Ctrl-C 退出。
set -euo pipefail

CRI_BIN="${CRI_BIN:-./bin/mydocker-cri}"
MYDOCKER="${MYDOCKER:-./bin/mydocker}"
SOCK="${SOCK:-/var/run/mydocker-cri.sock}"
STREAM_ADDR="${STREAM_ADDR:-127.0.0.1:10350}"

if [[ ! -x "$CRI_BIN" || ! -x "$MYDOCKER" ]]; then
    echo "build first: make build-linux" >&2
    exit 1
fi
if ! command -v crictl >/dev/null 2>&1; then
    echo "crictl not found" >&2
    exit 1
fi

# 把 mydocker 放到 PATH，让 mydocker-cri 能找到它作为 pause 载体
export PATH="$(pwd)/bin:$PATH"

echo "== start mydocker-cri (with streaming server) =="
"$CRI_BIN" serve \
    --socket "$SOCK" \
    --streaming-addr "$STREAM_ADDR" &
PID=$!
trap 'kill "$PID" 2>/dev/null || true; rm -f "$SOCK"' EXIT
sleep 0.5

CRICTL=(crictl --runtime-endpoint "unix://$SOCK")

echo
echo "== crictl info =="
"${CRICTL[@]}" info | head -n 20 || true

echo
echo "== crictl pull busybox =="
"${CRICTL[@]}" pull busybox || {
    echo "WARN: pull failed (offline? no internet?)"
}

echo
echo "== crictl images =="
"${CRICTL[@]}" images

echo
echo "== crictl runp (create sandbox) =="
cat > /tmp/sb.json <<EOF
{
  "metadata": { "name": "smoke-sb", "namespace": "default", "uid": "uid-smoke", "attempt": 1 },
  "log_directory": "/tmp",
  "linux": {}
}
EOF
POD=$("${CRICTL[@]}" runp /tmp/sb.json)
echo "sandbox id: $POD"

echo
echo "== crictl pods =="
"${CRICTL[@]}" pods

# --- Container lifecycle (requires 'busybox' image) ---
echo
echo "== crictl create (container in sandbox) =="
cat > /tmp/ctr.json <<EOF
{
  "metadata": { "name": "smoke-ctr" },
  "image": { "image": "busybox" },
  "command": ["/bin/sh", "-c", "while true; do echo tick; sleep 1; done"],
  "log_path": "smoke-ctr.log"
}
EOF
CTR=$("${CRICTL[@]}" create "$POD" /tmp/ctr.json /tmp/sb.json) || {
    echo "SKIP: container create failed (busybox image not available?)"
    CTR=""
}

if [[ -n "$CTR" ]]; then
    echo "container id: $CTR"

    echo
    echo "== crictl start =="
    "${CRICTL[@]}" start "$CTR"

    sleep 2

    echo
    echo "== crictl ps =="
    "${CRICTL[@]}" ps

    echo
    echo "== crictl logs (CRI format should render as plain text) =="
    "${CRICTL[@]}" logs "$CTR" | head -5 || true

    echo
    echo "== raw log file (inspect CRI framing) =="
    find /tmp -name smoke-ctr.log -newer /tmp/sb.json 2>/dev/null | head -1 | xargs -r head -3 || true

    echo
    echo "== crictl exec -s (ExecSync: should print 'hello') =="
    "${CRICTL[@]}" exec -s "$CTR" echo hello || true

    echo
    echo "== crictl exec -s (ExecSync: list /) =="
    "${CRICTL[@]}" exec -s "$CTR" ls / || true

    # exec -it 需要用户交互，自动化脚本不跑；手动验证：
    #   crictl --runtime-endpoint unix:///var/run/mydocker-cri.sock exec -it "$CTR" /bin/sh

    echo
    echo "== crictl stop =="
    "${CRICTL[@]}" stop "$CTR"

    echo
    echo "== crictl rm =="
    "${CRICTL[@]}" rm "$CTR"
fi

echo
echo "== crictl stopp + rmp =="
"${CRICTL[@]}" stopp "$POD"
"${CRICTL[@]}" rmp "$POD"

echo
echo "== crictl pods (should be empty) =="
"${CRICTL[@]}" pods

echo
echo "OK"
