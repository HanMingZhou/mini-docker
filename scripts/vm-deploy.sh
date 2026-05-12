#!/usr/bin/env bash
# vm-deploy.sh — 把 mydocker / mydocker-cri 部署到一台 multipass VM。
#
# 用法：
#   ./scripts/vm-deploy.sh                # 默认 VM 名 mydocker-dev
#   ./scripts/vm-deploy.sh --name myvm    # 指定 VM
#   ./scripts/vm-deploy.sh --skip-build   # 跳过 make build-linux
#
# VM 需要满足：Ubuntu 22.04+、cgroups v2、systemd、x86_64。
# 第一次运行会在 VM 里装 crictl + CNI 插件，耗时大概 30s。
set -euo pipefail

VM_NAME="mydocker-dev"
SKIP_BUILD=0
i=1
while [[ $i -le $# ]]; do
    eval "arg=\${$i}"
    case "$arg" in
        --skip-build) SKIP_BUILD=1 ;;
        --name)       i=$((i+1)); eval "VM_NAME=\${$i}" ;;
        --name=*)     VM_NAME="${arg#--name=}" ;;
    esac
    i=$((i+1))
done

if ! command -v multipass >/dev/null 2>&1; then
    echo "multipass not found. Install with: brew install multipass" >&2
    exit 1
fi

STATE=$(multipass info "$VM_NAME" 2>/dev/null | awk -F': *' '/^State:/ {print $2}')
if [[ -z "$STATE" ]]; then
    echo "VM '$VM_NAME' not found. Launch with:" >&2
    echo "  multipass launch --cpus 4 --memory 4G --disk 20G --name $VM_NAME 22.04" >&2
    exit 1
fi
if [[ "$STATE" != "Running" ]]; then
    echo "VM '$VM_NAME' is '$STATE'. Starting..."
    multipass start "$VM_NAME"
fi

# Arch check
NODE_ARCH=$(multipass exec "$VM_NAME" -- uname -m | tr -d '\r\n')
case "$NODE_ARCH" in
    x86_64)  GOARCH=amd64 ;;
    aarch64) GOARCH=arm64 ;;
    *)       echo "unknown VM arch: $NODE_ARCH" >&2; exit 1 ;;
esac
echo "==> VM: $VM_NAME ($NODE_ARCH → GOARCH=$GOARCH)"

# 1. Build Linux binaries
if [[ "$SKIP_BUILD" -eq 0 ]]; then
    echo "==> building linux/$GOARCH binaries"
    case "$GOARCH" in
        amd64) make build-linux ;;
        arm64) make build-linux-arm64 ;;
    esac
fi
for bin in bin/mydocker bin/mydocker-cri; do
    [[ -x "$bin" ]] || { echo "$bin missing; run without --skip-build" >&2; exit 1; }
done

# 2. Transfer binaries
echo "==> transferring binaries"
multipass transfer bin/mydocker     "$VM_NAME":/tmp/mydocker
multipass transfer bin/mydocker-cri "$VM_NAME":/tmp/mydocker-cri
multipass exec "$VM_NAME" -- sudo install -m 0755 /tmp/mydocker     /usr/local/bin/mydocker
multipass exec "$VM_NAME" -- sudo install -m 0755 /tmp/mydocker-cri /usr/local/bin/mydocker-cri
echo "    installed to /usr/local/bin/"

# If service already running from a previous deploy, restart it to pick up new binary.
if multipass exec "$VM_NAME" -- systemctl is-active --quiet mydocker-cri 2>/dev/null; then
    echo "    restarting mydocker-cri to pick up new binary"
    multipass exec "$VM_NAME" -- sudo systemctl restart mydocker-cri
fi

# 3. Install crictl (if missing)
if ! multipass exec "$VM_NAME" -- bash -c 'command -v crictl' >/dev/null 2>&1; then
    echo "==> installing crictl v1.30.0"
    multipass exec "$VM_NAME" -- bash -c '
        set -e
        VERSION=v1.30.0
        curl -fsSL "https://github.com/kubernetes-sigs/cri-tools/releases/download/$VERSION/crictl-$VERSION-linux-'"$GOARCH"'.tar.gz" \
            | sudo tar -C /usr/local/bin -xz
        command -v crictl
    '
else
    echo "==> crictl already installed"
fi

# 4. Install CNI plugins (if missing)
if ! multipass exec "$VM_NAME" -- test -x /opt/cni/bin/bridge; then
    echo "==> installing CNI reference plugins v1.5.1"
    multipass exec "$VM_NAME" -- bash -c '
        set -e
        VERSION=v1.5.1
        sudo mkdir -p /opt/cni/bin
        curl -fsSL "https://github.com/containernetworking/plugins/releases/download/$VERSION/cni-plugins-linux-'"$GOARCH"'-$VERSION.tgz" \
            | sudo tar -C /opt/cni/bin -xz
    '
else
    echo "==> CNI plugins already installed"
fi

# 5. Write CNI config (idempotent)
echo "==> writing /etc/cni/net.d/99-mydocker.conflist"
TMP_CONF="$(mktemp)"
trap 'rm -f "$TMP_CONF"' EXIT
cat > "$TMP_CONF" <<'EOF'
{
    "cniVersion": "1.0.0",
    "name": "mydocker",
    "plugins": [
        {
            "type": "bridge",
            "bridge": "mydocker0",
            "isGateway": true,
            "ipMasq": true,
            "hairpinMode": true,
            "ipam": {
                "type": "host-local",
                "ranges": [[{ "subnet": "10.33.0.0/16" }]],
                "routes": [{ "dst": "0.0.0.0/0" }]
            }
        },
        {
            "type": "portmap",
            "capabilities": { "portMappings": true },
            "snat": true
        },
        {
            "type": "firewall"
        }
    ]
}
EOF
multipass transfer "$TMP_CONF" "$VM_NAME":/tmp/99-mydocker.conflist
multipass exec "$VM_NAME" -- sudo mkdir -p /etc/cni/net.d
multipass exec "$VM_NAME" -- sudo install -m 0644 /tmp/99-mydocker.conflist /etc/cni/net.d/99-mydocker.conflist

# 6. kernel modules + sysctls (bridge netfilter, ip_forward)
echo "==> kernel modules and sysctls"
multipass exec "$VM_NAME" -- sudo bash -c '
    modprobe overlay 2>/dev/null || true
    modprobe br_netfilter 2>/dev/null || true
    cat >/etc/sysctl.d/99-mydocker.conf <<SYSCTL
net.bridge.bridge-nf-call-iptables=1
net.ipv4.ip_forward=1
SYSCTL
    sysctl -p /etc/sysctl.d/99-mydocker.conf >/dev/null 2>&1 || true
'

# 7. Transfer smoke script
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -f "$SCRIPT_DIR/vm-smoke.sh" ]]; then
    multipass transfer "$SCRIPT_DIR/vm-smoke.sh" "$VM_NAME":/tmp/vm-smoke.sh
    multipass exec "$VM_NAME" -- sudo chmod +x /tmp/vm-smoke.sh
    echo "==> smoke script installed at /tmp/vm-smoke.sh"
fi

# 8. Install systemd service — 让 daemon 脱离 shell session，由 systemd 直接管
if [[ -f "$SCRIPT_DIR/mydocker-cri.service" ]]; then
    echo "==> installing mydocker-cri.service"
    multipass transfer "$SCRIPT_DIR/mydocker-cri.service" "$VM_NAME":/tmp/mydocker-cri.service
    multipass exec "$VM_NAME" -- sudo install -m 0644 /tmp/mydocker-cri.service /etc/systemd/system/mydocker-cri.service
    multipass exec "$VM_NAME" -- sudo systemctl daemon-reload
    multipass exec "$VM_NAME" -- sudo systemctl enable --now mydocker-cri
    # 给 daemon 1s 起来
    sleep 1
    if multipass exec "$VM_NAME" -- systemctl is-active --quiet mydocker-cri; then
        echo "    mydocker-cri.service is active"
    else
        echo "    WARN: mydocker-cri.service failed to start"
        multipass exec "$VM_NAME" -- sudo journalctl -u mydocker-cri --no-pager -n 30 || true
    fi
fi

cat <<DONE

==> 部署完成

  # 一键跑完整 smoke test（daemon 已作为 systemd service 在运行）
  multipass exec $VM_NAME -- sudo bash /tmp/vm-smoke.sh

  # 看 daemon 日志
  multipass exec $VM_NAME -- sudo journalctl -u mydocker-cri -f

  # 重启 daemon（改代码后）
  ./scripts/vm-deploy.sh --skip-build   # 重新部署二进制 + 自动 restart
  # 或直接
  multipass exec $VM_NAME -- sudo systemctl restart mydocker-cri

DONE
