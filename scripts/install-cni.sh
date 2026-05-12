#!/usr/bin/env bash
# Install the official CNI reference plugins and a minimal bridge config.
#
# Prerequisite: root (for /opt/cni/bin writes). Tested on Linux/amd64.
#
# Usage: sudo ./scripts/install-cni.sh
set -euo pipefail

VERSION="${CNI_PLUGINS_VERSION:-v1.5.1}"
ARCH="$(uname -m)"
case "$ARCH" in
    x86_64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
esac

BIN_DIR="${CNI_BIN_DIR:-/opt/cni/bin}"
CONF_DIR="${CNI_CONF_DIR:-/etc/cni/net.d}"

echo "== installing CNI plugins $VERSION to $BIN_DIR =="
mkdir -p "$BIN_DIR" "$CONF_DIR"
URL="https://github.com/containernetworking/plugins/releases/download/$VERSION/cni-plugins-linux-$ARCH-$VERSION.tgz"
TMP="$(mktemp -d)"
curl -fsSL "$URL" -o "$TMP/cni.tgz"
tar -C "$BIN_DIR" -xzf "$TMP/cni.tgz"
rm -rf "$TMP"

echo "== writing bridge conflist to $CONF_DIR/10-mydocker.conflist =="
cat > "$CONF_DIR/10-mydocker.conflist" <<'EOF'
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
                "ranges": [
                    [{ "subnet": "10.22.0.0/16" }]
                ],
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

echo
echo "OK. Restart mydocker-cri to pick up the new network."
echo "Verify with: ls $BIN_DIR/"
