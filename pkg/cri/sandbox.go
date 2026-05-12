package cri

import (
	"context"
	"fmt"
	"net"
	"os"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/mini-docker/mini-docker/pkg/sandbox"
)

// RunPodSandbox 创建并启动一个 Pod 沙箱。
func (s *RuntimeService) RunPodSandbox(_ context.Context, req *runtime.RunPodSandboxRequest) (*runtime.RunPodSandboxResponse, error) {
	if req.Config == nil || req.Config.Metadata == nil {
		return nil, fmt.Errorf("RunPodSandbox: missing config/metadata")
	}
	md := req.Config.Metadata

	var cgroupParent string
	var hostNetwork bool
	if req.Config.Linux != nil {
		cgroupParent = req.Config.Linux.CgroupParent
		if req.Config.Linux.SecurityContext != nil &&
			req.Config.Linux.SecurityContext.NamespaceOptions != nil {
			hostNetwork = req.Config.Linux.SecurityContext.NamespaceOptions.Network == runtime.NamespaceMode_NODE
		}
	}

	sb, err := s.sandboxMgr.StartWithOptions(
		sandbox.Metadata{
			Name:      md.Name,
			Namespace: md.Namespace,
			UID:       md.Uid,
			Attempt:   md.Attempt,
		},
		sandbox.StartOptions{
			LogDir:       req.Config.LogDirectory,
			Labels:       req.Config.Labels,
			Annotations:  req.Config.Annotations,
			CgroupParent: cgroupParent,
			HostNetwork:  hostNetwork,
		},
	)
	if err != nil {
		return nil, err
	}
	return &runtime.RunPodSandboxResponse{PodSandboxId: sb.ID}, nil
}

// StopPodSandbox 停止沙箱但保留记录（语义对齐 CRI）。
// 幂等：沙箱不存在时返回成功。
// hostNetwork sandbox（PID=1）不做任何事（PID 1 不能被停止）。
func (s *RuntimeService) StopPodSandbox(_ context.Context, req *runtime.StopPodSandboxRequest) (*runtime.StopPodSandboxResponse, error) {
	if req.PodSandboxId == "" {
		return nil, fmt.Errorf("StopPodSandbox: empty id")
	}
	sb, err := s.sandboxMgr.Get(req.PodSandboxId)
	if err != nil {
		// 幂等：sandbox 不存在不算错
		return &runtime.StopPodSandboxResponse{}, nil
	}
	// hostNetwork sandbox (PID=1) 不停止 — 它永远 Ready
	if sb.PID == 1 {
		return &runtime.StopPodSandboxResponse{}, nil
	}
	if err := s.sandboxMgr.Stop(req.PodSandboxId); err != nil {
		if os.IsNotExist(err) {
			return &runtime.StopPodSandboxResponse{}, nil
		}
		return nil, err
	}
	return &runtime.StopPodSandboxResponse{}, nil
}

// RemovePodSandbox 删除已停止的沙箱记录。
// 幂等：沙箱不存在时返回成功。
// hostNetwork sandbox（PID=1）保留 config.json 以便后续 Status 查询。
func (s *RuntimeService) RemovePodSandbox(_ context.Context, req *runtime.RemovePodSandboxRequest) (*runtime.RemovePodSandboxResponse, error) {
	if req.PodSandboxId == "" {
		return nil, fmt.Errorf("RemovePodSandbox: empty id")
	}
	sb, err := s.sandboxMgr.Get(req.PodSandboxId)
	if err != nil {
		// 幂等：不存在不视为错误
		return &runtime.RemovePodSandboxResponse{}, nil
	}
	// 仍在 Ready 时先停
	if sb.State == sandbox.StateReady {
		_ = s.sandboxMgr.Stop(sb.ID) // best-effort
	}
	// hostNetwork sandbox (PID=1) 不删目录，保留 metadata 供 Status 查询
	if sb.PID != 1 {
		_ = s.sandboxMgr.Remove(sb.ID) // best-effort
	}
	return &runtime.RemovePodSandboxResponse{}, nil
}

// PodSandboxStatus 查询单个沙箱状态。
// 如果沙箱不存在，返回 gRPC NotFound 错误。
func (s *RuntimeService) PodSandboxStatus(_ context.Context, req *runtime.PodSandboxStatusRequest) (*runtime.PodSandboxStatusResponse, error) {
	if req.PodSandboxId == "" {
		return nil, fmt.Errorf("PodSandboxStatus: empty id")
	}
	sb, err := s.sandboxMgr.Get(req.PodSandboxId)
	if err != nil {
		// sandbox 不存在，返回错误让 kubelet 知道
		return nil, fmt.Errorf("PodSandboxStatus: %w", err)
	}
	return &runtime.PodSandboxStatusResponse{Status: sandboxToStatus(sb)}, nil
}

// ListPodSandbox 覆盖 server.go 里的默认空实现。
func (s *RuntimeService) ListPodSandbox(_ context.Context, req *runtime.ListPodSandboxRequest) (*runtime.ListPodSandboxResponse, error) {
	list, err := s.sandboxMgr.List()
	if err != nil {
		return nil, err
	}
	out := make([]*runtime.PodSandbox, 0, len(list))
	for _, sb := range list {
		if !matchSandboxFilter(sb, req.Filter) {
			continue
		}
		out = append(out, &runtime.PodSandbox{
			Id:          sb.ID,
			Metadata:    sandboxMetaToProto(sb.Metadata),
			State:       sandboxStateToProto(sb.State),
			CreatedAt:   sb.CreatedAt.UnixNano(),
			Labels:      sb.Labels,
			Annotations: sb.Annotations,
		})
	}
	return &runtime.ListPodSandboxResponse{Items: out}, nil
}

// --- helpers -----------------------------------------------------------------

func sandboxStateToProto(s sandbox.State) runtime.PodSandboxState {
	if s == sandbox.StateReady {
		return runtime.PodSandboxState_SANDBOX_READY
	}
	return runtime.PodSandboxState_SANDBOX_NOTREADY
}

func sandboxMetaToProto(m sandbox.Metadata) *runtime.PodSandboxMetadata {
	return &runtime.PodSandboxMetadata{
		Name:      m.Name,
		Namespace: m.Namespace,
		Uid:       m.UID,
		Attempt:   m.Attempt,
	}
}

func sandboxToStatus(sb *sandbox.Sandbox) *runtime.PodSandboxStatus {
	// 判断是否为 hostNetwork sandbox（PID=1 表示用的是宿主机 netns）
	hostNetwork := sb.PID == 1

	// hostNetwork pod 返回宿主机 IP（Kubelet 用它来设置 Pod status）
	ip := sb.IP
	if ip == "" {
		ip = getHostIP()
	}

	// Namespace 选项必须准确反映实际情况
	// hostNetwork=true 时 Network=NODE，否则 Network=POD
	netMode := runtime.NamespaceMode_POD
	if hostNetwork {
		netMode = runtime.NamespaceMode_NODE
	}

	return &runtime.PodSandboxStatus{
		Id:        sb.ID,
		Metadata:  sandboxMetaToProto(sb.Metadata),
		State:     sandboxStateToProto(sb.State),
		CreatedAt: sb.CreatedAt.UnixNano(),
		Network:   &runtime.PodSandboxNetworkStatus{Ip: ip},
		Linux: &runtime.LinuxPodSandboxStatus{
			Namespaces: &runtime.Namespace{
				Options: &runtime.NamespaceOption{
					Network: netMode,
					Pid:     runtime.NamespaceMode_CONTAINER,
					Ipc:     runtime.NamespaceMode_POD,
				},
			},
		},
		Labels:      sb.Labels,
		Annotations: sb.Annotations,
	}
}

// getHostIP 返回宿主机的第一个非 loopback IPv4 地址。
func getHostIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return ""
}

// matchSandboxFilter 实现 CRI 的 PodSandboxFilter 语义：
//   - Id: 精确匹配
//   - State: SANDBOX_READY / SANDBOX_NOTREADY 任一
//   - LabelSelector: 所有键值必须命中
func matchSandboxFilter(sb *sandbox.Sandbox, f *runtime.PodSandboxFilter) bool {
	if f == nil {
		return true
	}
	if f.Id != "" && f.Id != sb.ID {
		return false
	}
	if f.State != nil {
		want := f.State.State
		got := sandboxStateToProto(sb.State)
		if want != got {
			return false
		}
	}
	for k, v := range f.LabelSelector {
		if sb.Labels[k] != v {
			return false
		}
	}
	return true
}
