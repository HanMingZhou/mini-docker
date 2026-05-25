# RuntimeService 与 ImageService

它们是 Kubernetes CRI (Container Runtime Interface) 协议定义的两个 gRPC 服务，是 Kubelet 与底层容器运行时（containerd / cri-o / ）之间的唯一标准接口。
定义文件在 Kubernetes 源码 k8s.io/cri-api/pkg/apis/runtime/v1/api.proto

## 一、为什么要有这两个服务？

历史背景：Kubernetes 早期硬编码 Docker 作为运行时（叫 dockershim），代码耦合非常严重。2016 年 K8s 抽象出 CRI，让 Kubelet 只面向一个稳定的 gRPC 接口编程，谁实现这个接口谁就能当 K8s 的运行时。

职责切分逻辑：

| 维度       | RuntimeService               | ImageService                               |
| ---------- | ---------------------------- | ------------------------------------------ |
| 关心的对象 | 进程 / namespace / cgroup    | 镜像文件 / layer / registry                |
| 状态       | 有状态（运行中的 Pod、容器） | 主要操作本地存储                           |
| 生命周期   | 频繁变化（启停容器、Exec）   | 慢变化（pull、删除）                       |
| 谁调用     | Kubelet 主循环               | Kubelet 的 image manager（独立 goroutine） |

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

### （一）背景：gRPC 的 service 在 Go 里是 interface

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

### （二）问题：interface 演进 vs 你的代码

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

| Kubelet 调用    | 实际触发       | 返回                      |
| --------------- | -------------- | ------------------------- |
| Version         | 你写的方法     | 正常响应                  |
| Status          | 你写的方法     | 正常响应                  |
| RunPodSandbox   | 嵌入的默认方法 | gRPC error: Unimplemented |
| CreateContainer | 嵌入的默认方法 | gRPC error: Unimplemented |

代码编译通过、可以注册到 gRPC server，只是大部分 RPC 客户端调用时会拿到 Unimplemented 错误。

本项目的实际情况
"本项目几乎重写了所有方法"
意思是：在 pkg/cri/ 各个文件里，\*RuntimeService 已经显式实现了：
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

### （三）一句话总结

UnimplementedXxxServer 是 protoc 自动生成的"打底空壳"，通过 Go 的结构体嵌入让你的实现默认满足整个 interface；你只需要"挑你关心的 RPC 重写一下"，未来 cri-api 加新方法也不会破坏你的代码——这就是 gRPC Go 的前向兼容约定。

题外话：这个模式在 gRPC Go 里是强烈推荐做法，新版本的 protoc-gen-go-grpc 甚至默认要求你必须嵌入它，否则编译会拒绝（require_unimplemented_servers=true）

### （四）Kubelet 是 client，mydocker-cri 是 server

Kubelet 来找你，不是你去找 Kubelet

理解了这个方向，剩下的疑问就全部消解了。下面把整条链路拆开讲。

#### （一）CRI 是个 RPC 协议，谁是 server，谁是 client？

```text
┌─────────────┐    gRPC over Unix Socket    ┌──────────────────┐
│   Kubelet   │ ──────── client ──────────► │   mydocker-cri   │
│  (k8s 进程) │ ◄─────── response ──────── │   (你的进程)     │
└─────────────┘                             └──────────────────┘
                  /var/run/mydocker-cri.sock
                  (server 监听这里)
```

server：mydocker-cri 进程，监听 Unix socket，被动等连接
client：Kubelet（也包括 crictl 这个调试工具），主动连过来发请求
所以 mydocker-cri 根本不需要"知道 Kubelet 在哪里"——它只需要在一个约定好的 Unix socket 路径上监听，等别人来敲门。

#### （二）RegisterRuntimeServiceServer 到底做了什么

```go
runtime.RegisterRuntimeServiceServer(s.grpc, s.runtime)
runtime.RegisterImageServiceServer(s.grpc, s.image)
```

这两行根本不涉及"连接"，更没有"端口"。它只是在进程内部做一件事：把方法实现注册到本地 gRPC server 的路由表里。

可以把 gRPC server 想象成一个 HTTP web 框架（比如 Express、Gin）。"注册" 相当于：

```go
// 类比：HTTP framework
app.post('/runtime.v1.RuntimeService/RunPodSandbox', s.runtime.RunPodSandbox)
app.post('/runtime.v1.RuntimeService/CreateContainer', s.runtime.CreateContainer)
app.post('/runtime.v1.ImageService/PullImage',         s.image.PullImage)
// ...
```

Register\*ServiceServer 是 protoc 自动生成的纯本地函数，它做的事相当于这样（伪代码）：

```go
func RegisterRuntimeServiceServer(s *grpc.Server, srv RuntimeServiceServer) {
    s.RegisterService(&_RuntimeService_serviceDesc, srv)
}

// _RuntimeService_serviceDesc 里写明了：
//   service name = "runtime.v1.RuntimeService"
//   methods = [
//     {"Version",         srv.Version},
//     {"RunPodSandbox",   srv.RunPodSandbox},
//     {"CreateContainer", srv.CreateContainer},
//     ...
//   ]
```

注册完之后，gRPC server 内部维护的路由表大概长这样：

```bash
"/runtime.v1.RuntimeService/Version"          → s.runtime.Version
"/runtime.v1.RuntimeService/RunPodSandbox"    → s.runtime.RunPodSandbox
"/runtime.v1.RuntimeService/CreateContainer"  → s.runtime.CreateContainer
... (RuntimeService 的 25+ 个 RPC)
"/runtime.v1.ImageService/PullImage"          → s.image.PullImage
"/runtime.v1.ImageService/ListImages"         → s.image.ListImages
... (ImageService 的 5 个 RPC)
```

关键点：两个 service 都注册到同一个 s.grpc 实例（grpc.NewServer() 出来的那个对象），所以它们最终被同一个 socket 监听——s.cfg.Socket。

#### （三）那 socket 是怎么对上的

往上几行看 Start()：

```go
func (s *Server) Start() error {
    startReaper()

    // 清理之前的 socket 文件（daemon 被 kill 掉时不会自动删）
    if err := os.Remove(s.cfg.Socket); err != nil && !os.IsNotExist(err) {
        return fmt.Errorf("remove stale socket %s: %w", s.cfg.Socket, err)
    }
    if err := os.MkdirAll(filepath.Dir(s.cfg.Socket), 0755); err != nil {
        return fmt.Errorf("mkdir socket dir: %w", err)
    }
    lis, err := net.Listen("unix", s.cfg.Socket)
    if err != nil {
        return fmt.Errorf("listen unix %s: %w", s.cfg.Socket, err)
    }
    ...
    runtime.RegisterRuntimeServiceServer(s.grpc, s.runtime)
    runtime.RegisterImageServiceServer(s.grpc, s.image)

    if err := s.grpc.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
        return err
    }
    return nil
}
```

逻辑很直白：

net.Listen("unix", s.cfg.Socket) —— 在文件系统上创建 Unix socket 文件并 listen。s.cfg.Socket 默认是 /var/run/mydocker-cri.sock（见 @c:/project/mini-docker/pkg/cri/server.go:36），通过 CLI flag --socket 可改。
Register\* —— 往 s.grpc 的路由表写表项。
s.grpc.Serve(lis) —— 让 gRPC server 在这个 listener 上跑起来：accept 连接、解析 HTTP/2 帧、按方法名路由到注册的 handler、阻塞返回。
所以"两个服务在哪个端口"的答案是：

没有端口，它们共用一个 Unix domain socket 文件：/var/run/mydocker-cri.sock（或你用 --socket 配的路径）。
Unix socket 不是 TCP，没有端口号，就是文件系统里的一个特殊文件。两端通过文件路径"碰头"。

#### （四）Kubelet 怎么找到这个 socket

Kubelet 通过配置/启动参数知道 socket 路径，没有自动发现。
部署时你做了这件事（README 里写得很清楚）：

```bash
sudo tee /etc/crictl.yaml >/dev/null <<'EOF'
runtime-endpoint: unix:///var/run/mydocker-cri.sock
image-endpoint: unix:///var/run/mydocker-cri.sock
timeout: 10
EOF

sudo crictl info | head -20

# 类似地，Kubelet 启动参数有：
--container-runtime-endpoint=unix:///var/run/mydocker-cri.sock
--image-service-endpoint=unix:///var/run/mydocker-cri.sock   # 较新版本默认与上一个相同
```

Kubelet 启动后会做：

```go
// 伪代码（kubelet 内部）
runtimeConn, _ := grpc.Dial("unix:///var/run/mydocker-cri.sock", ...)
runtimeClient := runtime.NewRuntimeServiceClient(runtimeConn)  // ← 这是 Register* 的对偶
imageClient   := runtime.NewImageServiceClient(runtimeConn)
// 然后调用：
runtimeClient.Version(ctx, &VersionRequest{})
runtimeClient.RunPodSandbox(ctx, &RunPodSandboxRequest{...})
imageClient.PullImage(ctx, &PullImageRequest{...})
```

grpc.Dial("unix:///...") 就是 client 端"敲门"动作。Kubelet 进程成为这个 socket 的客户端，跟 mydocker-cri 建立长连接，后续所有 RPC 都通过这条连接。

注意：Kubelet 用 NewRuntimeServiceClient 和 NewImageServiceClient 创建了两个 client 对象，但它们底层共用同一个 runtimeConn，因为 socket 是同一个。这就是"两个 service，一个连接"的来历。

#### （五）完整的"启动 + 第一个 RPC"时序

```text
                                   ┌─────────────────────────────────┐
                                   │ /var/run/mydocker-cri.sock      │
                                   └─────────────────────────────────┘
                                                ▲   ▲
1. systemctl start mydocker-cri                │   │
   ──► main.go:newServeCmd                     │   │
   ──► cri.New(Config{Socket: "/var/run/..."}) │   │
   ──► Server.Start()                          │   │
        ├─ os.Remove(socket)                   │   │
        ├─ net.Listen("unix", socket) ────────►┘   │  (创建 socket 文件)
        ├─ grpc.NewServer()                        │
        ├─ RegisterRuntimeServiceServer(grpc, ...) │  (建路由表)
        ├─ RegisterImageServiceServer(grpc, ...)   │
        └─ grpc.Serve(lis) ─────── 阻塞 accept ───┘

2. systemctl start kubelet
   ──► kubelet 读启动参数 --container-runtime-endpoint=unix:///var/run/...
   ──► grpc.Dial("unix:///var/run/mydocker-cri.sock") ───► 连接建立
   ──► runtimeClient = NewRuntimeServiceClient(conn)
   ──► imageClient   = NewImageServiceClient(conn)

3. Kubelet 做心跳
   ──► runtimeClient.Version(ctx, &VersionRequest{})
       │
       │ 在 wire 上发送 HTTP/2 frame：
       │   :method = POST
       │   :path   = /runtime.v1.RuntimeService/Version
       │   body    = protobuf(VersionRequest)
       ▼
   mydocker-cri 收到帧：
       ──► 按 :path 在路由表查找
       ──► 命中 s.runtime.Version
       ──► 调 loggingInterceptor → s.runtime.Version(ctx, req)
       ──► 返回 VersionResponse
       ──► 序列化成 protobuf 发回
   ▲
   └── Kubelet 收到响应
```

#### （六）回到你的原问题

"RegisterRuntimeServiceServer(s.grpc, s.runtime) 是怎么知道 kubelet 的 runtime、image 这两个服务的端口呢？"

校正之后的答案：

Register 只是把方法注册到本地路由表
两个 service 共用一个 Unix socket 文件，没有端口
Kubelet 主动找 mydocker-cri（Kubelet 是 client）
Kubelet 通过启动参数 --container-runtime-endpoint 知道 socket 路径
两端"对上"靠的是共同约定的 Unix socket 路径，不是端口、不是注册中心、不是广播——就是文件系统里那个 .sock 文件。

#### （七）一些可能让你更踏实的实验

如果你想亲眼看见这件事，在 Linux 上可以做：

```bash
# 1. 看 mydocker-cri 监听的 socket 文件
sudo ls -l /var/run/mydocker-cri.sock
# srw-rw---- 1 root root 0 ... /var/run/mydocker-cri.sock
#  ↑ 's' 表示这是 socket

# 2. 看哪个进程在监听
sudo ss -lxp | grep mydocker-cri
# u_str LISTEN ... users:(("mydocker-cri",pid=12345,fd=7))

# 3. 看 kubelet 作为 client 连进来
sudo ss -xp | grep mydocker-cri
# u_str ESTAB ... users:(("kubelet",pid=...,fd=...))
#                 ↑ kubelet 是 client，建立了连接

# 4. 用 grpcurl 自己当 client，列出所有方法
sudo grpcurl -unix /var/run/mydocker-cri.sock list
# runtime.v1.ImageService
# runtime.v1.RuntimeService
sudo grpcurl -unix /var/run/mydocker-cri.sock list runtime.v1.RuntimeService
# runtime.v1.RuntimeService.Version
# runtime.v1.RuntimeService.RunPodSandbox
# ...
```

第 4 步特别有意思——这两个 service 名字 runtime.v1.RuntimeService 和 runtime.v1.ImageService 就是 Register\*ServiceServer 写进路由表的 key，是 proto 文件里 package runtime.v1; service RuntimeService { ... } 自动生成的。任何 client（Kubelet、crictl、grpcurl）只要能访问这个 socket、知道这个名字，就能调。

## 三、RuntimeService 的 RPC 大类

| 类别       | 典型 RPC                                                                                                                    | 本项目位置        |
| ---------- | --------------------------------------------------------------------------------------------------------------------------- | ----------------- |
| 握手/心跳  | Version、Status                                                                                                             | server.go:261-286 |
| Pod 沙箱   | RunPodSandbox / StopPodSandbox / RemovePodSandbox / PodSandboxStatus / ListPodSandbox                                       | sandbox.go        |
| 容器       | CreateContainer / StartContainer / StopContainer / RemoveContainer / ContainerStatus / ListContainers                       | container.go      |
| 执行       | ExecSync / Exec / Attach / PortForward                                                                                      | exec.go           |
| 统计与配置 | ContainerStats / ListContainerStats / PodSandboxStats / UpdateRuntimeConfig / UpdateContainerResources / ReopenContainerLog | stats.go          |

## 四、关键概念：什么是 Pod 沙箱

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

1. PullImage(image) ── ImageService
2. RunPodSandbox(podConfig) ── RuntimeService → 拿到 sandboxId, podIP
3. CreateContainer(sandboxId, containerConfig) ── RuntimeService → 拿到 containerId
4. StartContainer(containerId) ── RuntimeService
5. (循环) ContainerStatus / Status ── RuntimeService 心跳

## 五、ImageService 是什么

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

| RPC         | 作用                                                   | 本项目实现       |
| ----------- | ------------------------------------------------------ | ---------------- |
| PullImage   | 从 registry 拉镜像                                     | image.go:27-53   |
| ListImages  | 列出本地所有镜像                                       | image.go:56-74   |
| ImageStatus | 查询单个镜像（含 size、digest）                        | image.go:77-94   |
| RemoveImage | 删除本地镜像                                           | image.go:97-117  |
| ImageFsInfo | 镜像存储盘的使用量（给 Kubelet 做 disk pressure 判断） | image.go:120-147 |

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

## 六、两者怎么协作？以 mini-docker 为例

```text
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
```

两个 service 在同一个 gRPC server 上注册，共用 socket：

```go
runtime.RegisterRuntimeServiceServer(s.grpc, s.runtime)
runtime.RegisterImageServiceServer(s.grpc, s.image)
```

它们之间不直接调用对方，而是通过共享的 root 目录（/var/lib/mydocker/）解耦：

ImageService.PullImage 把 layer 写到 root/layers/、manifest 写到 root/images/
RuntimeService.CreateContainer 读 root/images/<name>/manifest.json，用 overlayfs 在 root/containers/<id>/ 把 layer 组装成 rootfs
这种"通过文件系统解耦"的设计就是 CRI 拆分两个 service 的目的——理论上 ImageService 可以是完全独立的 daemon 写到共享卷，RuntimeService 只读。

## 七、Kubelet 怎么连这两个 service

/etc/crictl.yaml 和 Kubelet 启动参数都有两个 endpoint：

```yaml
runtime-endpoint: unix:///var/run/mydocker-cri.sock
image-endpoint: unix:///var/run/mydocker-cri.sock
```

99% 情况两个值相同（同一个 daemon），但协议允许不同，例如：
用 containerd 跑容器
用 stargz-snapshotter 作为独立 image 服务做懒加载
这就是上一轮回答里"协议允许但通常不这么用"的具体含义。

记忆口诀
RuntimeService = "动的东西"：Pod、容器、进程、Exec、Stats
ImageService = "静的东西"：镜像文件、磁盘占用
两者通过 CRI 协议统一接口，通过 共享数据目录解耦实现

## 八、sock 是什么

.sock 文件是 Unix Domain Socket（Unix 域套接字），俗称 "Unix socket"。
它是操作系统提供的一种进程间通信（IPC）机制，跟 TCP/UDP 是同一级别的概念，但用法和性质完全不同。

### 1. 直观对比 TCP socket

| 维度             | TCP socket                               | Unix domain socket                |
| ---------------- | ---------------------------------------- | --------------------------------- |
| 寻址方式         | IP + 端口 (127.0.0.1:10250)              | 文件系统路径 (/var/run/foo.sock)  |
| 协议栈           | 走 TCP/IP，要经过网卡驱动、IP 层、TCP 层 | 直接在内核内存里拷贝，不走网络栈  |
| 跨主机           | 可以                                     | 不可以，只能同一台机器            |
| 性能             | 较慢                                     | 快几倍（无 checksum、无路由查找） |
| 权限控制         | 靠防火墙                                 | 靠文件系统权限（chmod / chown）   |
| 在 ss/netstat 里 | TCP 列表                                 | Unix 列表（ss -lx）               |

.sock 的 .sock 后缀只是约定俗成的命名习惯，不是必须的——内核根本不看后缀。
叫 foo.sock、bar.unix、baz 都行，只要双方约定好路径。
Docker 用 /var/run/docker.sock、
containerd 用 /run/containerd/containerd.sock、
systemd 用 /run/systemd/notify，都是同样的东西。

### 2. 在文件系统里长什么样

```bash
$ ls -l /var/run/mydocker-cri.sock
srw-rw---- 1 root root 0 Nov 20 10:00 /var/run/mydocker-cri.sock
^
└── 第一个字符是 's'，表示这是 socket 文件
   - 普通文件: '-'
   - 目录:    'd'
   - 软链接:  'l'
   - 块设备:  'b'
   - 字符设备:'c'
   - 管道:    'p'
   - socket:  's'  ← 这就是
```

它不是普通文件，里面没数据，size 永远 0。cat foo.sock 会卡住或报错。它只是文件系统里的一个"特殊节点"，作为内核里那个 socket 对象的"门牌号"。

### 3. 为什么 CRI 选 Unix socket 而不是 TCP

mydocker-cri 完全可以监听 127.0.0.1:10350，但 CRI 标准选了 Unix socket 有几个原因：
安全：Unix socket 用文件权限控制访问。chmod 0660 + chown root:root 就能确保只有 root（也就是 Kubelet）能调用，不需要担心 TCP 端口被外部扫到。

```go
// Kubelet 是 root，但我们仍设置 0660 便于本机调试（crictl 通常用 root）
if err := os.Chmod(s.cfg.Socket, 0660); err != nil {
    _ = lis.Close()
    return fmt.Errorf("chmod socket: %w", err)
}
```

本机通信场景明确：CRI 协议里有传文件描述符（fd 传递）、流式 stdio（Exec/Attach）这种需求，TCP 做起来麻烦，Unix socket 原生支持 SCM_RIGHTS 传 fd。
性能：Kubelet 每秒可能调几十次 Status / List，绕开 TCP 协议栈能省 CPU。
避免端口冲突：用一个文件路径，不用全局协调端口号。

### 4. 在代码里它是怎么"出现"的

这一行就是 socket 文件诞生的地方：

```go
lis, err := net.Listen("unix", s.cfg.Socket)
if err != nil {
    return fmt.Errorf("listen unix %s: %w", s.cfg.Socket, err)
}
```

net.Listen("unix", "/var/run/mydocker-cri.sock") 内部最终调用了 Linux 的 socket(AF_UNIX, SOCK_STREAM, 0) + bind() 系统调用，让内核：
创建一个 socket 内核对象
在文件系统的 /var/run/mydocker-cri.sock 路径上落一个 socket 类型的 inode，关联到这个内核对象
所以关掉进程时这个文件不会自动消失，需要主动删——这就是为什么 Start() 第一步要清 stale socket：

```go
// 清理之前的 socket 文件（daemon 被 kill 掉时不会自动删）
if err := os.Remove(s.cfg.Socket); err != nil && !os.IsNotExist(err) {
    return fmt.Errorf("remove stale socket %s: %w", s.cfg.Socket, err)
}
```

### 5. 客户端怎么连

Client 端用同样的路径，比如 crictl / grpcurl / Kubelet 内部都是：

```go
grpc.Dial("unix:///var/run/mydocker-cri.sock", ...)  // 注意 unix:// 前缀
```

unix:// 这个 scheme 告诉 gRPC："不要解析成主机名，把后面的路径当 socket 文件路径用"。

.sock = 文件系统里的一个"socket 类型节点"，是同主机内进程间通信的入口，用文件路径代替 IP+端口寻址，安全（靠文件权限）、快速（不过网络栈）、本地专用。

## 九、CoreDNS / DNS 相关概念

### 第一部分：K8s 用户视角的概念

#### 1 · CoreDNS：集群里的"114 查号台"

K8s 集群里每个 Pod 启动时都被注入一个统一的 DNS 服务器地址（10.96.0.10）。
Pod 里 nslookup nginx.default 这种查询全部发给这个地址，这个地址背后跑的就是 CoreDNS——一个用 Go 写的 DNS 服务器。
为什么要它？因为 K8s 里 Service 的 IP 经常变，Pod IP 更经常变，进程之间不能直接写死 IP 互相找，必须靠"域名"——CoreDNS 把 nginx.default.svc.cluster.local 解析成当前 Service 的 ClusterIP。
K8s 里它本身是个普通 Pod，跑在 kube-system namespace。CoreDNS 在容器里读不到 resolv.conf——它自己也是个容器，跑起来时自己也要读 /etc/resolv.conf

#### 2 · CoreDNS 的 forward 插件：把不会的题转给学霸

CoreDNS 内部是插件式架构。一个典型 Corefile 长这样：

```bash
.:53 {
    kubernetes cluster.local {...}    ← 集群内域名我自己答
    forward . /etc/resolv.conf        ← 不是集群域名的，转给上游
    cache 30
}
```

forward . 后面跟的是上游 DNS 服务器地址。
K8s 部署 CoreDNS 时填的是字面字符串 /etc/resolv.conf——意思是"读这个文件，把里面的 nameserver 当上游"。

如果容器里没有 /etc/resolv.conf，CoreDNS 启动时会做这件事：
试图读 /etc/resolv.conf
→ 文件不存在
→ 那就把 "/etc/resolv.conf" 这个字符串本身当 IP 解析
→ 失败：plugin/forward: not an IP address or file: "/etc/resolv.conf"
→ 启动失败
→ 容器 crash
修复方案要做的就是确保容器 rootfs 里有这个文件——这是 pkg/cri/etcfiles.go 整个文件存在的理由。

#### 3 · liveness probe：K8s 的"哨兵"

Kubelet 周期性（默认 10 秒）去敲打容器一下：

```yaml
livenessProbe:
  tcpSocket:
    port: 8080
  periodSeconds: 10
```

意思是"每 10 秒去 TCP 连一下 8080 端口"。能连上就活，连不上累计几次就重启容器。
CoreDNS 的 livenessProbe 是 HTTP :8080/health。它启动失败 → 8080 没起 → liveness 失败 → Kubelet 重启。重启又失败 → 再重启……

#### 4 · Kubelet：节点上的执行员

每个 K8s 节点都有一个常驻的 Kubelet 进程。它做的事：

- 跟 K8s 控制面通信（"我这个节点上要跑哪些 Pod？"）
- 调 CRI 接口（gRPC over Unix socket）让运行时（mydocker-cri）真正起容器
- 跑 livenessProbe / readinessProbe
- 上报节点和 Pod 状态

我们之前讲过：Kubelet 是 client，mydocker-cri 是 server。Kubelet 调 CRI 的 RunPodSandbox / CreateContainer / StartContainer 这些 RPC，传过来的参数就是下面要讲的 PodSandboxConfig。

#### 5 · PodSandboxConfig / DnsConfig：Kubelet 传来的"清单"

```go
func (s *RuntimeService) RunPodSandbox(_ context.Context, req *runtime.RunPodSandboxRequest) (*runtime.RunPodSandboxResponse, error) {
    if req.Config == nil || req.Config.Metadata == nil {
        return nil, fmt.Errorf("RunPodSandbox: missing config/metadata")
    }
    md := req.Config.Metadata
// req.Config 就是 PodSandboxConfig，是一个很大的 protobuf 消息，包含 Kubelet 想让你创建这个 Pod 时知道的所有信息：
type PodSandboxConfig struct {
    Metadata     *PodSandboxMetadata  // 名字/namespace/UID
    Hostname     string               // ← 38 行用的字段
    LogDirectory string
    DnsConfig    *DNSConfig           // ← 37 行判空的字段
    PortMappings []*PortMapping
    Labels       map[string]string
    Annotations  map[string]string
    Linux        *LinuxPodSandboxConfig  // cgroupParent / hostNetwork
}

type DNSConfig struct {
    Servers  []string  // ["10.96.0.10"]
    Searches []string  // ["default.svc.cluster.local", ...]
    Options  []string  // ["ndots:5"]
}
```

Kubelet 替我们填好了 DNS 服务器地址（cluster DNS = 10.96.0.10）、search 域、ndots 选项——我们只是搬运工。

#### 6 · cluster DNS（10.96.0.10）：K8s 怎么挑这个 IP

K8s 默认 Service 子网是 10.96.0.0/12。这个网段的第 10 个 IP（10.96.0.10）约定俗成给 CoreDNS Service 用。每个新建的 Pod，Kubelet 会把这个 IP 作为 nameserver 写进 req.Config.DnsConfig.Servers，然后调 RunPodSandbox 传过来。

我们的代码就是把这个 IP 拿出来：

```go
var dns sandbox.DNSConfig
if req.Config.DnsConfig != nil {
    dns = sandbox.DNSConfig{
        Servers:  append([]string(nil), req.Config.DnsConfig.Servers...),
        Searches: append([]string(nil), req.Config.DnsConfig.Searches...),
        Options:  append([]string(nil), req.Config.DnsConfig.Options...),
    }
}
```

最终它会被写进容器 /etc/resolv.conf 的 nameserver 10.96.0.10 那行。

#### 7 · HostAliases：Pod spec 里的"自定义 hosts"

```yaml
spec:
  hostAliases:
    - ip: "1.2.3.4"
      hostnames:
        - "foo.example.com"
```

效果：容器里 /etc/hosts 多一行 1.2.3.4 foo.example.com。我们的代码也支持：

```go
// 用户自定义 hostAliases
for _, a := range sb.HostAliases {
    if a.IP == "" || len(a.Hostnames) == 0 {
        continue
    }
    fmt.Fprintf(&b, "%s\t%s\n", a.IP, strings.Join(a.Hostnames, " "))
}
```

但注意

```go
hostAliases := make([]sandbox.HostAlias, 0, 0)
// CRI v1 没有 hostAliases 字段（K8s 通过 /etc/hosts mount 实现），保留扩展位。
```

CRI v1 协议本身没传 hostAliases——K8s 实际是用 ConfigMap + bind mount 把 /etc/hosts 整个换掉来实现的。所以现在我们这里收到的永远是空 slice，代码留位置占着，未来加扩展时不用改函数签名。

#### 8 · ConfigMap / bind mount 覆盖

ConfigMap 是 K8s 里存配置的资源。比如 CoreDNS 的 Corefile 就放在 ConfigMap 里：

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: coredns
data:
  Corefile: |
    .:53 {
        forward . /etc/resolv.conf
        ...
    }
# CoreDNS 的 Pod spec 里写：
volumeMounts:
  - name: config
    mountPath: /etc/coredns
volumes:
  - name: config
    configMap:
      name: coredns
```

Kubelet 会把这个 ConfigMap 物化成宿主机上的一个文件，然后通过 CRI 传给我们：

```go
req.Mounts = []*Mount{
    {HostPath: "/var/lib/kubelet/pods/<uid>/volumes/configmap/coredns/Corefile",
     ContainerPath: "/etc/coredns/Corefile"},
}
```

我们用 bind mount 把它挂进容器。

```go
// Apply bind mounts from CRI config (hostPath volumes, configMaps, secrets, etc.)
if len(cfg.Mounts) > 0 {
    for _, m := range cfg.Mounts {
        if m.HostPath == "" || m.ContainerPath == "" {
            continue
        }
        if err := bindMountIntoRootfs(mergedRoot, m.HostPath, m.ContainerPath, m.Readonly); err != nil {
            fmt.Fprintf(os.Stderr, "warn: mount %s → %s: %v\n", m.HostPath, m.ContainerPath, err)
        }
    }
}
```

关键：理论上 ConfigMap 也能指定挂到 /etc/resolv.conf 上来覆盖我们写的默认值。
所以写默认值要先于 bind mount——这就是第 85-90 行的注释：

```go
// 在 bind mount 之前生成 /etc/{resolv.conf,hosts,hostname}：
// 一来 bind mount 可能用 configmap 覆盖这几个文件（典型场景：CoreDNS Corefile），
// 二来 CoreDNS 起来必须能读 resolv.conf，否则 forward 插件初始化失败。
if err := writeContainerEtcFiles(mergedRoot, sb); err != nil {
    fmt.Fprintf(os.Stderr, "warn: write /etc files for %s: %v\n", id, err)
}
```

### 第二部分：Linux 文件 / 系统层

#### 9 · /etc/resolv.conf：DNS 客户端的配置

Linux 上几乎所有"想做 DNS 解析"的程序（curl、ping、nslookup、Go 的 net.LookupHost）都会读这个文件。它的格式：
nameserver 10.96.0.10 ← 主 DNS 服务器
nameserver 8.8.8.8 ← 备用
search default.svc.cluster.local svc.cluster.local cluster.local
options ndots:5
每行的含义：

```bash
nameserver：DNS 服务器 IP。最多 3 个（glibc 限制，下面讲），多了会被忽略。
search：搜索域。当你 nslookup nginx（不带点的短名），系统会依次试 nginx.default.svc.cluster.local、nginx.svc.cluster.local、nginx.cluster.local、最后 nginx.。
options ndots:5：当域名里点数 ≥ 5 才直接走 absolute 解析；否则先走 search 列表。这是 K8s 的标准设置。
```

我们生成它的代码：

```go
var b strings.Builder
for _, s := range dns.Servers {
    fmt.Fprintf(&b, "nameserver %s\n", s)
}
if len(dns.Searches) > 0 {
    fmt.Fprintf(&b, "search %s\n", strings.Join(dns.Searches, " "))
}
if len(dns.Options) > 0 {
    fmt.Fprintf(&b, "options %s\n", strings.Join(dns.Options, " "))
}
return []byte(b.String())
```

### 第三部分：项目内部概念

#### 10· rootfs / mergedRoot：容器看到的"根"

rootfs = root filesystem，容器进程跑起来后看到的 /。
镜像（比如 nginx:latest）由多层组成，磁盘上长这样：

```bash
/var/lib/mydocker/layers/
├── sha256:abc/diff/    ← 层 1：基础 OS（debian rootfs）
├── sha256:def/diff/    ← 层 2：装 nginx
└── sha256:ghi/diff/    ← 层 3：拷配置文件
```

启动容器时用 OverlayFS 把这几层"合并"成一个看起来像完整 Linux 根目录的目录：

```bash
/var/lib/mydocker/containers/<id>/merged/
├── bin/
├── etc/                ← 注意这里！
├── usr/
├── var/
└── ...
```

这个 merged 目录就是 mergedRoot，它是个真实存在于宿主机上的目录，但容器进程看到的"根"指向它。我们往它里面写文件等于"给容器加文件"：

```go
etc := filepath.Join(mergedRoot, "etc")
if err := os.MkdirAll(etc, 0755); err != nil {
    return fmt.Errorf("mkdir %s: %w", etc, err)
}
```

这里写 mergedRoot/etc/resolv.conf，容器里看到的就是 /etc/resolv.conf。

#### 11 · PrepareRootfs：把镜像 overlay 成 mergedRoot 的函数

```go
mergedRoot, err := imgStore.PrepareRootfs(imgName, containerDir)
if err != nil {
    _ = os.RemoveAll(containerDir)
    return nil, fmt.Errorf("prepare rootfs for image %s: %w", imgName, err)
}
```

它做三件事：
找到镜像每一层的目录
调 mount("overlay", mergedRoot, "overlay", lowerdir=层1:层2:层3, upperdir=..., workdir=...)
返回 mergedRoot 路径
之后所有"往 rootfs 里加文件"的操作（写 etc 文件、bind mount 卷）都基于这个返回值。

#### 12 · bind mount：Linux 的"软链接 plus"

普通软链接：ln -s /a /b，访问 /b 时内核会"重定向"到 /a，但 /b 在文件系统层面不是一个真目录。
bind mount：mount --bind /a /b，把 /a 这块挂载点绑到 /b 上。访问 /b 时就是在访问 /a——不是重定向，是同一个挂载点的两个入口。

容器场景为什么用 bind mount 而不是软链接：
软链接是文件系统级的——容器 pivot_root 后软链接的目标 /a 可能就不存在了
bind mount 是挂载层级的——pivot_root 不影响已经建立的挂载
所以 K8s 的 ConfigMap、Secret、HostPath 全部用 bind mount 实现：

```go
if err := bindMountIntoRootfs(mergedRoot, m.HostPath, m.ContainerPath, m.Readonly); err != nil {
```

把宿主机的 m.HostPath（比如 ConfigMap 物化出来的 Corefile 文件）bind 到容器内的 m.ContainerPath（比如 /etc/coredns/Corefile）。

#### 13 · sandbox / pause 进程

sandbox = 一个常驻睡死的 pause 进程 + 它持有的独立 net/uts/ipc namespace。Pod 内多容器通过 setns 进入这三栋"房子"，从而共享网络/主机名/IPC。
DNS 信息存在 sandbox 上（而不是 container 上）就是因为它也是 Pod 级的——Pod 内多容器看到的 /etc/hosts 应当一致：

```go
// /etc/* generation inputs (used by CRI when preparing per-container rootfs).
Hostname    string      `json:"hostname,omitempty"`
DNS         DNSConfig   `json:"dns,omitempty"`
HostAliases []HostAlias `json:"host_aliases,omitempty"`
```

#### 21 · mount namespace：之前讲过的"文件系统视图隔离"

容器有自己的 mount namespace，意味着：
它在自己的 mount namespace 里 pivot_root，不影响宿主机
它新挂载的东西（包括我们的 bind mount）只在它自己里可见
写 mergedRoot/etc/resolv.conf 必须在子进程进入新 mount namespace 之前完成——因为我们写的是宿主机上的目录，子进程切了 mount ns 后看到的根才会是 mergedRoot
这就是为什么 writeContainerEtcFiles 在 pkg/cri/container.go 的 CreateContainer（父进程上下文）里调用，而不是在 init 子进程里。

### Q1：为什么 cluster DNS 是 10.96.0.10？是约定还是必然？

是约定，不是必然。具体是这样来的：
决定 10.96.0.10 的两条配置
K8s 集群启动时（kubeadm init）会决定两个东西：
Service CIDR（Service 用的 IP 网段）
kubeadm 默认：10.96.0.0/12
你也可以 kubeadm init --service-cidr=192.168.0.0/16
集群 DNS 的 Service IP
kubeadm 默认：Service CIDR 的第 10 个 IP（也就是 10.96.0.10）
你也可以 kubeadm init --service-dns-domain=cluster.local --kubelet-extra-args=--cluster-dns=10.96.0.20
也就是说，只要你用 kubeadm 默认配置，就一定是 10.96.0.10。
但本质上它只是 K8s 集群里一个普通 Service 的 ClusterIP——你 kubectl get svc -n kube-system kube-dns 能看到：

```bash
$ kubectl get svc -n kube-system kube-dns
NAME       TYPE        CLUSTER-IP    PORT(S)
kube-dns   ClusterIP   10.96.0.10    53/UDP,53/TCP
```

它叫 kube-dns 是历史命名（K8s 早期 DNS 实现叫 kube-dns，后来换成 CoreDNS，Service 名没改）。
这个 IP 怎么传到 Kubelet 的？
你装 K8s 时 Kubelet 配置文件 /var/lib/kubelet/config.yaml 里写了：

```yaml
clusterDNS:
  - 10.96.0.10
clusterDomain: cluster.local
```

Kubelet 读这个配置，在创建每个 Pod 时把它填进 DnsConfig.Servers，然后调我们的 RunPodSandbox。
所以这个 IP 的来源链路是：

```bash
kubeadm init 默认配置
   └─ 决定 Service CIDR = 10.96.0.0/12
      └─ 决定 cluster DNS = 第 10 个 IP = 10.96.0.10
         └─ 写进 Kubelet config (clusterDNS 字段)
            └─ Kubelet 启 Pod 时填进 PodSandboxConfig.DnsConfig.Servers
               └─ RunPodSandbox RPC 传给我们
                  └─ 我们写进容器 /etc/resolv.conf
```

### Q2：CoreDNS 怎么把域名解析成 ClusterIP？Kubelet 什么时候填 DNS

CoreDNS 的解析过程
CoreDNS 启动时加载 Corefile（K8s 用 ConfigMap 存）：

```text
.:53 {
    errors
    health :8181
    ready
    kubernetes cluster.local in-addr.arpa ip6.arpa {     ← 关键插件
        pods insecure
        fallthrough in-addr.arpa ip6.arpa
    }
    forward . /etc/resolv.conf                            ← 不认识的转上游
    cache 30
    loop
    reload
    loadbalance
}
```

kubernetes 这个插件是核心。它做这两件事：
启动时通过 K8s API（https://<apiserver>:6443）watch 所有 Service 和 Endpoints 资源，在内存里建一张表：
nginx.default.svc.cluster.local → 10.96.123.45
coredns.kube-system.svc.cluster.local → 10.96.0.10
...
收到 DNS 查询时，按 <service>.<namespace>.svc.<clusterDomain> 模式拆解，查表返回。
整个解析过程不是"再问别人"，是 CoreDNS 自己内存里查——所以才叫"集群内服务发现"。

Kubelet 什么时候填的？
Kubelet 在调 RunPodSandbox 之前就准备好了 PodSandboxConfig。为每个 Pod做的事：
对每个新建的 Pod：

1. 读 Pod spec 看 dnsPolicy 字段（默认 ClusterFirst）
2. 根据 dnsPolicy 决定 DnsConfig：
   - ClusterFirst: [10.96.0.10] ← 大多数业务 Pod
   - Default: 读宿主机 /etc/resolv.conf 抄一份
   - None: 完全用 Pod spec 里的 dnsConfig
   - ClusterFirstWithHostNet: 同 ClusterFirst 但用宿主网络
3. 把 DnsConfig 装进 PodSandboxConfig
4. gRPC 调 mydocker-cri.RunPodSandbox(req)
   CoreDNS 自己的 Pod spec 里写着 dnsPolicy: Default——它不能用 ClusterFirst，否则它要解析自己（鸡生蛋问题）。所以 Kubelet 给 CoreDNS Pod 准备的 DnsConfig 是宿主机 /etc/resolv.conf 的内容。

我们项目里收到这个填好的 DnsConfig 后，从这里抽取：

```go
var dns sandbox.DNSConfig
if req.Config.DnsConfig != nil {
    dns = sandbox.DNSConfig{
        Servers:  append([]string(nil), req.Config.DnsConfig.Servers...),
        Searches: append([]string(nil), req.Config.DnsConfig.Searches...),
        Options:  append([]string(nil), req.Config.DnsConfig.Options...),
    }
}
```

### Q3：10.96.0.10 在我们项目代码里到底怎么"被用了"？

它从 Kubelet 进来后，要走完完整 5 步链路才生效。我把每一步贴出来：
Step 1: 进入 mydocker-cri，存进 sandbox 内存

```go
var dns sandbox.DNSConfig
if req.Config.DnsConfig != nil {
    dns = sandbox.DNSConfig{
        Servers:  append([]string(nil), req.Config.DnsConfig.Servers...),  // ← ["10.96.0.10"]
        Searches: append([]string(nil), req.Config.DnsConfig.Searches...),
        Options:  append([]string(nil), req.Config.DnsConfig.Options...),
    }
}
```

Step 2: 透传给 sandbox.Manager

```go
sandbox.StartOptions{
    LogDir:       req.Config.LogDirectory,
    Labels:       req.Config.Labels,
    Annotations:  req.Config.Annotations,
    CgroupParent: cgroupParent,
    HostNetwork:  hostNetwork,
    Hostname:     hostname,
    DNS:          dns,                ← ["10.96.0.10"] 跟到这里
    HostAliases:  hostAliases,
},
```

Step 3: 落到 Sandbox 结构体并持久化

```go
Hostname:     opts.Hostname,
DNS:          opts.DNS,           ← 存进 *Sandbox
HostAliases:  opts.HostAliases,
```

之后 m.save(sb) 把整个 Sandbox 写到磁盘的 sandboxes/<id>/sandbox.json，里面会有：

```json
{
  "id": "...",
  "dns": {
    "servers": ["10.96.0.10"],
    "searches": [
      "default.svc.cluster.local",
      "svc.cluster.local",
      "cluster.local"
    ],
    "options": ["ndots:5"]
  }
}
```

Step 4: CreateContainer 时取出来用

```go
if err := writeContainerEtcFiles(mergedRoot, sb); err != nil {
    fmt.Fprintf(os.Stderr, "warn: write /etc files for %s: %v\n", id, err)
}
```

把 sb（包含 DNS）传给 etc 文件生成函数。
Step 5: 真正写到容器 rootfs 里

```go
var b strings.Builder
for _, s := range dns.Servers {
    fmt.Fprintf(&b, "nameserver %s\n", s)        ← 这里把 "10.96.0.10" 拼成
                                                    "nameserver 10.96.0.10\n"
}
if len(dns.Searches) > 0 {
    fmt.Fprintf(&b, "search %s\n", strings.Join(dns.Searches, " "))
}
if len(dns.Options) > 0 {
    fmt.Fprintf(&b, "options %s\n", strings.Join(dns.Options, " "))
}
return []byte(b.String())
```

最终内容写进 <mergedRoot>/etc/resolv.conf：

```bash
nameserver 10.96.0.10
search default.svc.cluster.local svc.cluster.local cluster.local
options ndots:5
```

Step 6（容器运行时）：业务进程读这个文件
业务容器（比如 nginx）启动后调用 getaddrinfo("kubernetes.default") → glibc 读 /etc/resolv.conf → 看到 nameserver = 10.96.0.10 → 把 DNS 查询发到这个 IP → kube-proxy 的 iptables 规则把它转给 CoreDNS Pod IP → CoreDNS 返回结果。
10.96.0.10 的"用处"在 Step 5 落盘那一刻就完成了——剩下都是 Linux/glibc 的事。

完整的链路是这样：

```text
业务 Pod (dnsPolicy: ClusterFirst)
  /etc/resolv.conf
    nameserver 10.96.0.10              ← CoreDNS Service ClusterIP
                  │
                  ▼  K8s Service → kube-proxy iptables → CoreDNS Pod IP
CoreDNS Pod (dnsPolicy: Default)
  /etc/resolv.conf
    nameserver 10.255.141.8            ← 抄宿主机的
    nameserver 127.0.0.53              ← 抄宿主机的
                  │
                  │  CoreDNS 内部：先查 kubernetes 插件
                  │  （集群域名 → 直接返回）
                  │  再走 forward 到 /etc/resolv.conf 的 nameserver
                  │  （非集群域名 → 转发）
                  ▼
真实上游 DNS (10.255.141.8 / 127.0.0.53)
                  │
                  ▼ （127.0.0.53 是 systemd-resolved，自己又递归到外网）
最终公网 DNS（比如 8.8.8.8 或运营商 DNS）
```

两层 forward：
业务 Pod 把 DNS 发给 CoreDNS（10.96.0.10）—— 集群内服务发现
CoreDNS 把不认识的转给宿主网络上的 DNS——能走外网
集群域名（比如 nginx.default.svc.cluster.local）只在第 1 步就被 CoreDNS 答了，根本不会走到第 2 步。第 2 步只服务非集群域名（比如 www.google.com）。

题外细节：业务 Pod 和 CoreDNS Pod 看到的 resolv.conf 内容不一样
我们项目代码里正好对应这个区别：

```go
func resolvConfContent(dns sandbox.DNSConfig) []byte {
    if len(dns.Servers) == 0 && len(dns.Searches) == 0 && len(dns.Options) == 0 {
        // 容器没收到任何 DNS 配置（独立 mydocker run 或 Kubelet 未传）：
        // 复制宿主机配置作为兜底，让容器能解析外网。
        if data, err := os.ReadFile("/etc/resolv.conf"); err == nil {
            return data
        }
        // 宿主也读不到——再不济也写 8.8.8.8，防止 forward 插件失败。
        return []byte("nameserver 8.8.8.8\n")
    }
```

业务 Pod：Kubelet 传了 [10.96.0.10] → 走上面的非空分支 → 写 nameserver 10.96.0.10
CoreDNS Pod（dnsPolicy: Default）：Kubelet 传的就是宿主机 /etc/resolv.conf 内容 → 也走非空分支
独立 mydocker run 调试场景（没经过 K8s）：dns 是空 → 走兜底分支 → 抄宿主 /etc/resolv.conf

### Q6：writeContainerEtcFiles 是 bind mount 吗？configmap 覆盖在哪里？

关键澄清：writeContainerEtcFiles 不是 bind mount。它是普通的 os.WriteFile：

```go
func writeResolvConf(etcDir string, dns sandbox.DNSConfig) error {
    return os.WriteFile(filepath.Join(etcDir, "resolv.conf"), resolvConfContent(dns), 0644)
}
```

os.WriteFile = 普通文件系统写入。它把 nameserver 10.96.0.10\n 这串字节直接写进 /var/lib/mydocker/containers/<id>/merged/etc/resolv.conf 这个真实文件。

那 bind mount 在哪？
bind mount 在它之后：

```go
// 在 bind mount 之前生成 /etc/{resolv.conf,hosts,hostname}：
// 一来 bind mount 可能用 configmap 覆盖这几个文件（典型场景：CoreDNS Corefile），
// 二来 CoreDNS 起来必须能读 resolv.conf，否则 forward 插件初始化失败。
if err := writeContainerEtcFiles(mergedRoot, sb); err != nil {        ← Step 1: 写文件
    fmt.Fprintf(os.Stderr, "warn: write /etc files for %s: %v\n", id, err)
}

// Apply bind mounts from CRI config (hostPath volumes, configMaps, secrets, etc.)
if len(cfg.Mounts) > 0 {
    for _, m := range cfg.Mounts {
        if m.HostPath == "" || m.ContainerPath == "" {
            continue
        }
        if err := bindMountIntoRootfs(mergedRoot, m.HostPath, m.ContainerPath, m.Readonly); err != nil {  ← Step 2: bind mount
            fmt.Fprintf(os.Stderr, "warn: mount %s → %s: %v\n", m.HostPath, m.ContainerPath, err)
        }
    }
}
```

两步是不同操作：

| 步骤                           | 操作                      | 谁触发                           | 数据源                                           |
| ------------------------------ | ------------------------- | -------------------------------- | ------------------------------------------------ |
| Step 1: writeContainerEtcFiles | 普通文件写 (os.WriteFile) | 我们项目主动做                   | 用 sb.DNS（Kubelet 传的）                        |
| Step 2: bindMountIntoRootfs    | mount --bind 系统调用     | Kubelet 通过 cfg.Mounts 让我们做 | 用户在 Pod spec 里配的 ConfigMap/Secret/HostPath |

"覆盖" 是怎么发生的？—— 一个具体场景
假设有这么一个 Pod spec：

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: weird-app
spec:
  containers:
    - name: app
      image: nginx
      volumeMounts:
        - name: my-resolv          ← 把 ConfigMap 挂到容器的 /etc/resolv.conf
          mountPath: /etc/resolv.conf
          subPath: resolv.conf
  volumes:
    - name: my-resolv
      configMap:
        name: my-dns-config
```

ConfigMap 内容：

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: my-dns-config
data:
  resolv.conf: |
    nameserver 1.1.1.1       ← 用户想强制用 Cloudflare DNS
```

Kubelet 处理这个 Pod 时：
把 ConfigMap 物化成宿主机文件：/var/lib/kubelet/pods/<uid>/volumes/configmap/my-resolv/resolv.conf
调我们的 CreateContainer，传过来：

```go
req.Config.Mounts = []*Mount{
    {
        HostPath:      "/var/lib/kubelet/pods/<uid>/volumes/configmap/my-resolv/resolv.conf",
        ContainerPath: "/etc/resolv.conf",
        Readonly:      true,
    },
}
```

我们的 CreateContainer 走完两步：

```text
Step 1（writeContainerEtcFiles）：
  os.WriteFile("<merged>/etc/resolv.conf", "nameserver 10.96.0.10\n...")
  ← merged/etc/resolv.conf 这时是 "nameserver 10.96.0.10..."

Step 2（bindMountIntoRootfs）：
  mount --bind \
    /var/lib/kubelet/pods/<uid>/volumes/configmap/my-resolv/resolv.conf \
    <merged>/etc/resolv.conf
  ← 现在容器看到的 /etc/resolv.conf 不再是"我们写的字节"，
    而是"那个 ConfigMap 物化文件"——内容变成 "nameserver 1.1.1.1"
```

bind mount 的本质是"在挂载点叠一层"——下面那个真实文件还在，但通过这个挂载点访问时看到的是上层 bind 的内容。容器进程读 /etc/resolv.conf 看到的是 ConfigMap 的版本。

顺序为什么不能反？
反过来的话：

```text
Step 1: bind mount 把 /etc/resolv.conf 绑到 ConfigMap 文件
Step 2: os.WriteFile("<merged>/etc/resolv.conf", "nameserver 10.96.0.10")
        ↑ 这一步会把字节写到 bind mount 的挂载点上
        ↑ 也就是写到了原来那个 ConfigMap 物化文件里！
        ↑ 用户的 ConfigMap 文件被我们污染了
```

更糟的是，ConfigMap 物化的文件可能是只读的 bind mount，写会失败；或者多个 Pod 共享这个 ConfigMap 文件，污染会扩散。

正确顺序的逻辑：

```text
先写默认值（让没配 ConfigMap 的场景也有合理 DNS）
  ↓
再 bind mount（让配了 ConfigMap 的用户能覆盖）
  ↓
```

结果：

- 没配 ConfigMap：容器看到我们写的 nameserver 10.96.0.10
- 配了 ConfigMap：容器看到 ConfigMap 内容（默认值"被遮"了，但底层文件没坏）

一个直观的图
想象 /etc/resolv.conf 是一张桌子的某个位置。

Step 1（writeContainerEtcFiles）：
我们在桌面上放了一张纸条："nameserver 10.96.0.10"

Step 2（bindMountIntoRootfs）： 仅当用户配了 ConfigMap 才执行
Kubelet 把另一张纸条"nameserver 1.1.1.1"叠在我们纸条上面

容器进程来看时：

- 没 Step 2：看到我们的"10.96.0.10"
- 有 Step 2：看到 Kubelet 叠的"1.1.1.1"

容器走后撤掉 bind mount：底下那张"10.96.0.10"还在，没被污染。

画一张更准确的图

```text
                                     ┌───────────────┐
                                     │  etcd（持久化） │ ← 真正的数据
                                     │ Service:nginx │
                                     │ Endpoints:... │
                                     └───────┬───────┘
                                             │
                                     ┌───────▼───────┐
                                     │  API Server   │ ← 数据出入口
                                     └───┬───────────┘
                          watch / LIST  │ │watch
            ┌───────────────────────────┘ └────────────────┐
            ▼                                              ▼
  ┌──────────────────┐                          ┌──────────────────┐
  │  CoreDNS Pod #1  │                          │  CoreDNS Pod #2  │
  │  内存索引（缓存） │                          │  内存索引（缓存） │
  └────────┬─────────┘                          └────────┬─────────┘
           │              ┌─────────────────┐            │
           └──────────────┤ 10.96.0.10:53   ├────────────┘
                          │ Service VIP     │
                          └────────┬────────┘
                                   │ kube-proxy 负载均衡
                                   ▼
                            业务 Pod 的 DNS 查询
```

CoreDNS Pod 挂一个：另一个还在，VIP 自动切。
两个全挂：1-5 秒后新 Pod 起来，重新从 API Server 拉一遍数据，恢复服务。

```text
                       ↑
                 永远没有"数据丢失"——数据从来不在 CoreDNS 里
```
