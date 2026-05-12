#!/usr/bin/env bash
# vm-smoke.sh — 在一台 Ubuntu VM 里驱动 mydocker-cri 的完整 CRI 生命周期。
#
# 必须 root 运行（需要建 cgroup、挂 overlay、调 CNI）。
# 不假定机器上跑 Kubelet；通过独立 socket 全程只用 crictl 驱动。
#
# 用法：
#   sudo bash /tmp/vm-smoke.sh                 # 假设 daemon 已在另一 shell 跑
#   sudo bash /tmp/vm-smoke.sh --with-daemon   # 脚本自己起 daemon 再跑
set -uo pipefail

SOCK=/var/run/mydocker-cri.sock
IMAGE=busybox
CRICTL="crictl --runtime-endpoint=unix://$SOCK"
WITH_DAEMON=0

for a in "$@"; do
    case "$a" in
        --with-daemon) WITH_DAEMON=1 ;;
    esac
done

log()  { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }
ok()   { printf '   \033[32m✓\033[0m %s\n' "$*"; }
fail() { printf '   \033[31m✗\033[0m %s\n' "$*"; FAILED=1; }

FAILED=0
POD=""
CTR=""
DAEMON_PID=""

trap cleanup EXIT
cleanup() {
    set +e
    log "cleaning up"
    [[ -n "$CTR" ]] && $CRICTL stop "$CTR" 2>/dev/null && $CRICTL rm "$CTR" 2>/dev/null
    [[ -n "$POD" ]] && $CRICTL stopp "$POD" 2>/dev/null && $CRICTL rmp "$POD" 2>/dev/null
    if [[ "$WITH_DAEMON" -eq 1 && -n "$DAEMON_PID" ]]; then
        kill "$DAEMON_PID" 2>/dev/null
        wait "$DAEMON_PID" 2>/dev/null
    fi
    if [[ "$FAILED" -eq 1 ]]; then
        echo
        echo ">>> some checks failed; daemon log (last 80 lines):"
        if systemctl is-active --quiet mydocker-cri 2>/dev/null; then
            journalctl -u mydocker-cri --no-pager -n 80
        elif [[ -f /tmp/mydocker-cri.log ]]; then
            tail -80 /tmp/mydocker-cri.log
        fi
    fi
}

# ---- preflight ----------------------------------------------------------
log "preflight"
for bin in mydocker mydocker-cri crictl; do
    if command -v "$bin" >/dev/null 2>&1; then
        ok "$bin at $(command -v $bin)"
    else
        fail "$bin not in PATH"
    fi
done

if [[ "$(stat -fc %T /sys/fs/cgroup 2>/dev/null)" != "cgroup2fs" ]]; then
    fail "cgroups v2 not active"
else
    ok "cgroups v2"
fi

if [[ ! -x /opt/cni/bin/bridge ]]; then
    fail "/opt/cni/bin/bridge missing — run vm-deploy.sh"
else
    ok "CNI bridge plugin present"
fi

if [[ ! -f /etc/cni/net.d/99-mydocker.conflist ]]; then
    fail "/etc/cni/net.d/99-mydocker.conflist missing"
else
    ok "mydocker CNI config present"
fi

[[ "$FAILED" -eq 1 ]] && { echo "preflight failed"; exit 1; }

# ---- check daemon status ------------------------------------------------
if [[ "$WITH_DAEMON" -eq 1 ]]; then
    log "starting mydocker-cri in background (--with-daemon mode)"
    rm -f "$SOCK"
    : > /tmp/mydocker-cri.log
    nohup mydocker-cri serve \
        --socket="$SOCK" \
        --streaming-addr=127.0.0.1:10350 \
        --cni-conf-dir=/etc/cni/net.d \
        --cni-bin-dir=/opt/cni/bin \
        --cgroup-driver=systemd \
        >/tmp/mydocker-cri.log 2>&1 &
    DAEMON_PID=$!
    ok "daemon PID=$DAEMON_PID"
    for i in $(seq 1 40); do
        [[ -S "$SOCK" ]] && break
        sleep 0.2
    done
    [[ -S "$SOCK" ]] || { fail "socket did not appear"; exit 1; }
else
    log "using mydocker-cri systemd service"
    if ! systemctl is-active --quiet mydocker-cri 2>/dev/null; then
        fail "service not active. Run: sudo systemctl start mydocker-cri"
        exit 1
    fi
    if [[ ! -S "$SOCK" ]]; then
        fail "socket $SOCK missing even though service is active"
        exit 1
    fi
    ok "socket ready: $SOCK"
fi

# ---- info + status ------------------------------------------------------
log "crictl info"
if ! INFO=$($CRICTL info 2>&1); then
    fail "crictl info failed: $INFO"
    exit 1
fi
echo "$INFO" | head -25

# The JSON output has "type": "RuntimeReady", on one line and "status": true,
# on the next. Use awk to pair them up.
read_ready() {
    echo "$INFO" | awk -v key="$1" '
        /"type":/ { gsub(/.*: "/, ""); gsub(/".*/, ""); t = $0 }
        /"status":/ { gsub(/.*: /, ""); gsub(/,.*/, ""); if (t == key) { print; exit } }
    '
}
if [[ "$(read_ready RuntimeReady)" == "true" ]]; then
    ok "RuntimeReady=true"
else
    fail "RuntimeReady not true"
fi
if [[ "$(read_ready NetworkReady)" == "true" ]]; then
    ok "NetworkReady=true"
else
    fail "NetworkReady not true — CNI config not loaded?"
fi

# ---- image pull ---------------------------------------------------------
log "crictl pull $IMAGE"
if PULL_OUT=$($CRICTL pull "$IMAGE" 2>&1); then
    ok "pull succeeded"
    echo "$PULL_OUT" | tail -3
else
    fail "pull failed: $PULL_OUT"
fi

log "crictl images"
$CRICTL images || fail "crictl images failed"

# ---- sandbox ------------------------------------------------------------
log "crictl runp (sandbox)"
mkdir -p /tmp/mydocker-logs
cat > /tmp/mydocker-sb.json <<'EOF'
{
    "metadata": {"name": "smoke-sb", "namespace": "default", "uid": "uid-smoke", "attempt": 1},
    "log_directory": "/tmp/mydocker-logs",
    "linux": {"cgroup_parent": "kubepods.slice"}
}
EOF
POD=$($CRICTL runp /tmp/mydocker-sb.json 2>&1)
RC=$?
if [[ $RC -ne 0 ]]; then
    fail "runp failed: $POD"
    POD=""
    exit 1
fi
ok "sandbox id: $POD"

log "crictl pods"
$CRICTL pods

log "crictl inspectp — check IP"
INSPECT=$($CRICTL inspectp -o json "$POD")
IP=$(echo "$INSPECT" | sed -n 's/.*"ip": "\([0-9.]*\)".*/\1/p' | head -1)
if [[ -n "$IP" && "$IP" != "0.0.0.0" ]]; then
    ok "sandbox IP: $IP"
else
    fail "sandbox has no IP"
fi

# 找 pause 进程（通过 ps grep，不依赖 inspectp 里的 pid 字段）
if pgrep -a -f '/usr/local/bin/mydocker pause' >/dev/null; then
    ok "pause process alive"
else
    fail "no pause process running"
fi

# ---- container ----------------------------------------------------------
log "crictl create + start (container)"
cat > /tmp/mydocker-ctr.json <<'EOF'
{
    "metadata": {"name": "smoke-ctr"},
    "image": {"image": "busybox"},
    "command": ["/bin/sh", "-c", "echo starting; while true; do date; sleep 2; done"],
    "log_path": "smoke-ctr.log",
    "linux": {
        "resources": {
            "memory_limit_in_bytes": 67108864,
            "cpu_quota": 50000,
            "cpu_period": 100000
        }
    }
}
EOF
CTR=$($CRICTL create "$POD" /tmp/mydocker-ctr.json /tmp/mydocker-sb.json 2>&1)
if [[ $? -ne 0 ]]; then
    fail "create failed: $CTR"
    CTR=""
    exit 1
fi
ok "container id: $CTR"

if $CRICTL start "$CTR" >/dev/null 2>&1; then
    ok "container started"
else
    fail "start failed"
fi

sleep 2

log "crictl ps"
$CRICTL ps

# ---- systemd scope ------------------------------------------------------
log "verify systemd scope"
SCOPE=$(systemctl list-units --no-pager --type=scope --all 2>/dev/null | awk '{print $1}' | grep "$CTR" | head -1)
if [[ -z "$SCOPE" ]]; then
    SCOPE=$(systemctl list-units --no-pager --type=scope --all 2>/dev/null | awk '{print $1}' | grep -E '^mydocker' | head -1)
fi
if [[ -n "$SCOPE" ]]; then
    ok "found scope: $SCOPE"
    systemctl show "$SCOPE" --property=MemoryMax,CPUQuotaPerSecUSec,ControlGroup 2>/dev/null | sed 's/^/   /'
else
    fail "no systemd scope for container — cgroup driver broken?"
fi

# ---- logs format --------------------------------------------------------
log "verify CRI log format"
RAW_LOG="/tmp/mydocker-logs/smoke-ctr.log"
if [[ -f "$RAW_LOG" ]]; then
    ok "log file: $RAW_LOG"
    if head -1 "$RAW_LOG" | grep -qE '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z (stdout|stderr) [FP] '; then
        ok "log format matches CRI spec"
    else
        fail "log format mismatch:"
        head -3 "$RAW_LOG"
    fi
    echo "   first 3 lines:"
    head -3 "$RAW_LOG" | sed 's/^/   /'
else
    fail "log file missing: $RAW_LOG"
fi

log "crictl logs (should render as plain text)"
$CRICTL logs "$CTR" 2>&1 | head -5 | sed 's/^/   /'

# ---- exec sync ----------------------------------------------------------
log "crictl exec -s (ExecSync)"
if OUT=$($CRICTL exec -s "$CTR" echo 'exec works' 2>&1); then
    if [[ "$OUT" == *"exec works"* ]]; then
        ok "ExecSync output: $OUT"
    else
        fail "ExecSync ran but output unexpected: $OUT"
    fi
else
    fail "ExecSync failed: $OUT"
fi

log "crictl exec -s ls /"
$CRICTL exec -s "$CTR" ls / | head -10 | sed 's/^/   /' || fail "ls in container failed"

# ---- result -------------------------------------------------------------
if [[ "$FAILED" -eq 0 ]]; then
    printf '\n\033[1;32m✓✓✓ ALL CHECKS PASSED ✓✓✓\033[0m\n'
    exit 0
else
    printf '\n\033[1;31m✗ SOME CHECKS FAILED ✗\033[0m\n'
    exit 1
fi
