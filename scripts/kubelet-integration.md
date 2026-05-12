# 接入 Kubelet 的部署指南（Level 4 MVP）

这是从 Level 3（CRI 独立可用）走到 Level 4（Kubelet 把 mydocker-cri 当成运行时）的完整流程。目标是在**单节点 Linux VM** 上，用 kubeadm 拉起集群，让 mydocker-cri 作为唯一的容器运行时，跑通 `kubectl run nginx`。

## 前置环境

- **Linux** x86_64 VM，内核 5.10+（Ubuntu 22.04+ / Debian 12 推荐）
- **cgroups v2**（`stat -fc %T /sys/fs/cgroup/` 输出 `cgroup2fs`）
- **systemd** 初始化系统
- 2 CPU / 2GB RAM 起步
- 关闭 swap：`sudo swapoff -a && sudo sed -i '/ swap / s/^/#/' /etc/fstab`
- 关闭 SELinux 或设为 permissive（`setenforce 0`）
- 关闭 firewalld 或放行相关端口
- **root** 权限

## 1. 准备内核模块和 sysctl

```bash
# 装载内核模块
sudo modprobe overlay
sudo modprobe br_netfilter
cat <<EOF | sudo tee /etc/modules-load.d/k8s.conf
overlay
br_netfilter
EOF

# iptables 能看到桥接流量
cat <<EOF | sudo tee /etc/sysctl.d/k8s.conf
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF
sudo sysctl --system
```

## 2. 安装 mydocker-cri

```bash
# 在宿主机上从项目构建（或 scp 已 build 的 bin/）
make build-linux
sudo install -m 0755 bin/mydocker     /usr/local/bin/mydocker
sudo install -m 0755 bin/mydocker-cri /usr/local/bin/mydocker-cri
```

## 3. 安装 CNI 插件

```bash
sudo ./scripts/install-cni.sh
ls /opt/cni/bin/        # 应该看到 bridge / host-local / portmap / firewall 等
cat /etc/cni/net.d/10-mydocker.conflist
```

## 4. 准备 systemd unit（让 mydocker-cri 作为 service 运行）

```bash
sudo tee /etc/systemd/system/mydocker-cri.service >/dev/null <<'EOF'
[Unit]
Description=mydocker CRI server
Documentation=https://github.com/mini-docker/mini-docker
After=network.target

[Service]
Type=exec
ExecStart=/usr/local/bin/mydocker-cri serve \
    --socket=/var/run/my-cri.sock \
    --streaming-addr=127.0.0.1:10350 \
    --cni-conf-dir=/etc/cni/net.d \
    --cni-bin-dir=/opt/cni/bin
Restart=always
RestartSec=2
LimitNOFILE=1048576
# cgroup driver 必须与 Kubelet 配置一致。我们实现的是 cgroupfs 语义，
# 所以此处 Delegate=yes 让 systemd 不干涉我们自己的 cgroup 层级。
Delegate=yes
KillMode=process

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now mydocker-cri
sudo systemctl status mydocker-cri   # 确认 active (running)
```

## 5. 验证 CRI 自己能用（接 Kubelet 之前必做）

```bash
# 1. 装 crictl
VERSION=v1.30.0
curl -fsSL "https://github.com/kubernetes-sigs/cri-tools/releases/download/$VERSION/crictl-$VERSION-linux-amd64.tar.gz" \
  | sudo tar -C /usr/local/bin -xz

# 2. 配置 crictl 默认 endpoint
sudo tee /etc/crictl.yaml >/dev/null <<'EOF'
runtime-endpoint: unix:///var/run/my-cri.sock
image-endpoint: unix:///var/run/my-cri.sock
timeout: 10
EOF

# 3. 检查
sudo crictl info
sudo crictl info | grep -E "Ready|network"
# 应该看到 RuntimeReady=true，NetworkReady=true

# 4. 拉 pause 镜像并验证 Pod 能跑
sudo crictl pull registry.k8s.io/pause:3.9
sudo crictl images
```

如果这一步有问题，先别往下走——Kubelet 出错后日志极难读。

## 6. 安装 kubeadm / kubelet / kubectl

```bash
# Ubuntu / Debian
sudo apt-get update
sudo apt-get install -y apt-transport-https ca-certificates curl gpg
sudo mkdir -p -m 755 /etc/apt/keyrings
curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.30/deb/Release.key \
  | sudo gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.30/deb/ /' \
  | sudo tee /etc/apt/sources.list.d/kubernetes.list
sudo apt-get update
sudo apt-get install -y kubelet=1.30.* kubeadm=1.30.* kubectl=1.30.*
sudo apt-mark hold kubelet kubeadm kubectl
```

> 我们的 `cri-api` 版本是 `v0.30.0`，对应 Kubernetes 1.30。跨版本不保证兼容。

## 7. 写 Kubelet 配置（指向 mydocker-cri）

```bash
# kubelet 需要知道 socket 路径
sudo mkdir -p /etc/systemd/system/kubelet.service.d
sudo tee /etc/systemd/system/kubelet.service.d/20-mydocker.conf >/dev/null <<'EOF'
[Service]
Environment="KUBELET_EXTRA_ARGS=--container-runtime-endpoint=unix:///var/run/my-cri.sock --image-service-endpoint=unix:///var/run/my-cri.sock --pod-infra-container-image=registry.k8s.io/pause:3.9"
EOF
sudo systemctl daemon-reload
```

## 8. 初始化集群

```bash
sudo kubeadm init \
  --cri-socket=unix:///var/run/my-cri.sock \
  --pod-network-cidr=10.22.0.0/16 \
  --kubernetes-version=v1.30.0

# kubectl 配置
mkdir -p ~/.kube
sudo cp -f /etc/kubernetes/admin.conf ~/.kube/config
sudo chown $(id -u):$(id -g) ~/.kube/config

# 单节点：去掉 control-plane 的 taint 让 Pod 能调度上来
kubectl taint nodes --all node-role.kubernetes.io/control-plane-
```

## 9. 跑一个 Pod

```bash
kubectl get nodes           # 应该是 Ready
kubectl get pods -A         # coredns 会起不来，因为我们的 CNI 是最简配置
                             # 不影响 nginx 测试

kubectl run nginx --image=nginx:alpine
kubectl get pods -w         # 等状态变 Running

# 验证网络
POD_IP=$(kubectl get pod nginx -o jsonpath='{.status.podIP}')
kubectl run curl --rm -it --restart=Never --image=busybox -- wget -qO- "http://$POD_IP"

# 验证 exec（需要 --streaming-addr 正确配置）
kubectl exec -it nginx -- sh

# 验证 logs
kubectl logs nginx
```

## 10. 常见坑

### Kubelet 日志刷 "NetworkReady=false"
- 检查 `/etc/cni/net.d/` 有无 `.conflist`
- `sudo crictl info | grep NetworkReady` 确认 CRI 侧状态
- 检查 `/opt/cni/bin/` 下插件完整

### Kubelet 启动卡在 "waiting for api-server"
- 排查 mydocker-cri 日志：`journalctl -u mydocker-cri -f`
- 检查 pause 镜像是否能拉：`sudo crictl pull registry.k8s.io/pause:3.9`
- 如果 registry 不可达，换成 `--pod-infra-container-image=` 指向本地已导入的镜像

### "cgroup driver mismatch"
- Kubelet 1.30 默认 `cgroupDriver: systemd`；我们实现的是 cgroupfs 等价
- 改 `/var/lib/kubelet/config.yaml` 加 `cgroupDriver: cgroupfs` 或调整我们的 cgroup 代码对接 systemd D-Bus
- 这是单节点 POC 最常见的阻塞点

### Pod 拿不到 IP
- `sudo crictl inspectp <POD_ID>` 看 `network.ip`
- 手动调一次 CNI：`sudo /opt/cni/bin/bridge < /etc/cni/net.d/10-mydocker.conflist`（stdin 传参）
- 检查 netns：`sudo ip netns list`（应能看到 mydocker 创建的 ns）

### kubectl exec 报 "upgrade request required"
- streaming server 没启或地址不对
- `curl http://127.0.0.1:10350/` 应返回 404 而不是拒绝连接
- 检查 `--streaming-addr` 和 Kubelet 能否访问这个地址

### Exec / logs 后 journalctl 报 panic
- 说明某些接口我们还没实现（如 `GetContainerEvents`，1.29+ Kubelet 会重试）
- 短期方案：回退到 v1.28 Kubelet；长期方案：实现对应 RPC

## 11. 节点级排查命令速查

```bash
# CRI daemon 日志
journalctl -u mydocker-cri -f

# Kubelet 日志
journalctl -u kubelet -f

# CRI 直接查
sudo crictl info
sudo crictl pods
sudo crictl ps -a
sudo crictl logs <container_id>

# 容器进程树
sudo ps auxf | grep -B1 mydocker

# cgroup v2 层级
sudo systemd-cgls

# Pod 的 netns
sudo ls /var/run/netns
sudo ip netns exec <netns> ip -br addr

# CNI 缓存（排查 ADD/DEL 序列问题）
sudo ls /var/lib/mydocker/cni-cache/
```

## 12. 卸载 / 重置

```bash
sudo kubeadm reset -f
sudo systemctl stop kubelet mydocker-cri
sudo rm -rf /var/lib/kubelet /etc/kubernetes /var/lib/mydocker /var/lib/cni
sudo rm -f /etc/cni/net.d/10-mydocker.conflist /var/run/my-cri.sock
sudo ip link del mydocker0 2>/dev/null || true
sudo iptables -t nat -F
sudo iptables -F
```

---

## 验收标准（L4 MVP）

- [x] `kubectl get nodes` 显示 Ready
- [x] `kubectl run nginx --image=nginx:alpine` → Pod 进入 Running
- [x] `kubectl exec nginx -- wget -qO- http://localhost` 能返回 nginx 默认页
- [x] `kubectl logs nginx` 能看到 nginx 启动日志
- [x] 节点重启 systemd 能拉回 mydocker-cri 和 kubelet，集群恢复

如果这些都过了，你实现了一个能跑 K8s 工作负载的容器运行时。🎉
