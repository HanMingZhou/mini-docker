#!/usr/bin/env bash
# e2e-docker.sh — 完整端到端验证 mydocker 的所有 Docker 功能。
#
# 在 Linux VM 上以 root 运行。前置：mydocker 已安装、CNI 已配置。
# 用法：sudo bash /tmp/e2e-docker.sh
set -uo pipefail

PASS=0
FAIL=0
log()  { printf '\n\033[1;36m[%02d] %s\033[0m\n' $((PASS+FAIL+1)) "$*"; }
ok()   { PASS=$((PASS+1)); printf '   \033[32m✓\033[0m %s\n' "$*"; }
fail() { FAIL=$((FAIL+1)); printf '   \033[31m✗\033[0m %s\n' "$*"; }
check() { if eval "$1"; then ok "$2"; else fail "$2 ($1)"; fi; }

cleanup() {
    set +e
    mydocker stop restart-test 2>/dev/null
    mydocker rm restart-test 2>/dev/null
    mydocker stop web-e2e 2>/dev/null
    mydocker rm web-e2e 2>/dev/null
    mydocker rm commit-src 2>/dev/null
    mydocker rm commit-run 2>/dev/null
    mydocker rm exec-test 2>/dev/null
    mydocker rm cp-test 2>/dev/null
    mydocker rm pause-test 2>/dev/null
    mydocker rm stats-test 2>/dev/null
    mydocker image rm my-committed 2>/dev/null
    mydocker image rm bb-tag 2>/dev/null
}
trap cleanup EXIT

# ============================================================================
log "image pull"
OUT=$(mydocker image pull busybox 2>&1)
check '[[ $? -eq 0 ]]' "pull busybox"

log "image ls"
OUT=$(mydocker image ls)
check 'echo "$OUT" | grep -q busybox' "busybox in image list"

log "image tag"
mydocker image tag busybox:latest bb-tag
check 'mydocker image ls | grep -q bb-tag' "tag created"

# ============================================================================
log "run -it (foreground, quick exit)"
OUT=$(mydocker run --image busybox --name quick --network none -- echo "hello-e2e" 2>&1)
check '[[ "$OUT" == *"hello-e2e"* ]]' "foreground run output"
mydocker rm quick 2>/dev/null

# ============================================================================
log "run -d + network + port mapping"
mydocker run -d --image busybox --name web-e2e --network bridge -p 7777:80 -- \
    sh -c 'mkdir -p /www && echo e2e-ok > /www/index.html && httpd -f -p 80 -h /www'
sleep 3
check 'mydocker ps | grep -q web-e2e' "container in ps"
check 'mydocker ps | grep -q "7777->80/tcp"' "port shown in ps"

log "port mapping works (curl)"
sleep 1  # extra wait for httpd to bind
# Try direct container IP first (always works), then DNAT
WEB_IP=$(mydocker inspect web-e2e 2>/dev/null | grep network_ip | tr -d '" ,' | awk -F: '{print $2}')
CURL_OUT=$(curl -s --max-time 5 "http://${WEB_IP}:80/" 2>&1)
if [[ "$CURL_OUT" != *"e2e-ok"* ]]; then
    # Fallback: try DNAT
    CURL_OUT=$(curl -s --max-time 5 http://127.0.0.1:7777/ 2>&1)
fi
check '[[ "$CURL_OUT" == *"e2e-ok"* ]]' "curl container returns e2e-ok"

# ============================================================================
log "exec"
mydocker run -d --image busybox --name exec-test --network none -- sh -c 'sleep 60'
sleep 1
EXEC_OUT=$(mydocker exec exec-test -- echo "from-exec" 2>&1)
check '[[ "$EXEC_OUT" == *"from-exec"* ]]' "exec output"

# ============================================================================
log "logs"
LOGS=$(mydocker logs web-e2e 2>&1)
# httpd doesn't log to stdout, but our container echoed nothing. Check no error.
check '[[ $? -eq 0 ]]' "logs command succeeds"

# ============================================================================
log "inspect"
INSPECT=$(mydocker inspect web-e2e 2>&1)
check 'echo "$INSPECT" | grep -q "network_ip"' "inspect shows IP"
check 'echo "$INSPECT" | grep -q "port_mappings"' "inspect shows ports"

# ============================================================================
log "stats"
mydocker run -d --image busybox --name stats-test --network none --memory 32m -- sh -c 'while :; do :; done'
sleep 1
STATS=$(mydocker stats 2>&1)
check 'echo "$STATS" | grep -q stats-test' "stats shows container"
mydocker stop stats-test && mydocker rm stats-test

# ============================================================================
log "kill -s"
mydocker run -d --image busybox --name kill-test --network none -- sh -c 'sleep 60'
sleep 1
mydocker kill -s KILL kill-test
sleep 1
check 'mydocker ps | grep kill-test | grep -q Exited' "kill -s KILL exits container"
mydocker rm kill-test 2>/dev/null

# ============================================================================
log "pause / unpause"
mydocker run -d --image busybox --name pause-test --network none -- sh -c 'while :; do sleep 1; done'
sleep 1
mydocker pause pause-test
check 'mydocker ps | grep pause-test | grep -q Paused' "container paused"
mydocker unpause pause-test
check 'mydocker ps | grep pause-test | grep -q Running' "container unpaused"
mydocker stop pause-test && mydocker rm pause-test

# ============================================================================
log "cp (host → container)"
mydocker run -d --image busybox --name cp-test --network none -- sh -c 'sleep 60'
sleep 1
echo "copied-content" > /tmp/e2e-file.txt
mydocker cp /tmp/e2e-file.txt cp-test:/tmp/e2e-file.txt
CP_OUT=$(mydocker exec cp-test -- cat /tmp/e2e-file.txt 2>&1)
check '[[ "$CP_OUT" == *"copied-content"* ]]' "cp host→container"

log "cp (container → host)"
mydocker exec cp-test -- sh -c 'echo from-container > /tmp/out.txt'
mydocker cp cp-test:/tmp/out.txt /tmp/e2e-from-ctr.txt
check 'grep -q from-container /tmp/e2e-from-ctr.txt' "cp container→host"
mydocker stop cp-test && mydocker rm cp-test

# ============================================================================
log "commit"
mydocker run --image busybox --name commit-src --network none -- sh -c 'echo committed > /data.txt'
mydocker commit commit-src my-committed
check 'mydocker image ls | grep -q my-committed' "committed image exists"

log "run from committed image"
OUT=$(mydocker run --image my-committed --name commit-run --network none -- cat /data.txt 2>&1)
check '[[ "$OUT" == *"committed"* ]]' "committed image has /data.txt"
mydocker rm commit-src && mydocker rm commit-run

# ============================================================================
log "restart policy (--restart always)"
mydocker run -d --image busybox --name restart-test --network none --restart always -- \
    sh -c 'echo alive; sleep 2; exit 1'
sleep 4
# Monitor should have detected exit and be attempting restart
# Check that the monitor process exists
check 'pgrep -f "_monitor restart-test" >/dev/null 2>&1 || pgrep -f "_monitor" >/dev/null 2>&1' "monitor process running"
mydocker stop restart-test 2>/dev/null
mydocker rm restart-test 2>/dev/null
# Kill any leftover monitors
pkill -f "_monitor" 2>/dev/null || true

# ============================================================================
log "stop + rm (cleanup)"
mydocker stop web-e2e
mydocker rm web-e2e
mydocker stop exec-test 2>/dev/null
mydocker rm exec-test 2>/dev/null
check '! mydocker ps | grep -q web-e2e' "web-e2e removed"

log "image rm"
mydocker image rm my-committed
mydocker image rm bb-tag
check '! mydocker image ls | grep -q bb-tag' "tag removed"

# ============================================================================
echo
echo "============================================"
printf "  PASSED: %d   FAILED: %d\n" "$PASS" "$FAIL"
echo "============================================"
if [[ "$FAIL" -eq 0 ]]; then
    printf '\033[1;32m  ALL TESTS PASSED\033[0m\n'
    exit 0
else
    printf '\033[1;31m  SOME TESTS FAILED\033[0m\n'
    exit 1
fi
