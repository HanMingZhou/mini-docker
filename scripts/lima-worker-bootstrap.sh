#!/usr/bin/env bash
# lima-worker-bootstrap.sh — 在 lima `mydocker-worker` VM 里搭建 K8s worker，
# 准备 kubeadm join 到 `mydocker` control-plane。
#
# 设计为可重入：每段做完打 marker，重跑跳过已完成。
#
# 用法（在 worker VM 内 root 跑）：
#   sudo bash /Users/jerrytom/go/src/mini-docker/scripts/lima-worker-bootstrap.sh

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
    # Worker 不需要也不应该有 go/task：直接用 Mac 上交叉编译好的 bin/
    # (task build:linux 产物)。lima 会反向 mount Mac $HOME 到 /Users/...
    SRC=/Users/jerrytom/go/src/mini-docker
    if [ ! -x "$SRC/bin/mydocker" ] || [ ! -x "$SRC/bin/mydocker-cri" ]; then
        echo "  ! Mac 端 bin/mydocker* 不存在，请先在 Mac 上跑: task build:linux" >&2
        return 1
    fi
    install -m 0755 "$SRC/bin/mydocker"     /usr/local/bin/mydocker
    install -m 0755 "$SRC/bin/mydocker-cri" /usr/local/bin/mydocker-cri
    /usr/local/bin/mydocker-cri --help 2>&1 | head -3 || return 1
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
    bash /Users/jerrytom/go/src/mini-docker/scripts/install-cni.sh
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
    crictl info | grep -E '"Ready":|"network":' || true
}

# --------------------------------------------------------------------- 10
preflight_pull() {
    crictl pull registry.k8s.io/pause:3.9
    crictl pull registry.k8s.io/kube-proxy:v1.30.14
    crictl images
    return 0
}

# ============================================================================

if [ "$EUID" -ne 0 ]; then
    echo "Run as root (sudo)" >&2
    exit 1
fi

if [ -z "${SUDO_USER:-}" ]; then
    SUDO_USER="jerrytom"
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

echo
echo "=========================================================="
echo " Worker prerequisites ready."
echo " Now in the CONTROL PLANE VM, generate a join token:"
echo
echo "   sudo kubeadm token create --print-join-command"
echo
echo " Copy the printed 'kubeadm join ...' command and run it"
echo " HERE (with --cri-socket appended):"
echo
echo "   unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY"
echo "   sudo kubeadm join <CONTROL_PLANE_IP>:6443 \\"
echo "       --token <TOKEN> \\"
echo "       --discovery-token-ca-cert-hash sha256:<HASH> \\"
echo "       --cri-socket=unix:///var/run/mydocker-cri.sock"
echo "=========================================================="
