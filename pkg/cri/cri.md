# RuntimeService 与 ImageService
它们是 Kubernetes CRI (Container Runtime Interface) 协议定义的两个 gRPC 服务，是 Kubelet 与底层容器运行时（containerd / cri-o / ）之间的唯一标准接口。
定义文件在 Kubernetes 源码 k8s.io/cri-api/pkg/apis/runtime/v1/api.proto

## 一、为什么要有这两个服务？
历史背景：Kubernetes 早期硬编码 Docker 作为运行时（叫 dockershim），代码耦合非常严重。2016 年 K8s 抽象出 CRI，让 Kubelet 只面向一个稳定的 gRPC 接口编程，谁实现这个接口谁就能当 K8s 的运行时。

职责切分逻辑：
维度	        RuntimeService	            ImageService
关心的对象	     进程 / namespace / cgroup	      镜像文件 / layer / registry
状态	        有状态（运行中的 Pod、容器）	    主要操作本地存储
生命周期	     频繁变化（启停容器、Exec）	        慢变化（pull、删除）
谁调用	        Kubelet 主循环	                 Kubelet 的 image manager（独立 goroutine）

把镜像 RPC 单独切出来，可以让 Kubelet 在 pull 大镜像时不阻塞容器生命周期管理，也方便实现侧把镜像存储完全独立出来（理论上甚至可以是另一个 daemon）。

## 二、RuntimeService 是什么
一句话：管理"正在跑的东西"——Pod 沙箱、容器、Exec 通道、运行时状态。

它的 server 端骨架：
```go
// RuntimeService 是 CRI RuntimeService 的实现。
type RuntimeService struct {
    runtime.UnimplementedRuntimeServiceServer
 
    sandboxMgr     *sandbox.Manager
    streaming      StreamingServer
    cni            *network.Manager
    cgroupDriver   cgroup.Driver
    mydockerBinary string
}
```
runtime.UnimplementedRuntimeServiceServer 是 gRPC protoc 生成的"默认空实现"，把所有 RPC 默认返回 Unimplemented 错误。你只需要"重写"自己想支持的方法，没重写的自动 fallback。本项目几乎重写了所有方法。

UnimplementedRuntimeServiceServer 是 gRPC 在 Go 里的一个前向兼容机制。理解它需要分三层：proto 定义 → protoc 生成代码 → 你的实现
### 一、背景：gRPC 的 service 在 Go 里是 interface
CRI 在 proto 里定义了一个 service：
```go
service RuntimeService {
    rpc Version(VersionRequest) returns (VersionResponse) {}
    rpc RunPodSandbox(RunPodSandboxRequest) returns (RunPodSandboxResponse) {}
    rpc CreateContainer(CreateContainerRequest) returns (CreateContainerResponse) {}
    // ... 还有 20 多个 RPC
}
```
protoc-gen-go-grpc 看到 service 关键字，会自动生成一个 Go interface（在 k8s.io/cri-api 包里）：
```go
type RuntimeServiceServer interface {
    Version(context.Context, *VersionRequest) (*VersionResponse, error)
    RunPodSandbox(context.Context, *RunPodSandboxRequest) (*RunPodSandboxResponse, error)
    CreateContainer(context.Context, *CreateContainerRequest) (*CreateContainerResponse, error)
    // ... 25+ 个方法
}
```
而 runtime.RegisterRuntimeServiceServer(grpcServer, impl) 要求传进来的 impl 必须满足这整个 interface——一个方法都不能少，否则编译失败。

### 二、问题：interface 演进 vs 你的代码
CRI 是 Kubernetes 项目维护的，每个 K8s 版本可能给 RuntimeServiceServer 加新方法。比如 v1.27 加了 CheckpointContainer，v1.28 加了 ListMetricDescriptors。

如果 gRPC 只生成 interface，那么：
你今天实现了 25 个方法，编译通过
升级 cri-api 到下一版，interface 多了 1 个方法
你的代码立刻编译失败，哪怕你完全用不到那个新方法
这对生态来说是灾难——所有运行时实现者都被强制跟版本。

解法：生成一个"全是空方法"的结构体
protoc-gen-go-grpc 在生成 interface 的同时，还生成一个配套的 struct：

```go
// 由 protoc 自动生成（不是手写的）
type UnimplementedRuntimeServiceServer struct{}
 
func (UnimplementedRuntimeServiceServer) Version(context.Context, *VersionRequest) (*VersionResponse, error) {
    return nil, status.Errorf(codes.Unimplemented, "method Version not implemented")
}
func (UnimplementedRuntimeServiceServer) RunPodSandbox(context.Context, *RunPodSandboxRequest) (*RunPodSandboxResponse, error) {
    return nil, status.Errorf(codes.Unimplemented, "method RunPodSandbox not implemented")
}
// ... 每个 RPC 都有一个"返回 Unimplemented 错误"的默认实现
```

这个 struct 自动满足 RuntimeServiceServer interface 的所有方法。

关键技巧：Go 的"嵌入"（embedding）
现在看本项目的代码：
```go
type RuntimeService struct {
    runtime.UnimplementedRuntimeServiceServer
 
    sandboxMgr     *sandbox.Manager
    streaming      StreamingServer
    cni            *network.Manager
    cgroupDriver   cgroup.Driver
    mydockerBinary string
}
```
第一行 runtime.UnimplementedRuntimeServiceServer（没有字段名）是 Go 的结构体嵌入语法。它的效果是：
RuntimeService 自动"继承"了 UnimplementedRuntimeServiceServer 的所有方法。
所以一开始，RuntimeService 就已经满足了整个 RuntimeServiceServer interface——所有 RPC 都返回 Unimplemented 错误。

然后你只需要写自己想支持的方法：
```go
func (s *RuntimeService) Version(...) (...) { ... }  // 重写
func (s *RuntimeService) RunPodSandbox(...) (...) { ... }  // 重写
```
Go 的方法解析规则是"外层方法优先于嵌入字段的方法"，所以你写的 Version 会覆盖嵌入的那个返回 Unimplemented 的版本。没写的方法继续走嵌入的默认实现。

举个具体例子
假设你只实现了 Version 和 Status：
```go
type RuntimeService struct {
    runtime.UnimplementedRuntimeServiceServer
}
 
func (s *RuntimeService) Version(...) (*VersionResponse, error) {
    return &VersionResponse{...}, nil
}
func (s *RuntimeService) Status(...) (*StatusResponse, error) {
    return &StatusResponse{...}, nil
}
```
效果：
Kubelet 调用	实际触发	返回
Version	你写的方法	正常响应
Status	你写的方法	正常响应
RunPodSandbox	嵌入的默认方法	gRPC error: Unimplemented
CreateContainer	嵌入的默认方法	gRPC error: Unimplemented
代码编译通过、可以注册到 gRPC server，只是大部分 RPC 客户端调用时会拿到 Unimplemented 错误。

本项目的实际情况
"本项目几乎重写了所有方法"
意思是：在 pkg/cri/ 各个文件里，*RuntimeService 已经显式实现了：
server.go: Version, Status
sandbox.go: RunPodSandbox, StopPodSandbox, RemovePodSandbox, PodSandboxStatus, ListPodSandbox
container.go: CreateContainer, StartContainer, StopContainer, RemoveContainer, ContainerStatus, ListContainers
exec.go: ExecSync, Exec, Attach, PortForward
stats.go: ContainerStats, ListContainerStats, PodSandboxStats, ListPodSandboxStats, UpdateRuntimeConfig, UpdateContainerResources, ReopenContainerLog
这覆盖了 Kubelet 实际会调用的所有 RPC。没覆盖的（比如 CheckpointContainer、GetContainerEvents、ListMetricDescriptors、ListPodSandboxMetrics 这些较新或可选的 RPC）就自动走嵌入的默认实现，返回 Unimplemented，Kubelet 收到这个错误码会优雅降级（不当成致命错误）。

ImageService 也是同样的模式：
```go
type ImageService struct {
    runtime.UnimplementedImageServiceServer
 
    root string // 数据根目录
}
```
七、一句话总结
UnimplementedXxxServer 是 protoc 自动生成的"打底空壳"，通过 Go 的结构体嵌入让你的实现默认满足整个 interface；你只需要"挑你关心的 RPC 重写一下"，未来 cri-api 加新方法也不会破坏你的代码——这就是 gRPC Go 的前向兼容约定。

题外话：这个模式在 gRPC Go 里是强烈推荐做法，新版本的 protoc-gen-go-grpc 甚至默认要求你必须嵌入它，否则编译会拒绝（require_unimplemented_servers=true）。


RuntimeService 的 RPC 大类
类别	    典型 RPC	        本项目位置
握手/心跳	Version、Status	        server.go:261-286
Pod 沙箱	RunPodSandbox / StopPodSandbox / RemovePodSandbox / PodSandboxStatus / ListPodSandbox   sandbox.go
容器	CreateContainer / StartContainer / StopContainer / RemoveContainer / ContainerStatus / ListContainers	container.go
执行	ExecSync / Exec / Attach / PortForward	exec.go
统计与配置	ContainerStats / ListContainerStats / PodSandboxStats / UpdateRuntimeConfig / UpdateContainerResources / ReopenContainerLog     stats.go

### 关键概念：什么是 Pod 沙箱？
CRI 把"Pod"分成两层：
Sandbox（沙箱）：先创建的"壳"，持有 Pod 级的 网络命名空间、IPC 命名空间、cgroup 父目录、Pod IP，本身通常只跑一个轻量 pause 进程，永不退出。
Container（容器）：在沙箱之上，加入沙箱的 network/IPC/UTS 命名空间，但有自己的 PID/Mount 命名空间和镜像 rootfs。

本项目的体现：
```go
// Build JoinNS map: container joins sandbox's net/ipc/uts
joinNS := map[string]string{
    "net": fmt.Sprintf("/proc/%d/ns/net", sb.PID),
    "ipc": fmt.Sprintf("/proc/%d/ns/ipc", sb.PID),
    "uts": fmt.Sprintf("/proc/%d/ns/uts", sb.PID),
}
```
这样同一个 Pod 里的多个容器（业务 + sidecar）就能 localhost 互通、看到同一组 SysV IPC，但各自有独立的进程视图和文件系统视图。

心跳：Status RPC
Kubelet 每隔几秒调用一次 Status，问两个布尔：
```go
func (s *RuntimeService) Status(_ context.Context, _ *runtime.StatusRequest) (*runtime.StatusResponse, error) {
    networkReady := false
    if s.cni != nil {
        networkReady = s.cni.Ready()
    }
    return &runtime.StatusResponse{
        Status: &runtime.RuntimeStatus{
            Conditions: []*runtime.RuntimeCondition{
                {Type: runtime.RuntimeReady, Status: true},
                {Type: runtime.NetworkReady, Status: networkReady},
            },
        },
    }, nil
}
```
RuntimeReady=false → Kubelet 把 Node 标记为 NotReady，不再调度新 Pod
NetworkReady=false → Kubelet 只允许 hostNetwork: true 的 Pod 启动（这就是为什么 kubeadm init 时 CoreDNS 一直 Pending —— 它不是 hostNetwork）

Kubelet 的典型调用顺序（创建一个 Pod）
1. PullImage(image)              ── ImageService
2. RunPodSandbox(podConfig)      ── RuntimeService → 拿到 sandboxId, podIP
3. CreateContainer(sandboxId, containerConfig)  ── RuntimeService → 拿到 containerId
4. StartContainer(containerId)   ── RuntimeService
5. (循环) ContainerStatus / Status  ── RuntimeService 心跳

## 三、ImageService 是什么
一句话：管理"镜像仓库"——pull、list、删除、查盘占用。
```go
type ImageService struct {
    runtime.UnimplementedImageServiceServer
 
    root string // 数据根目录
}
func newImageService(root string) *ImageService {
    return &ImageService{root: root}
}
 ```
注意它只有一个字段 root——这就是 ImageService 的本质：它只是一个建立在数据目录上的镜像 CRUD 服务，与正在跑的容器完全无关。

ImageService 的 RPC 全集（只有 5 个）
RPC	作用	本项目实现
PullImage	从 registry 拉镜像	image.go:27-53
ListImages	列出本地所有镜像	image.go:56-74
ImageStatus	查询单个镜像（含 size、digest）	image.go:77-94
RemoveImage	删除本地镜像	image.go:97-117
ImageFsInfo	镜像存储盘的使用量（给 Kubelet 做 disk pressure 判断）	image.go:120-147

对比一下：RuntimeService 大概 25+ 个 RPC，ImageService 只有 5 个，因为镜像操作本质简单。
CRI 镜像约定中的两个细节
### 1. ImageStatus 找不到镜像不是错误：
```go
m, err := is.Resolve(req.Image.Image)
if err != nil {
    return nil, err
}
if m == nil {
    // CRI convention: not found -> nil status, no error
    return &runtime.ImageStatusResponse{}, nil
}
return &runtime.ImageStatusResponse{Image: manifestToProto(m)}, nil
```
Kubelet 用 ImageStatus 检查"镜像在不在本地"，靠 response.Image == nil 判断。如果返回错误，Kubelet 会以为运行时挂了。

### 2. ImageFsInfo 决定 Node 的 DiskPressure 状态：Kubelet 用这个值与 --eviction-hard imagefs.available<10% 比较，超过阈值就开始驱逐 Pod。

## 四、两者怎么协作？以 mini-docker 为例
                ┌────────────────────────────────────┐
                │       grpc.Server (单个进程)        │
                │                                    │
Kubelet ───────►│  RuntimeService ◄── sandboxMgr, cni│
crictl  ───────►│       │             container, store
                │       │ root                       │
                │       ▼                            │
                │  ImageService ◄── pkg/image        │
                │       │                            │
                └───────┼────────────────────────────┘
                        ▼
                /var/lib/mydocker/
                  ├── images/      ← ImageService 写
                  ├── layers/      ← ImageService 写
                  ├── containers/  ← RuntimeService 写
                  └── sandboxes/   ← RuntimeService 写
两个 service 在同一个 gRPC server 上注册，共用 socket：
```go
runtime.RegisterRuntimeServiceServer(s.grpc, s.runtime)
runtime.RegisterImageServiceServer(s.grpc, s.image)
```
它们之间不直接调用对方，而是通过共享的 root 目录（/var/lib/mydocker/）解耦：

ImageService.PullImage 把 layer 写到 root/layers/、manifest 写到 root/images/
RuntimeService.CreateContainer 读 root/images/<name>/manifest.json，用 overlayfs 在 root/containers/<id>/ 把 layer 组装成 rootfs
这种"通过文件系统解耦"的设计就是 CRI 拆分两个 service 的目的——理论上 ImageService 可以是完全独立的 daemon 写到共享卷，RuntimeService 只读。

## 五、Kubelet 怎么连这两个 service
/etc/crictl.yaml 和 Kubelet 启动参数都有两个 endpoint：
```yaml
runtime-endpoint: unix:///var/run/mydocker-cri.sock
image-endpoint:   unix:///var/run/mydocker-cri.sock
```
99% 情况两个值相同（同一个 daemon），但协议允许不同，例如：
用 containerd 跑容器
用 stargz-snapshotter 作为独立 image 服务做懒加载
这就是上一轮回答里"协议允许但通常不这么用"的具体含义。

## 六、记忆口诀
RuntimeService = "动的东西"：Pod、容器、进程、Exec、Stats
ImageService = "静的东西"：镜像文件、磁盘占用
两者通过 CRI 协议统一接口，通过 共享数据目录解耦实现







共享 namespace 的实现机制
要回答这个问题，需要理解三个 Linux 内核概念，然后看 mini-docker 在哪一行调用了对应的系统调用。

一、Linux 内核里"namespace"是什么
每个进程在内核里都有一组指针，指向 7 类 namespace 对象：

进程 PCB (task_struct)
  └── nsproxy
       ├── mnt_ns   (文件系统挂载视图)
       ├── pid_ns   (进程号视图)
       ├── net_ns   (网卡/路由/iptables/端口)
       ├── uts_ns   (hostname/domainname)
       ├── ipc_ns   (SysV IPC、POSIX 消息队列)
       ├── user_ns  (uid/gid 映射)
       └── cgroup_ns
两个进程指针指向同一个 namespace 对象 ⇒ 它们看到同一份资源，反之看到不同的副本。

每个 namespace 对象在 /proc 里被表示为一个文件：

/proc/<PID>/ns/net  → 一个 inode，代表"net namespace 实例 X"
/proc/<PID>/ns/ipc  → ...
/proc/<PID>/ns/uts  → ...
/proc/<PID>/ns/pid  → ...
/proc/<PID>/ns/mnt  → ...
如果两个进程的 /proc/<pid>/ns/net 指向同一个 inode，它们就在同一个 net namespace 里。

bash
# 验证：同一个 Pod 里两个容器
$ ls -L /proc/12345/ns/net   # 容器 A
net:[4026532001]
$ ls -L /proc/12346/ns/net   # 容器 B
net:[4026532001]   # ← 数字相同！
二、Pod 沙箱的角色
回想 sandbox 是怎么创建的（@c:/project/mini-docker/pkg/sandbox/sandbox.go:1-7）：

沙箱 = 一个常驻的 pause 进程 + 独立的 network/UTS/IPC namespace。

启动 sandbox 时用 clone(2) 系统调用，传入 CLONE_NEWNET | CLONE_NEWIPC | CLONE_NEWUTS，内核会新建三个 namespace 对象，把 pause 进程指过去。pause 进程一直 sleep 不退出，namespace 对象的引用计数就永远 ≥ 1，不会被销毁。

之后 sandbox 把 pause 的 PID 记下来：

sandbox.go:46-50
Metadata     Metadata          `json:"metadata"`
State        State             `json:"state"`
PID          int               `json:"pid"`          // pause 进程的宿主机 PID
NetnsPath    string            `json:"netns_path"`   // /proc/<pid>/ns/net，便于业务容器 setns
IP           string            `json:"ip,omitempty"` // CNI 分配的 IPv4，无则空
/proc/<sb.PID>/ns/net 这个文件路径就成了"指向沙箱 net namespace 的句柄"。

三、StartContainer 怎么让容器进去
回到你看的代码（@c:/project/mini-docker/pkg/cri/container.go:233-264），有两步关键动作：

第 1 步：告诉内核 clone 时不要新建这三个 ns
在父进程（mydocker-cri）里：

go
joinNS := map[string]string{
    "net": fmt.Sprintf("/proc/%d/ns/net", sb.PID),
    "ipc": fmt.Sprintf("/proc/%d/ns/ipc", sb.PID),
    "uts": fmt.Sprintf("/proc/%d/ns/uts", sb.PID),
}
...
Namespaces: namespace.Flags{
    PID:     true,
    Mount:   true,
    Network: true, // will be cleared by JoinNS
    IPC:     true, // will be cleared by JoinNS
    UTS:     true, // will be cleared by JoinNS
},
注释 "will be cleared by JoinNS" 是核心机关。来看实现：

container_linux.go:60-71
// 计算 clone flags：如果某个 ns 要 join 已有的，就不创建新的
cloneFlags := cfg.Namespaces.CloneFlags()
if cfg.JoinNS != nil {
    for nsType := range cfg.JoinNS {
        cloneFlags = clearCloneFlag(cloneFlags, nsType)
    }
}
 
cmd := exec.Command(initBinary(cfg), "init")
cmd.SysProcAttr = &syscall.SysProcAttr{
    Cloneflags: cloneFlags,
}
Cloneflags 是 Go 标准库对 Linux clone(2) 的封装。clearCloneFlag 用 Go 的位运算 &^（AND-NOT）把对应 bit 清零：

container_linux.go:333-348
func clearCloneFlag(flags uintptr, ns string) uintptr {
    switch ns {
    case "net":
        return flags &^ uintptr(syscall.CLONE_NEWNET)
    case "ipc":
        return flags &^ uintptr(syscall.CLONE_NEWIPC)
    case "uts":
        return flags &^ uintptr(syscall.CLONE_NEWUTS)
结果：exec.Command(...).Start() 实际触发的 clone 调用，flags 只包含 CLONE_NEWPID | CLONE_NEWNS，没有 CLONE_NEWNET / IPC / UTS。

内核看到这组 flag 后：

给新进程新建 PID 和 mount namespace（独立进程视图、独立文件系统视图）
复制父进程指针给 net/ipc/uts namespace（也就是 mydocker-cri 自己的，还不是 sandbox 的）
所以这一步只是"先不要新建"，还没真的进 sandbox 的 namespace。

第 2 步：子进程启动后用 setns(2) 主动加入
子进程（init 进程）启动后立刻调用 initProcess：

container_linux.go:238-243
// 加入已有 namespace（CRI 场景：加入沙箱的 netns/ipc/uts）
if len(payload.JoinNS) > 0 {
    if err := joinNamespaces(payload.JoinNS); err != nil {
        return fmt.Errorf("join namespaces: %w", err)
    }
}
joinNamespaces 是真正干活的地方：

container_linux.go:295-315
// joinNamespaces 使用 setns 加入指定的 namespace。
func joinNamespaces(nsMap map[string]string) error {
    // 顺序：net, ipc, uts（pid 不在这里处理，mount 也不——我们自己做 pivot_root）
    order := []string{"net", "ipc", "uts"}
    for _, ns := range order {
        path, ok := nsMap[ns]
        if !ok {
            continue
        }
        fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
        if err != nil {
            return fmt.Errorf("open %s (%s): %w", ns, path, err)
        }
        if err := unix.Setns(fd, nsCloneFlag(ns)); err != nil {
            _ = unix.Close(fd)
            return fmt.Errorf("setns %s: %w", ns, err)
        }
        _ = unix.Close(fd)
    }
    return nil
}
逐行翻译：

unix.Open("/proc/<sb.PID>/ns/net", O_RDONLY) —— 打开 sandbox 那个 net namespace 文件，拿到一个 fd。
unix.Setns(fd, CLONE_NEWNET) —— 核心系统调用。告诉内核："把当前进程的 net namespace 指针，改为这个 fd 所代表的那一个"。
ipc、uts 同理。
setns(2) 之后，子进程的内核 task_struct 里：

nsproxy.net_ns 指向 sandbox 的 net_ns ✅
nsproxy.ipc_ns 指向 sandbox 的 ipc_ns ✅
nsproxy.uts_ns 指向 sandbox 的 uts_ns ✅
nsproxy.pid_ns 是 clone 时新建的（独立进程视图） ✅
nsproxy.mnt_ns 是 clone 时新建的（独立文件系统视图） ✅
正好对应你的描述：共享 net/ipc/uts，独立 pid/mount。

第 3 步：pivot_root + execve
container_linux.go:251-289
// pivot_root + /proc /sys /dev（extraMounts 已在父进程预挂到 merged）
if err := rootfs.Setup(payload.Rootfs, nil); err != nil {
    return fmt.Errorf("rootfs setup: %w", err)
}
...
if err := syscall.Exec(bin, payload.Cmd, envSlice); err != nil {
在自己独立的 mount namespace 里 pivot_root，把根目录换成镜像 rootfs；最后 execve 替换成业务进程（nginx / sh / 你的应用）。

四、串起来：一次 StartContainer 的完整时序
mydocker-cri (父)                 init 子进程                     业务进程
─────────────────────────────────────────────────────────────────────────
1. exec.Command(init, "init")
   Cloneflags = CLONE_NEWPID|NS
                 (NET/IPC/UTS 已被 clear)
2. cmd.Start()
   ───── clone(2) ─────────────►   诞生
                                   nsproxy 状态：
                                   - pid_ns: 新建 ✅
                                   - mnt_ns: 新建 ✅
                                   - net_ns: 继承父（cri）❌
                                   - ipc_ns: 继承父（cri）❌
                                   - uts_ns: 继承父（cri）❌
3. 父写 JSON payload(JoinNS)
   到 pipe ─────────────────────► 读到 payload
 
                                4. for ns in [net, ipc, uts]:
                                     fd = open(/proc/<sb.PID>/ns/<ns>)
                                     setns(fd, CLONE_NEW<NS>)
                                   nsproxy 状态：
                                   - net_ns: → sandbox ✅
                                   - ipc_ns: → sandbox ✅
                                   - uts_ns: → sandbox ✅
 
                                5. pivot_root(rootfs)
                                   挂 /proc /sys /dev（独立 mnt_ns 里）
 
                                6. execve(/usr/bin/nginx, ...) ──► nginx
                                   ↑ 内存映像被替换，但 nsproxy 不变
最后业务进程（nginx）继承了 init 设置好的所有 namespace 指针。

五、可观测的效果
假设同一个 Pod 里跑两个容器 A（nginx）和 B（curl-sidecar），它们的 pause 进程 PID 是 1000，nginx 是 1001，curl 是 1002：

验证	命令	预期
共享 netns	ls -L /proc/{1000,1001,1002}/ns/net	三个 inode 数字相同
共享 utsns	ls -L /proc/{1000,1001,1002}/ns/uts	三个 inode 数字相同
独立 pidns	ls -L /proc/{1001,1002}/ns/pid	nginx 与 curl 不同
独立 mntns	ls -L /proc/{1001,1002}/ns/mnt	nginx 与 curl 不同
localhost 互通	在 curl 容器里 curl 127.0.0.1:80	命中 nginx
独立 ps	nginx 容器里 ps、curl 容器里 ps	各自只看到自己的进程
独立 rootfs	nginx 容器里 ls / 是 nginx 镜像，curl 容器里是 curl 镜像	互不可见
localhost 互通的内核解释：因为指向同一个 net_ns，意味着同一份网卡列表（同一个 lo）、同一个路由表、同一个 TCP 连接表、同一段 IP（Pod IP）。nginx 把 80 端口绑到 0.0.0.0 时，curl 容器一发 127.0.0.1:80，内核在共享的 lo 设备上把包送给同一个 net_ns 里监听的 socket——也就是 nginx。

六、几个容易被忽略的细节
1. clone vs setns 为什么要分开？
理论上完全可以全用 setns，但 mount namespace 必须早进入（pivot_root 依赖独立的 mnt_ns），而 setns(CLONE_NEWNS) 在 Go 多线程运行时里有大坑（见 pkg/nsenter 的 unshare CLONE_FS 注释）。所以本项目策略是：能在 clone 时新建的就 clone 时建，需要"加入已有"的就 setns。

2. 为什么不 join PID namespace？
K8s 默认 Pod 内多容器不共享 PID namespace（避免一个容器看到别人的进程）。PID: true 会让每个容器有自己的 init=1 进程视图。如果 Pod spec 里设置 shareProcessNamespace: true，CRI 层应该把 pid 也加进 joinNS——本项目目前没实现这块。

3. 为什么不 join mount namespace？
Mount namespace 必须和镜像 rootfs绑定。每个容器跑不同的镜像，pivot_root 必须在自己独立的 mnt_ns 里做，否则会污染其它容器看到的文件系统。

4. CNI 网络是怎么和 sandbox 的 netns 关联的？
CNI 在 RunPodSandbox 阶段调用，作用对象就是 /proc/<pause-pid>/ns/net：CNI 插件在那个 netns 里建 veth pair、配 IP、加路由。等 StartContainer 让业务容器 setns 进同一个 netns 时，网络已经布好了，容器一启动就能直接用 Pod IP 通信。

一句话本质
"共享 namespace" = 多个进程的内核 nsproxy 指针指向同一个 namespace 对象。 mini-docker 的实现 = clone() 时不要新建（清掉 CLONE_NEW* 位）+ 子进程启动后用 setns(open("/proc/<sb.PID>/ns/<ns>")) 主动加入沙箱的 namespace。