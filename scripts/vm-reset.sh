#!/usr/bin/env bash
# vm-reset.sh — 彻底清理 VM 上的 mydocker 和 kubelet 状态。
# 在 VM 里以 root 运行。
set +e

echo "=== stopping services ==="
systemctl stop kubelet 2>/dev/null
systemctl stop mydocker-cri 2>/dev/null

echo "=== killing stale processes ==="
pkill -9 etcd 2>/dev/null
pkill -9 kube 2>/dev/null
pkill -9 mydocker 2>/dev/null
sleep 1

echo "=== unmounting stale overlay mounts ==="
# Unmount anything under /var/lib/mydocker/containers/*/merged
for m in $(mount | grep /var/lib/mydocker/containers | awk '{print $3}'); do
    umount -l "$m" 2>/dev/null
done
# Also unmount any bind mounts still active
for m in $(mount | grep /var/lib/mydocker | awk '{print $3}'); do
    umount -l "$m" 2>/dev/null
done

echo "=== cleaning directories ==="
rm -rf /etc/kubernetes /var/lib/kubelet /var/lib/etcd
rm -rf /var/lib/mydocker/containers
rm -rf /var/lib/mydocker/sandboxes
mkdir -p /var/lib/mydocker/containers /var/lib/mydocker/sandboxes

echo "=== flushing iptables ==="
iptables -t nat -F 2>/dev/null
iptables -F 2>/dev/null

echo "=== cleaning CNI state ==="
rm -rf /var/lib/cni/networks/* 2>/dev/null

echo "=== restarting mydocker-cri ==="
systemctl restart mydocker-cri
sleep 1
systemctl is-active mydocker-cri

echo "=== final check ==="
crictl --runtime-endpoint=unix:///var/run/mydocker-cri.sock pods 2>&1 | head -3
echo "done"
