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
    
### 三、一句话总结
UnimplementedXxxServer 是 protoc 自动生成的"打底空壳"，通过 Go 的结构体嵌入让你的实现默认满足整个 interface；你只需要"挑你关心的 RPC 重写一下"，未来 cri-api 加新方法也不会破坏你的代码——这就是 gRPC Go 的前向兼容约定。

题外话：这个模式在 gRPC Go 里是强烈推荐做法，新版本的 protoc-gen-go-grpc 甚至默认要求你必须嵌入它，否则编译会拒绝（require_unimplemented_servers=true）



### 四、Kubelet 是 client，mydocker-cri 是 server
Kubelet 来找你，不是你去找 Kubelet

理解了这个方向，剩下的疑问就全部消解了。下面把整条链路拆开讲。
#### （一）CRI 是个 RPC 协议，谁是 server，谁是 client？
┌─────────────┐    gRPC over Unix Socket    ┌──────────────────┐
│   Kubelet   │ ──────── client ──────────► │   mydocker-cri   │
│  (k8s 进程) │ ◄─────── response ──────── │   (你的进程)     │
└─────────────┘                             └──────────────────┘
                  /var/run/mydocker-cri.sock
                  (server 监听这里)
server：mydocker-cri 进程，监听 Unix socket，被动等连接
client：Kubelet（也包括 crictl 这个调试工具），主动连过来发请求
所以 mydocker-cri 根本不需要"知道 Kubelet 在哪里"——它只需要在一个约定好的 Unix socket 路径上监听，等别人来敲门。

#### （二—）RegisterRuntimeServiceServer 到底做了什么
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
Register*ServiceServer 是 protoc 自动生成的纯本地函数，它做的事相当于这样（伪代码）：


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

#### (三)、那 socket 是怎么对上的
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
net.Listen("unix", s.cfg.Socket) —— 在文件系统上创建 Unix socket 文件并 listen。s.cfg.Socket 默认是 /var/run/my-cri.sock（见 @c:/project/mini-docker/pkg/cri/server.go:36），通过 CLI flag --socket 可改。
Register* —— 往 s.grpc 的路由表写表项。
s.grpc.Serve(lis) —— 让 gRPC server 在这个 listener 上跑起来：accept 连接、解析 HTTP/2 帧、按方法名路由到注册的 handler、阻塞返回。
所以"两个服务在哪个端口"的答案是：

没有端口，它们共用一个 Unix domain socket 文件：/var/run/my-cri.sock（或你用 --socket 配的路径）。
Unix socket 不是 TCP，没有端口号，就是文件系统里的一个特殊文件。两端通过文件路径"碰头"。

#### (四)、Kubelet 怎么找到这个 socket
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

#### (五)、完整的"启动 + 第一个 RPC"时序
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
#### (六)、回到你的原问题
"RegisterRuntimeServiceServer(s.grpc, s.runtime) 是怎么知道 kubelet 的 runtime、image 这两个服务的端口呢？"

校正之后的答案：
Register 只是把方法注册到本地路由表
两个 service 共用一个 Unix socket 文件，没有端口
Kubelet 主动找 mydocker-cri（Kubelet 是 client）
Kubelet 通过启动参数 --container-runtime-endpoint 知道 socket 路径
两端"对上"靠的是共同约定的 Unix socket 路径，不是端口、不是注册中心、不是广播——就是文件系统里那个 .sock 文件。

#### (七)、一些可能让你更踏实的实验
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
第 4 步特别有意思——这两个 service 名字 runtime.v1.RuntimeService 和 runtime.v1.ImageService 就是 Register*ServiceServer 写进路由表的 key，是 proto 文件里 package runtime.v1; service RuntimeService { ... } 自动生成的。任何 client（Kubelet、crictl、grpcurl）只要能访问这个 socket、知道这个名字，就能调。



## 三、RuntimeService 的 RPC 大类
类别	    典型 RPC	        本项目位置
握手/心跳	Version、Status	        server.go:261-286
Pod 沙箱	RunPodSandbox / StopPodSandbox / RemovePodSandbox / PodSandboxStatus / ListPodSandbox   sandbox.go
容器	CreateContainer / StartContainer / StopContainer / RemoveContainer / ContainerStatus / ListContainers	container.go
执行	ExecSync / Exec / Attach / PortForward	exec.go
统计与配置	ContainerStats / ListContainerStats / PodSandboxStats / UpdateRuntimeConfig / UpdateContainerResources / ReopenContainerLog     stats.go

## 关键概念：什么是 Pod 沙箱
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

记忆口诀
RuntimeService = "动的东西"：Pod、容器、进程、Exec、Stats
ImageService = "静的东西"：镜像文件、磁盘占用
两者通过 CRI 协议统一接口，通过 共享数据目录解耦实现



## 六、sock 是什么
.sock 文件是 Unix Domain Socket（Unix 域套接字），俗称 "Unix socket"。
它是操作系统提供的一种进程间通信（IPC）机制，跟 TCP/UDP 是同一级别的概念，但用法和性质完全不同。

### 1. 直观对比 TCP socket
维度	    TCP socket	    Unix domain socket
寻址方式	IP + 端口 (127.0.0.1:10250)	              文件系统路径 (/var/run/foo.sock)
协议栈	    走 TCP/IP，要经过网卡驱动、IP 层、TCP 层	直接在内核内存里拷贝，不走网络栈
跨主机	    可以	                                  不可以，只能同一台机器
性能	    较慢	                                  快几倍（无 checksum、无路由查找）
权限控制	靠防火墙	                               靠文件系统权限（chmod / chown）
在 ss/netstat 里	TCP 列表	                      Unix 列表（ss -lx）
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
net.Listen("unix", "/var/run/my-cri.sock") 内部最终调用了 Linux 的 socket(AF_UNIX, SOCK_STREAM, 0) + bind() 系统调用，让内核：
创建一个 socket 内核对象
在文件系统的 /var/run/my-cri.sock 路径上落一个 socket 类型的 inode，关联到这个内核对象
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
grpc.Dial("unix:///var/run/my-cri.sock", ...)  // 注意 unix:// 前缀
unix:// 这个 scheme 告诉 gRPC："不要解析成主机名，把后面的路径当 socket 文件路径用"。
```
.sock = 文件系统里的一个"socket 类型节点"，是同主机内进程间通信的入口，用文件路径代替 IP+端口寻址，安全（靠文件权限）、快速（不过网络栈）、本地专用。



