#!/usr/bin/env bash
# lima-k8s-bootstrap.sh — 在 lima `mydocker` VM 里完整搭起 K8s + mydocker-cri。
#
# 设计为**可重入**：每一段做完打 marker，重跑会跳过已完成的步骤。
#
# 用法（在 VM 内 root 跑）：
#   sudo bash /Users/jerrytom/go/src/mini-docker/scripts/lima-k8s-bootstrap.sh
#
# 完整后会执行 `kubeadm init` 并打印结果。
set -uo pipefail

MARKER_DIR=/var/lib/mydocker/.bootstrap
mkdir -p "$MARKER_DIR"

step() {
    local name="$1"; shift
    if [ -f "$MARKER_DIR/$name" ]; then
        echo "==[skip] $name"
        return 0
    fi
    echo
    echo "==[$name]=========================================="
    if "$@"; then
        touch "$MARKER_DIR/$name"
        echo "  ✓ $name done"
    else
        echo "  ✗ $name FAILED"
        exit 1
    fi
}

# --------------------------------------------------------------------- 1
sync_and_build() {
    # lima 的真实 user 名和 home 目录路径不一致：whoami 返回 jerrytom，
    # 但 home 是 /home/jerrytom.guest。getent 查实际 home。
    REAL_HOME=$(getent passwd "$SUDO_USER" | cut -d: -f6)
    if [ -z "$REAL_HOME" ] || [ ! -d "$REAL_HOME" ]; then
        REAL_HOME="/home/$SUDO_USER"
    fi
    DST="$REAL_HOME/mini-docker"
    mkdir -p "$DST"
    chown "$SUDO_USER" "$REAL_HOME"
    rsync -a --delete --exclude bin/ --exclude .git/ \
        /Users/jerrytom/go/src/mini-docker/ "$DST"/
    chown -R "$SUDO_USER:$SUDO_USER" "$DST"
    # 在 build 之前进入 VM-local 目录（不是 read-only mount）
    cd "$DST"
    rm -rf bin/
    # 用 GOFLAGS 禁用 VCS stamping，因为 .git 没拷过来
    sudo -u "$SUDO_USER" env GOFLAGS=-buildvcs=false /usr/local/bin/task build
    install -m 0755 bin/mydocker     /usr/local/bin/mydocker
    install -m 0755 bin/mydocker-cri /usr/local/bin/mydocker-cri
    cd - >/dev/null
}

# --------------------------------------------------------------------- 2
prep_kernel() {
    cat <<EOF >/etc/modules-load.d/k8s.conf
overlay
br_netfilter
EOF
    modprobe overlay
    modprobe br_netfilter
    cat <<EOF >/etc/sysctl.d/99-k8s.conf
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
net.ipv4.conf.all.route_localnet    = 1
EOF
    sysctl --system >/dev/null
    swapoff -a
    sed -i '/ swap / s/^/#/' /etc/fstab
}

# --------------------------------------------------------------------- 3
install_cni() {
    if [ -x /opt/cni/bin/bridge ]; then return 0; fi
    REAL_HOME=$(getent passwd "$SUDO_USER" | cut -d: -f6)
    cd "$REAL_HOME/mini-docker"
    bash scripts/install-cni.sh
}

# --------------------------------------------------------------------- 4
install_crictl() {
    if command -v crictl >/dev/null 2>&1; then return 0; fi
    local V=v1.30.0
    curl -fsSL "https://github.com/kubernetes-sigs/cri-tools/releases/download/${V}/crictl-${V}-linux-amd64.tar.gz" \
      | tar -C /usr/local/bin -xz
    chmod +x /usr/local/bin/crictl
}

# --------------------------------------------------------------------- 5
configure_crictl() {
    cat >/etc/crictl.yaml <<'EOF'
runtime-endpoint: unix:///var/run/mydocker-cri.sock
image-endpoint: unix:///var/run/mydocker-cri.sock
timeout: 10
EOF
}

# --------------------------------------------------------------------- 6
install_mydocker_cri_service() {
    cat >/etc/systemd/system/mydocker-cri.service <<'EOF'
[Unit]
Description=mydocker CRI server
After=network.target

[Service]
Type=exec
ExecStart=/usr/local/bin/mydocker-cri serve \
    --socket=/var/run/mydocker-cri.sock \
    --streaming-addr=127.0.0.1:10350 \
    --cni-conf-dir=/etc/cni/net.d \
    --cni-bin-dir=/opt/cni/bin
Restart=always
RestartSec=2
LimitNOFILE=1048576
Delegate=yes
KillMode=process

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable --now mydocker-cri
    sleep 2
    systemctl is-active mydocker-cri
}

# --------------------------------------------------------------------- 7
install_k8s_packages() {
    if command -v kubeadm >/dev/null 2>&1; then return 0; fi
    apt-get update -qq
    apt-get install -y -qq apt-transport-https ca-certificates curl gpg
    mkdir -p -m 755 /etc/apt/keyrings
    curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.30/deb/Release.key \
        | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
    echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.30/deb/ /' \
        > /etc/apt/sources.list.d/kubernetes.list
    apt-get update -qq
    apt-get install -y -qq kubelet=1.30.* kubeadm=1.30.* kubectl=1.30.*
    apt-mark hold kubelet kubeadm kubectl
}

# --------------------------------------------------------------------- 8
configure_kubelet() {
    mkdir -p /etc/systemd/system/kubelet.service.d
    cat >/etc/systemd/system/kubelet.service.d/20-mydocker.conf <<'EOF'
[Service]
Environment="KUBELET_EXTRA_ARGS=--container-runtime-endpoint=unix:///var/run/mydocker-cri.sock --image-service-endpoint=unix:///var/run/mydocker-cri.sock --pod-infra-container-image=registry.k8s.io/pause:3.9"
EOF
    systemctl daemon-reload
}

# --------------------------------------------------------------------- 9
verify_cri() {
    sleep 2
    crictl info | head -20
    crictl info | grep -E '"Ready":|"network":' || true
}

# --------------------------------------------------------------------- 10
preflight_pull() {
    crictl pull registry.k8s.io/pause:3.9
    crictl pull registry.k8s.io/kube-apiserver:v1.30.14
    crictl pull registry.k8s.io/kube-controller-manager:v1.30.14
    crictl pull registry.k8s.io/kube-scheduler:v1.30.14
    crictl pull registry.k8s.io/kube-proxy:v1.30.14
    crictl pull registry.k8s.io/etcd:3.5.15-0
    crictl pull registry.k8s.io/coredns/coredns:v1.11.3
    # 列出已缓存镜像，pipefail 下避免 head SIGPIPE 让 crictl 返回非零
    crictl images
    return 0
}

# --------------------------------------------------------------------- 11
kubeadm_reset_clean() {
    systemctl stop kubelet 2>/dev/null
    kubeadm reset -f --cri-socket=unix:///var/run/mydocker-cri.sock 2>/dev/null
    rm -rf /etc/kubernetes /var/lib/kubelet /var/lib/etcd /var/log/pods
    rm -rf /var/lib/mydocker/containers/* /var/lib/mydocker/sandboxes/*
    iptables -F; iptables -t nat -F; iptables -t mangle -F; iptables -X 2>/dev/null
    systemctl restart mydocker-cri
    sleep 2
    return 0
}

# --------------------------------------------------------------------- 12
kubeadm_init() {
    local IP
    IP=$(ip -4 addr show eth0 | awk '/inet / {print $2}' | cut -d/ -f1)
    echo "==> using node IP: $IP"
    # 关键：lima 默认设置 http_proxy 指向 host (192.168.5.2:7890)，
    # 这会让 kubeadm 的 healthcheck 走代理，结果代理打不通本机 6443，超时失败。
    # 必须显式 unset 代理变量并配 NO_PROXY 让 kubeadm 直连本机服务。
    unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY
    export NO_PROXY="127.0.0.1,localhost,$IP,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,.svc,.cluster.local"
    export no_proxy="$NO_PROXY"
    kubeadm init \
        --cri-socket=unix:///var/run/mydocker-cri.sock \
        --apiserver-advertise-address="$IP" \
        --pod-network-cidr=10.244.0.0/16 \
        --kubernetes-version=v1.30.14 \
        --ignore-preflight-errors=Mem,HTTPProxy,HTTPProxyCIDR \
        2>&1 | tee /tmp/kubeadm-init.log
    test "${PIPESTATUS[0]}" -eq 0
}

# --------------------------------------------------------------------- 13
kubectl_setup() {
    REAL_HOME=$(getent passwd "$SUDO_USER" | cut -d: -f6)
    mkdir -p "$REAL_HOME/.kube"
    cp /etc/kubernetes/admin.conf "$REAL_HOME/.kube/config"
    chown -R "$SUDO_USER:$SUDO_USER" "$REAL_HOME/.kube"
    mkdir -p /root/.kube
    cp /etc/kubernetes/admin.conf /root/.kube/config
    # 单节点：去 control-plane taint
    KUBECONFIG=/etc/kubernetes/admin.conf kubectl taint nodes --all node-role.kubernetes.io/control-plane- 2>/dev/null || true
}

# --------------------------------------------------------------------- 14
verify_cluster() {
    export KUBECONFIG=/etc/kubernetes/admin.conf
    echo
    echo "=== nodes ==="
    kubectl get nodes -o wide
    echo
    echo "=== control-plane pods ==="
    kubectl get pods -n kube-system
    echo
    echo "=== runtime version (should show mydocker://) ==="
    kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.containerRuntimeVersion}'
    echo
}

# ============================================================================
# Run
# ============================================================================

if [ "$EUID" -ne 0 ]; then
    echo "Run as root (sudo)" >&2
    exit 1
fi

# 当 sudo 调用时 SUDO_USER 是真实用户；直接 root 时是空，用 jerrytom.guest 兜底
if [ -z "${SUDO_USER:-}" ]; then
    SUDO_USER="jerrytom.guest"
fi

step "1-sync-build"      sync_and_build
step "2-kernel-prep"     prep_kernel
step "3-cni"             install_cni
step "4-crictl"          install_crictl
step "5-crictl-conf"     configure_crictl
step "6-mycri-service"   install_mydocker_cri_service
step "7-k8s-pkgs"        install_k8s_packages
step "8-kubelet-conf"    configure_kubelet
step "9-cri-check"       verify_cri
step "10-pull-images"    preflight_pull
step "11-reset"          kubeadm_reset_clean
step "12-kubeadm-init"   kubeadm_init
step "13-kubectl-setup"  kubectl_setup
verify_cluster

echo
echo "=========================================================="
echo " ALL DONE. Try:"
echo "    sudo -u $SUDO_USER kubectl get nodes -o wide"
echo "    sudo -u $SUDO_USER kubectl get pods -A"
echo "=========================================================="
