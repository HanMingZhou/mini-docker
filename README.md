# mini-docker

A from-scratch container engine **and** Kubernetes CRI runtime, built for learning.

这是一个 4 级递进的容器化实现：

| Level | 目标 | 状态 |
|-------|------|------|
| L1 | Namespace + cgroup + rootfs 隔离（CLI） | ✅ |
| L2 | 完整 Docker 功能（image/network/volume/build/push 等） | ✅ 22/22 e2e |
| L3 | CRI gRPC Server（crictl 可用） | ✅ smoke 通过 |
| L4 | Kubelet + kubeadm 集成（标准 K8s 控制面） | ✅ kubeadm init 成功 |

Kubelet 接入指南见 [`scripts/kubelet-integration.md`](./scripts/kubelet-integration.md)。

---

## 目录结构

```
mini-docker/
├── cmd/
│   ├── mydocker/          # L1-L2：Docker 风格的 CLI
│   └── mydocker-cri/      # L3-L4：CRI gRPC 守护进程
├── pkg/
│   ├── namespace/         # clone flag、setns 辅助
│   ├── cgroup/            # cgroups v2 + systemd driver
│   ├── rootfs/            # pivot_root + overlayfs
│   ├── container/         # 容器生命周期（启动、init、wait）
│   ├── image/             # OCI 镜像 pull / push / overlay 组装
│   ├── network/           # libcni 封装 + 端口映射
│   ├── sandbox/           # Pod 沙箱（pause 进程 / hostNetwork）
│   ├── cri/               # CRI RuntimeService + ImageService
│   ├── store/             # 容器元数据持久化
│   ├── logger/            # CRI 规定的日志格式
│   └── ...
├── scripts/
│   ├── prepare-rootfs.sh      # 从 docker 镜像导出 rootfs
│   ├── install-cni.sh         # 安装标准 CNI 插件
│   ├── mydocker-cri.service   # systemd unit
│   ├── smoke-l1.sh            # L1 最小冒烟
│   ├── e2e-docker.sh          # L2 完整 Docker 功能测试（22 项）
│   ├── smoke-l3.sh            # L3 CRI 冒烟（sandbox/image/container/exec）
│   ├── standalone-kubelet.sh  # L4A 独立 kubelet + static pod
│   ├── kubeadm-e2e.sh         # L4B kubeadm init 完整集群
│   └── kubelet-integration.md # L4 部署与排查手册
└── ...
```

---

## 开发环境

- 代码可在 Windows / macOS / Linux 编辑
- **运行必须在 Linux**（推荐 Ubuntu 22.04+，cgroups v2 + systemd）
- Go 1.21+（当前项目使用 1.24）
- 推荐 macOS 用户用 multipass 起 VM：
  ```bash
  multipass launch --cpus 4 --memory 4G --disk 20G --name mydocker-dev 22.04
  multipass shell mydocker-dev
  ```

## 构建

```bash
make build-linux       # 交叉编译到 linux/amd64，产物 bin/mydocker{,-cri}
make vet               # 静态检查
make test              # 单元测试（所有 _test.go）
```

---

## Level 1：最小隔离容器

```bash
# 准备一个 rootfs（从 busybox 镜像导出）
sudo ./scripts/prepare-rootfs.sh ./rootfs-busybox busybox:latest

# 跑一个带 PID/UTS/Mount namespace + cgroup 限制的 shell
sudo ./mydocker run -it \
    --rootfs ./rootfs-busybox \
    --name demo \
    --hostname demo \
    --memory 100m \
    --cpus 0.5 \
    /bin/sh
```

容器内部：
- `ps` 只看到自己的进程（PID ns）
- `hostname` 显示 `demo`（UTS ns）
- `ls /` 是 rootfs 的内容（mount ns + pivot_root）
- 在宿主机看 `/sys/fs/cgroup/demo/memory.max` 能看到 100MiB 限制

快速验证：

```bash
sudo ROOTFS=./rootfs-busybox ./scripts/smoke-l1.sh
```

---

## Level 2：完整 Docker 功能

Level 2 在 L1 基础上补齐 image pull / push、桥接网络、端口映射、volume、commit、build、inspect、exec、stats、kill、pause 等用户日常会用到的 Docker 子命令。

### 快速验证（22 项端到端）

```bash
# 装二进制 + CNI 插件
sudo install -m 0755 bin/mydocker /usr/local/bin/mydocker
sudo ./scripts/install-cni.sh

# 跑完整 e2e
sudo bash scripts/e2e-docker.sh
```

e2e 覆盖：
- image pull / ls / tag
- run -it 前台、run -d 后台
- 桥接网络 + 端口映射（httpd on :80 → host :7777，curl 通）
- volume 挂载、环境变量、-c 命令
- exec / logs / inspect / ps / stats
- pause / unpause / kill / restart
- cp 拷贝文件进出容器
- commit 提交成新镜像，镜像落地可重新 run
- build（Dockerfile）
- save / load
- push / pull（需要配置 registry，跳过时打 skip）

---

## Level 3：CRI gRPC Server

`mydocker-cri` 实现 Kubernetes CRI v1，可以被 `crictl` 直接驱动。

### 部署

```bash
# 二进制到位
sudo install -m 0755 bin/mydocker-cri /usr/local/bin/mydocker-cri
sudo install -m 0755 bin/mydocker      /usr/local/bin/mydocker

# systemd 托管
sudo install -m 0644 scripts/mydocker-cri.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now mydocker-cri
sudo systemctl is-active mydocker-cri   # active

# CNI 插件（让 Pod 能拿到 IP）
sudo ./scripts/install-cni.sh
```

### crictl 配置

```bash
sudo tee /etc/crictl.yaml >/dev/null <<'EOF'
runtime-endpoint: unix:///var/run/mydocker-cri.sock
image-endpoint: unix:///var/run/mydocker-cri.sock
timeout: 10
EOF

sudo crictl info | head -20
# RuntimeReady=true、NetworkReady=true
```

### 冒烟测试

```bash
sudo bash scripts/smoke-l3.sh
```

覆盖：
- Version / Status RPC
- RunPodSandbox + PodSandboxStatus + IP 分配
- PullImage + ImageStatus + ListImages
- CreateContainer + StartContainer + ContainerStatus
- ExecSync（同步命令）
- StopContainer + RemoveContainer
- StopPodSandbox + RemovePodSandbox

---

## Level 4：接入 Kubelet / kubeadm

两条路径，都已打通：

### 4A：独立 Kubelet + Static Pod（快速验证 CRI 协议）

不依赖 apiserver，kubelet 直接读 `/etc/kubernetes/manifests/*.yaml` 创建 Pod。

```bash
sudo bash scripts/standalone-kubelet.sh
```

13 项检查，覆盖：sandbox → container → 网络 → 镜像拉取 → Pod 自恢复。

### 4B：kubeadm init 完整 K8s 控制面

真正的 Kubernetes 集群，以 mydocker-cri 为底层 runtime。

```bash
# 一键端到端
sudo bash scripts/kubeadm-e2e.sh
```

验证：
- `kubeadm init` 8 秒内 API server healthy
- `kubectl get nodes` → Ready
- `kubectl get nodes -o wide` → CONTAINER-RUNTIME 显示 `mydocker://0.1.0`
- etcd / kube-apiserver / kube-controller-manager / kube-scheduler 全部 Running

详细部署流程、systemd unit、cgroup driver 配置、常见坑见 [`scripts/kubelet-integration.md`](./scripts/kubelet-integration.md)。

#### 典型输出

```
[api-check] The API server is healthy after 8.507231s
Your Kubernetes control-plane has initialized successfully!

NAME           STATUS   ROLES           AGE   VERSION    INTERNAL-IP     CONTAINER-RUNTIME
mydocker-dev   Ready    control-plane   98s   v1.30.14   192.168.252.2   mydocker://0.1.0
```

---

## 调试

### mydocker-cri 日志

```bash
# 文件日志（比 journald 全）
sudo tail -f /var/log/mydocker-cri.log

# RPC 拦截器输出每个调用的方法名、耗时、错误
# [CRI] /runtime.v1.RuntimeService/RunPodSandbox OK (1.944ms)
# [CRI] /runtime.v1.RuntimeService/ContainerStatus ERR no such container (...) (58µs)
```

默认过滤 List/Status 这种高频 RPC 只在出错时打印，避免噪音。

### Kubelet 日志

```bash
sudo journalctl -u kubelet -f
sudo journalctl -u kubelet --since '5 min ago' --no-pager
```

### 容器日志

```bash
# CRI 格式（kubectl logs / crictl logs 都走这个）
sudo cat /var/log/pods/<namespace>_<pod>_<uid>/<container>/<attempt>.log

# mydocker CLI 场景
sudo mydocker logs <container>
```

### 常见坑

- **cgroup driver 不一致**：Kubelet 默认 systemd，mydocker-cri 必须同步（`--cgroup-driver=systemd`）
- **CNI 不通**：`/etc/cni/net.d/` 要有 conflist，`/opt/cni/bin/` 要全
- **端口 10250 占用**：前一次 kubelet 没停，`systemctl stop kubelet` 再 `kubeadm reset`
- **hostNetwork Pod 状态**：sandbox 的 NetnsPath 必须是 `/proc/1/ns/net`，NamespaceOption.Network 必须是 `NODE`（非 `POD`）——否则 Kubelet 会无限重建 Pod

---

## 里程碑

- [x] L1：`mydocker run` 能起一个带 PID/UTS/Mount ns 和 cgroup v2 限制的 shell
- [x] L2：端到端 22/22 跑通（image/network/volume/build/commit/push/save 全通）
- [x] L3：`crictl info`、`crictl pull`、`crictl runp/create/start/exec` 全通
- [x] L4A：standalone kubelet 从 manifest 把 Pod 拉起来、自恢复、外部能 curl
- [x] L4B：**`kubeadm init` 在 mydocker-cri 上拉起完整控制面**（etcd + apiserver + controller-manager + scheduler）
- [ ] CoreDNS / 自定义 Pod（非 hostNetwork）调度 — 需要 CNI 对 Pod 网络做更完整的支持
- [ ] 多节点 `kubeadm join`

---

## License

MIT
