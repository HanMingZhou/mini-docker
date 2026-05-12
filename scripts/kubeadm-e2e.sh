#!/usr/bin/env bash
# kubeadm-e2e.sh — 用 mydocker-cri 作为容器运行时，用 kubeadm 拉起完整 K8s 控制面。
#
# 这是 Level 4 的完整端到端测试：
#   1. 前置检查（mydocker-cri, kubeadm, kubelet, CNI）
#   2. kubeadm init
#   3. 验证控制面（etcd / apiserver / scheduler / controller-manager）全部 Running
#   4. kubectl get nodes 显示 Ready
#   5. CONTAINER-RUNTIME 字段显示 mydocker://
#
# 在 Linux VM 上以 root 运行：
#   sudo bash scripts/kubeadm-e2e.sh
#
# 前置：
#   - /usr/local/bin/mydocker 和 /usr/local/bin/mydocker-cri 已安装
#   - mydocker-cri systemd service 已运行（scripts/mydocker-cri.service）
#   - kubeadm / kubelet / kubectl 1.30.x 已安装
#   - CNI 插件已装（scripts/install-cni.sh）
#   - 节点 IP 为 192.168.252.2（或相应修改）
set -uo pipefail

SOCK=/var/run/mydocker-cri.sock
CRICTL="crictl --runtime-endpoint=unix://$SOCK"
KUBECONFIG_ADMIN=/etc/kubernetes/admin.conf

PASS=0
FAIL=0
log()  { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }
ok()   { PASS=$((PASS+1)); printf '   \033[32m✓\033[0m %s\n' "$*"; }
fail() { FAIL=$((FAIL+1)); printf '   \033[31m✗\033[0m %s\n' "$*"; }

cleanup() {
    set +e
    log "cleaning up"
    systemctl stop kubelet 2>/dev/null
    kubeadm reset -f 2>/dev/null
    pkill -9 kube 2>/dev/null
    pkill -9 etcd 2>/dev/null
    pkill -9 mydocker 2>/dev/null
    sleep 1
    # 清理 overlay mount 残留
    for m in $(mount | grep /var/lib/mydocker/containers | awk '{print $3}'); do
        umount -l "$m" 2>/dev/null
    done
    rm -rf /etc/kubernetes /var/lib/kubelet /var/lib/etcd /var/log/pods
    rm -rf /var/lib/mydocker/containers /var/lib/mydocker/sandboxes
    mkdir -p /var/lib/mydocker/containers /var/lib/mydocker/sandboxes
    systemctl restart mydocker-cri 2>/dev/null
}

# ============================================================================
log "preflight: 检查二进制与服务"
for bin in mydocker mydocker-cri kubeadm kubelet kubectl crictl; do
    if command -v "$bin" >/dev/null 2>&1; then
        ok "$bin found: $(command -v $bin)"
    else
        fail "$bin not found"
        exit 1
    fi
done

if systemctl is-active mydocker-cri >/dev/null 2>&1; then
    ok "mydocker-cri service active"
else
    fail "mydocker-cri service not active"
    systemctl status mydocker-cri --no-pager | tail -10
    exit 1
fi

# ============================================================================
log "重置任何旧的 kubeadm 状态"
cleanup 2>&1 | tail -3
sleep 2

# ============================================================================
log "CRI 自身冒烟：runtime+network ready"
RUNTIME_READY=$($CRICTL info 2>/dev/null | python3 -c "
import json,sys
d=json.load(sys.stdin)
for c in d['status']['conditions']:
    if c['type']=='RuntimeReady': print(c['status'])
" 2>/dev/null)
[[ "$RUNTIME_READY" = "True" ]] && ok "RuntimeReady=True" || fail "RuntimeReady=$RUNTIME_READY"

NET_READY=$($CRICTL info 2>/dev/null | python3 -c "
import json,sys
d=json.load(sys.stdin)
for c in d['status']['conditions']:
    if c['type']=='NetworkReady': print(c['status'])
" 2>/dev/null)
[[ "$NET_READY" = "True" ]] && ok "NetworkReady=True" || echo "   warn: NetworkReady=$NET_READY (CoreDNS 可能起不来，但控制面不受影响)"

# ============================================================================
log "kubeadm init（最多等 4 分钟）"
: > /tmp/kubeadm.log
if kubeadm init \
    --cri-socket=unix://$SOCK \
    --pod-network-cidr=10.22.0.0/16 \
    --kubernetes-version=v1.30.0 >> /tmp/kubeadm.log 2>&1; then
    ok "kubeadm init 成功"
else
    fail "kubeadm init 失败（完整日志在 /tmp/kubeadm.log）"
    tail -20 /tmp/kubeadm.log
    exit 1
fi

# ============================================================================
log "apiserver healthz"
export KUBECONFIG=$KUBECONFIG_ADMIN
if curl -sk --connect-timeout 5 https://192.168.252.2:6443/healthz | grep -q ok; then
    ok "apiserver /healthz 返回 ok"
else
    fail "apiserver /healthz 失败"
fi

# ============================================================================
log "kubectl get nodes"
NODE_STATUS=$(kubectl get nodes --no-headers 2>/dev/null | awk '{print $2}')
if [[ "$NODE_STATUS" = "Ready" ]]; then
    ok "node status: Ready"
else
    fail "node status: $NODE_STATUS"
fi

NODE_RUNTIME=$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.containerRuntimeVersion}' 2>/dev/null)
if [[ "$NODE_RUNTIME" = mydocker://* ]]; then
    ok "container runtime: $NODE_RUNTIME"
else
    fail "container runtime: $NODE_RUNTIME (expected mydocker://...)"
fi

# ============================================================================
log "控制面 Pod 状态"
# 给 apiserver 30 秒让所有 pod 状态稳定（包括自己上报）
sleep 30
for pod in etcd kube-apiserver kube-controller-manager kube-scheduler; do
    STATUS=$(kubectl -n kube-system get pod $pod-$(hostname) --no-headers 2>/dev/null | awk '{print $3}')
    if [[ "$STATUS" = "Running" ]]; then
        ok "$pod: Running"
    else
        fail "$pod: $STATUS"
    fi
done

# ============================================================================
log "CRI 里的容器"
RUNNING=$($CRICTL ps --no-trunc --quiet 2>/dev/null | wc -l)
if [[ $RUNNING -ge 4 ]]; then
    ok "$RUNNING 个容器 Running（预期 ≥4）"
else
    fail "只有 $RUNNING 个容器 Running（预期 ≥4）"
    $CRICTL ps -a | head -10
fi

# ============================================================================
echo ""
echo "=================================="
echo "  通过: $PASS / $((PASS+FAIL))"
[[ $FAIL -eq 0 ]] && echo "  \033[32mALL GREEN\033[0m — L4 kubeadm 全链路打通！" || echo "  \033[31m$FAIL 项失败\033[0m"
echo "=================================="

exit $FAIL
