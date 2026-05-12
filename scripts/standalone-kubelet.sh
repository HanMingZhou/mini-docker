#!/usr/bin/env bash
# standalone-kubelet.sh — 用 standalone kubelet 驱动我们的 CRI 跑一个 static pod。
#
# 不依赖 apiserver/etcd 等，只用 kubelet 读本地 manifest 目录。
# 证明 "Kubelet 能通过 mydocker-cri 创建和维持 Pod"。
#
# 在 VM 里以 root 运行。前置：mydocker-cri systemd service 已运行。
set -uo pipefail

SOCK=/var/run/mydocker-cri.sock
MANIFEST_DIR=/etc/kubernetes/manifests
CRICTL="crictl --runtime-endpoint=unix://$SOCK"

PASS=0
FAIL=0
log()  { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }
ok()   { PASS=$((PASS+1)); printf '   \033[32m✓\033[0m %s\n' "$*"; }
fail() { FAIL=$((FAIL+1)); printf '   \033[31m✗\033[0m %s\n' "$*"; }

cleanup() {
    set +e
    log "cleaning up"
    systemctl stop standalone-kubelet 2>/dev/null
    pkill -9 kubelet 2>/dev/null
    # Let CRI clean up pods that had the standalone-kubelet parent
    sleep 1
    for p in $($CRICTL pods --quiet 2>/dev/null); do
        $CRICTL stopp "$p" 2>/dev/null
        $CRICTL rmp "$p" 2>/dev/null
    done
    rm -f /etc/systemd/system/standalone-kubelet.service
    rm -rf "$MANIFEST_DIR"
}
trap cleanup EXIT

# ============================================================================
log "preflight"
for bin in mydocker mydocker-cri crictl kubelet; do
    if command -v "$bin" >/dev/null 2>&1; then
        ok "$bin"
    else
        fail "$bin not in PATH"
    fi
done
if [[ "$FAIL" -gt 0 ]]; then
    echo "preflight failed"
    exit 1
fi

# Make sure our CRI is up
if ! systemctl is-active --quiet mydocker-cri; then
    fail "mydocker-cri service not active"
    exit 1
fi
ok "mydocker-cri active"

# Pre-pull nginx and pause
log "pre-pulling images"
$CRICTL pull registry.k8s.io/pause:3.9 2>&1 | tail -1
$CRICTL pull nginx:alpine 2>&1 | tail -1
ok "images pulled"

# ============================================================================
log "writing static pod manifest"
mkdir -p "$MANIFEST_DIR"
cat > "$MANIFEST_DIR/nginx.yaml" <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: nginx
  namespace: default
spec:
  containers:
  - name: nginx
    image: nginx:alpine
    ports:
    - containerPort: 80
EOF
ok "manifest written at $MANIFEST_DIR/nginx.yaml"

# ============================================================================
log "starting standalone kubelet"
# Kubelet needs a minimal config. We point it at our CRI socket and
# disable all the things that want an apiserver.
KUBELET_CONFIG=/etc/kubernetes/standalone-kubelet-config.yaml
cat > "$KUBELET_CONFIG" <<EOF
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
staticPodPath: $MANIFEST_DIR
authentication:
  anonymous:
    enabled: true
  webhook:
    enabled: false
authorization:
  mode: AlwaysAllow
cgroupDriver: systemd
containerRuntimeEndpoint: unix://$SOCK
imageServiceEndpoint: unix://$SOCK
failSwapOn: false
# Disable everything that wants apiserver
registerNode: false
# Use a fake node name; kubelet tries to sync it but will just log warnings
# without apiserver.
EOF

# systemd unit for kubelet (not using /etc/systemd/system/kubelet.service which
# kubeadm installs; we write our own to avoid interference).
cat > /etc/systemd/system/standalone-kubelet.service <<EOF
[Unit]
Description=Standalone kubelet (no apiserver)
After=mydocker-cri.service

[Service]
Type=exec
ExecStart=/usr/bin/kubelet \\
    --config=$KUBELET_CONFIG \\
    --hostname-override=mydocker-dev \\
    --v=2
Restart=no
KillMode=process

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl start standalone-kubelet
sleep 2

if systemctl is-active --quiet standalone-kubelet; then
    ok "kubelet started"
else
    fail "kubelet failed to start"
    journalctl -u standalone-kubelet --no-pager -n 30
    exit 1
fi

# ============================================================================
log "waiting for kubelet to create nginx pod (up to 60s)"
for i in $(seq 1 30); do
    POD=$($CRICTL pods --name nginx --quiet 2>/dev/null | head -1)
    if [[ -n "$POD" ]]; then
        ok "sandbox created: $POD (after ${i}*2s)"
        break
    fi
    sleep 2
done

if [[ -z "$POD" ]]; then
    fail "sandbox never created"
    echo "--- kubelet log ---"
    journalctl -u standalone-kubelet --no-pager -n 40
    exit 1
fi

# Give kubelet extra time to create and start the nginx container
log "waiting for nginx container (up to 60s)"
for i in $(seq 1 30); do
    CTR=$($CRICTL ps -a --name nginx --quiet 2>/dev/null | head -1)
    if [[ -n "$CTR" ]]; then
        STATE=$($CRICTL inspect "$CTR" 2>/dev/null | grep '"state"' | head -1 | awk -F'"' '{print $4}')
        if [[ "$STATE" == "CONTAINER_RUNNING" ]]; then
            ok "container running: $CTR (after ${i}*2s)"
            break
        fi
    fi
    sleep 2
done

if [[ -z "$CTR" ]]; then
    fail "container never created"
    echo "--- kubelet log ---"
    journalctl -u standalone-kubelet --no-pager -n 40
    exit 1
fi

# ============================================================================
log "listing pods"
$CRICTL pods

log "listing containers"
$CRICTL ps

log "sandbox detail"
$CRICTL inspectp "$POD" 2>&1 | head -30

log "container detail"
$CRICTL inspect "$CTR" 2>&1 | grep -E '"state"|"image"|"createdAt"' | head -5

# ============================================================================
# Try to curl nginx via its CNI IP
log "fetching nginx index page"
IP=$($CRICTL inspectp "$POD" | grep '"ip"' | head -1 | awk -F'"' '{print $4}')
if [[ -n "$IP" && "$IP" != "0.0.0.0" ]]; then
    ok "pod IP: $IP"
    if curl -s --max-time 3 "http://$IP/" | grep -q -i 'welcome\|nginx'; then
        ok "nginx serving HTML"
    else
        fail "nginx not reachable at $IP (may need CNI MASQUERADE)"
    fi
else
    fail "no IP assigned"
fi

# ============================================================================
log "simulating restart: kill the nginx container"
BEFORE=$($CRICTL ps --quiet | head -1)
$CRICTL stop "$CTR"
sleep 1

# Wait for kubelet to recreate
log "waiting for kubelet to recreate (up to 30s)"
for i in $(seq 1 15); do
    AFTER=$($CRICTL ps --name nginx --quiet 2>/dev/null | head -1)
    if [[ -n "$AFTER" && "$AFTER" != "$BEFORE" ]]; then
        ok "new container id: $AFTER (kubelet restarted it)"
        break
    fi
    sleep 2
done

if [[ -z "$AFTER" || "$AFTER" == "$BEFORE" ]]; then
    fail "kubelet did not restart the container"
fi

# ============================================================================
echo
echo "============================================"
printf "  PASSED: %d   FAILED: %d\n" "$PASS" "$FAIL"
echo "============================================"
if [[ "$FAIL" -eq 0 ]]; then
    printf '\033[1;32m  STANDALONE KUBELET INTEGRATION PASSED\033[0m\n'
    printf '  Kubelet successfully drove mydocker-cri to run and manage an nginx Pod.\n'
    exit 0
else
    printf '\033[1;31m  SOME TESTS FAILED\033[0m\n'
    exit 1
fi
