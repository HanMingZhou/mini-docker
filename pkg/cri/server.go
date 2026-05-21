// Package cri 实现 Kubernetes Container Runtime Interface（CRI）。
//
// Level 3 起步：只搭 gRPC 骨架 + 最小接口集合（Version / Status / 各种 List），
// 目标是让 `crictl info` 能正常握手、Kubelet 初步认得。
//
// 后续（Level 3 完整）会分批填：PullImage、RunPodSandbox、CreateContainer、
// StartContainer、Exec、ContainerStats 等。
package cri

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/mini-docker/mini-docker/pkg/cgroup"
	"github.com/mini-docker/mini-docker/pkg/network"
	"github.com/mini-docker/mini-docker/pkg/sandbox"
	"github.com/mini-docker/mini-docker/pkg/store"
)

// Version 字符串在 Version RPC 和 Status 中共用。
const (
	RuntimeName       = "mydocker"
	RuntimeVersion    = "0.1.0"
	RuntimeAPIVersion = "v1"
	DefaultSocketPath = "/var/run/mydocker-cri.sock"
)

// Config 是 CRI server 的启动配置。
type Config struct {
	// Socket 是 gRPC 监听的 Unix Socket 路径。
	Socket string

	// Root 是 mydocker 数据目录，默认 store.Root()。
	Root string

	// MydockerBinary 是 `mydocker` 可执行文件路径，供 sandbox 启动 pause 使用。
	// 留空则从 PATH 查找；找不到会回退到 /proc/self/exe（即 mydocker-cri 自己，
	// 只要它是同一个二进制或者能响应 `pause` 子命令）。
	MydockerBinary string

	// StreamingAddr 是 HTTP streaming server 监听地址，空则不启用。
	StreamingAddr string

	// StreamingBaseURL 用于 Exec/Attach 返回的 URL；空则用 StreamingAddr。
	StreamingBaseURL string

	// CNIConfDir 是 CNI 配置目录，默认 /etc/cni/net.d。
	CNIConfDir string

	// CNIBinDirs 是 CNI 插件二进制搜索路径，默认 /opt/cni/bin。
	CNIBinDirs []string

	// CgroupDriver 是 "systemd" 或 "cgroupfs"，默认跟 Kubelet 一致用 systemd。
	// 必须和 Kubelet 的 cgroupDriver 完全匹配，否则 Pod 启动后资源限制失效。
	CgroupDriver cgroup.Driver
}

// Server 是 mydocker-cri 的 gRPC 服务器。
type Server struct {
	cfg       Config
	grpc      *grpc.Server
	runtime   *RuntimeService
	image     *ImageService
	listener  net.Listener
	streaming *wrappedStreamingServer
	cni       *network.Manager
}

// New 创建一个尚未启动的 CRI Server。
func New(cfg Config) (*Server, error) {
	if cfg.Socket == "" {
		cfg.Socket = DefaultSocketPath
	}
	if cfg.Root == "" {
		cfg.Root = store.Root()
	}
	if cfg.MydockerBinary == "" {
		if p, err := exec.LookPath("mydocker"); err == nil {
			cfg.MydockerBinary = p
		}
	}

	sbMgr, err := sandbox.NewManager(cfg.Root, cfg.MydockerBinary)
	if err != nil {
		return nil, fmt.Errorf("init sandbox manager: %w", err)
	}

	// 初始化 CNI manager（无配置时为 disabled，不致命）
	cniMgr, err := network.NewManager(cfg.CNIConfDir, filepath.Join(cfg.Root, "cni-cache"), cfg.CNIBinDirs)
	if err != nil {
		return nil, fmt.Errorf("init cni manager: %w", err)
	}
	if cniMgr.Ready() {
		sbMgr.SetNetworkHook(&cniHook{mgr: cniMgr})
	}

	rt := newRuntimeService(sbMgr)
	rt.cni = cniMgr // 让 Status 能拿到 NetworkReady
	rt.mydockerBinary = cfg.MydockerBinary
	if cfg.CgroupDriver == "" {
		cfg.CgroupDriver = cgroup.DriverSystemd // Kubelet 默认值
	}
	rt.cgroupDriver = cfg.CgroupDriver

	// gRPC server 带日志拦截器
	grpcSrv := grpc.NewServer(grpc.UnaryInterceptor(loggingInterceptor))

	return &Server{
		cfg:     cfg,
		grpc:    grpcSrv,
		runtime: rt,
		image:   newImageService(cfg.Root),
		cni:     cniMgr,
	}, nil
}

// loggingInterceptor 打印每个 RPC 的方法名、耗时和错误到 stderr（进 journald）。
// 过滤掉高频无用的 List/Status 调用以减少噪音。
func loggingInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	start := time.Now()
	resp, err := handler(ctx, req)
	elapsed := time.Since(start)

	// 过滤高频 RPC，只在出错时打印
	method := info.FullMethod
	quiet := strings.HasSuffix(method, "/Status") ||
		strings.HasSuffix(method, "/ListPodSandbox") ||
		strings.HasSuffix(method, "/ListContainers") ||
		strings.HasSuffix(method, "/ListContainerStats") ||
		strings.HasSuffix(method, "/ListPodSandboxStats") ||
		strings.HasSuffix(method, "/ListImages")
	if quiet && err == nil {
		return resp, err
	}

	if err != nil {
		log.Printf("[CRI] %s ERR %v (%s)", method, err, elapsed)
	} else {
		log.Printf("[CRI] %s OK (%s)", method, elapsed)
	}
	return resp, err
}

// startReaper — 禁用全局 reaper。
// 容器进程由 StartContainer 的 Wait goroutine 管理。
// hostNetwork sandbox 用 PID 1 不需要 reap。
// 非 hostNetwork sandbox 的 pause 进程退出后会变 zombie，但数量极少且不影响功能。
func startReaper() {
	// 不做任何事。zombie 由 cmd.Wait() goroutine 或 sandbox.Stop 处理。
}

// Start 开始监听并阻塞到停止。调用方可通过 Stop() 优雅关闭。
func (s *Server) Start() error {
	// 启动僵尸进程收割器
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
	// Kubelet 是 root，但我们仍设置 0660 便于本机调试（crictl 通常用 root）
	if err := os.Chmod(s.cfg.Socket, 0660); err != nil {
		_ = lis.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}
	s.listener = lis

	// 可选：启动 HTTP streaming server（Exec/Attach 依赖它）
	if s.cfg.StreamingAddr != "" {
		ss, err := NewStreamingServer(s.cfg.Root, StreamingConfig{
			Addr:           s.cfg.StreamingAddr,
			BaseURL:        s.cfg.StreamingBaseURL,
			MydockerBinary: s.cfg.MydockerBinary,
		})
		if err != nil {
			_ = lis.Close()
			return fmt.Errorf("init streaming server: %w", err)
		}
		s.streaming = ss
		s.runtime.SetStreamingServer(ss)

		go func() {
			if err := ss.Start(); err != nil {
				fmt.Fprintf(os.Stderr, "streaming server stopped: %v\n", err)
			}
		}()
	}

	runtime.RegisterRuntimeServiceServer(s.grpc, s.runtime)
	runtime.RegisterImageServiceServer(s.grpc, s.image)

	if err := s.grpc.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}

// Stop 优雅关闭，停止接受新请求并等待现有请求完成。
func (s *Server) Stop() {
	if s.streaming != nil {
		_ = s.streaming.Stop()
	}
	if s.grpc != nil {
		s.grpc.GracefulStop()
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
	_ = os.Remove(s.cfg.Socket)
}

// --- 以下是两个 service 的最简实现 ------------------------------------------

// RuntimeService 是 CRI RuntimeService 的实现。
type RuntimeService struct {
	runtime.UnimplementedRuntimeServiceServer

	sandboxMgr     *sandbox.Manager
	streaming      StreamingServer
	cni            *network.Manager
	cgroupDriver   cgroup.Driver
	mydockerBinary string
}

// StreamingServer 是 HTTP streaming server 的接口抽象。
// 真正的实现见 pkg/streaming（Linux 专属），这里保留一个接口避免耦合。
type StreamingServer interface {
	GetExec(req *runtime.ExecRequest) (*runtime.ExecResponse, error)
	GetAttach(req *runtime.AttachRequest) (*runtime.AttachResponse, error)
	GetPortForward(req *runtime.PortForwardRequest) (*runtime.PortForwardResponse, error)
}

func newRuntimeService(sbMgr *sandbox.Manager) *RuntimeService {
	return &RuntimeService{sandboxMgr: sbMgr}
}

// SetStreamingServer attaches a streaming server for Exec/Attach/PortForward.
func (s *RuntimeService) SetStreamingServer(ss StreamingServer) {
	s.streaming = ss
}

// Version 是 Kubelet 握手时的第一个 RPC。
func (s *RuntimeService) Version(_ context.Context, _ *runtime.VersionRequest) (*runtime.VersionResponse, error) {
	return &runtime.VersionResponse{
		Version:           RuntimeVersion,
		RuntimeName:       RuntimeName,
		RuntimeVersion:    RuntimeVersion,
		RuntimeApiVersion: RuntimeAPIVersion,
	}, nil
}

// Status 返回运行时健康。Kubelet 会定期调用它做心跳。
// RuntimeReady 恒为 true（daemon 能响应 RPC 就说明 Runtime 就绪）；
// NetworkReady 取决于 CNI manager 是否加载了网络配置。
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

// ListPodSandbox / ListContainers 在还没接沙箱/容器管理时先返回空列表，
// 避免 Kubelet / crictl 报错。
// ListPodSandbox 的真正实现在 sandbox.go 中（覆盖这里的默认）。
// ListContainers 的真正实现在 container.go 中。

// ImageService 是 CRI ImageService 的实现。
// 真正的 RPC 方法在 image.go 中实现，这里只保留类型与构造器。
type ImageService struct {
	runtime.UnimplementedImageServiceServer

	root string // 数据根目录
}

func newImageService(root string) *ImageService {
	return &ImageService{root: root}
}
