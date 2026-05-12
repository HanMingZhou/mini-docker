package cri

import (
	"context"
	"time"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/mini-docker/mini-docker/pkg/store"
)

// UpdateRuntimeConfig 接受 Kubelet 传入的运行时配置（通常只有 PodCIDR）。
// 我们当前不实际使用 PodCIDR（CNI 插件自己用 IPAM 分），但必须返回成功
// 否则 Kubelet 启动流程会卡住。
func (s *RuntimeService) UpdateRuntimeConfig(_ context.Context, _ *runtime.UpdateRuntimeConfigRequest) (*runtime.UpdateRuntimeConfigResponse, error) {
	return &runtime.UpdateRuntimeConfigResponse{}, nil
}

// UpdateContainerResources 动态改 cgroup 限制。
// Level 3 简化：记录成功，但不实际改限制（CRI 的主要消费者是 VPA，第一阶段不上）。
func (s *RuntimeService) UpdateContainerResources(_ context.Context, _ *runtime.UpdateContainerResourcesRequest) (*runtime.UpdateContainerResourcesResponse, error) {
	return &runtime.UpdateContainerResourcesResponse{}, nil
}

// ReopenContainerLog 由 log rotator 触发。
// 我们的日志不做 rotation，返回成功即可。
func (s *RuntimeService) ReopenContainerLog(_ context.Context, _ *runtime.ReopenContainerLogRequest) (*runtime.ReopenContainerLogResponse, error) {
	return &runtime.ReopenContainerLogResponse{}, nil
}

// ContainerStats 返回容器资源使用统计。
//
// 最小实现：返回容器身份信息 + 零值指标。kubectl top 会显示 0 值但不会报错；
// HPA 不会触发扩缩容。真实实现需要从 cgroup v2 的 memory.stat / cpu.stat 读取。
func (s *RuntimeService) ContainerStats(_ context.Context, req *runtime.ContainerStatsRequest) (*runtime.ContainerStatsResponse, error) {
	st, err := store.New(s.root())
	if err != nil {
		return nil, err
	}
	rec, err := st.Resolve(req.ContainerId)
	if err != nil {
		return nil, err
	}
	return &runtime.ContainerStatsResponse{
		Stats: emptyContainerStats(rec),
	}, nil
}

// ListContainerStats 返回所有容器的 stats。
func (s *RuntimeService) ListContainerStats(_ context.Context, req *runtime.ListContainerStatsRequest) (*runtime.ListContainerStatsResponse, error) {
	st, err := store.New(s.root())
	if err != nil {
		return nil, err
	}
	all, err := st.List()
	if err != nil {
		return nil, err
	}
	out := make([]*runtime.ContainerStats, 0, len(all))
	for _, rec := range all {
		if req.Filter != nil {
			if req.Filter.Id != "" && req.Filter.Id != rec.ID {
				continue
			}
		}
		out = append(out, emptyContainerStats(rec))
	}
	return &runtime.ListContainerStatsResponse{Stats: out}, nil
}

// PodSandboxStats 返回沙箱级统计。最小实现。
func (s *RuntimeService) PodSandboxStats(_ context.Context, req *runtime.PodSandboxStatsRequest) (*runtime.PodSandboxStatsResponse, error) {
	sb, err := s.sandboxMgr.Get(req.PodSandboxId)
	if err != nil {
		// 沙箱不存在时返回空统计
		return &runtime.PodSandboxStatsResponse{
			Stats: &runtime.PodSandboxStats{
				Attributes: &runtime.PodSandboxAttributes{
					Id:       req.PodSandboxId,
					Metadata: &runtime.PodSandboxMetadata{},
				},
				Linux: &runtime.LinuxPodSandboxStats{},
			},
		}, nil
	}
	return &runtime.PodSandboxStatsResponse{
		Stats: &runtime.PodSandboxStats{
			Attributes: &runtime.PodSandboxAttributes{
				Id:          sb.ID,
				Metadata:    sandboxMetaToProto(sb.Metadata),
				Labels:      sb.Labels,
				Annotations: sb.Annotations,
			},
			Linux: &runtime.LinuxPodSandboxStats{},
		},
	}, nil
}

// ListPodSandboxStats 列出所有沙箱的统计。
func (s *RuntimeService) ListPodSandboxStats(_ context.Context, req *runtime.ListPodSandboxStatsRequest) (*runtime.ListPodSandboxStatsResponse, error) {
	list, err := s.sandboxMgr.List()
	if err != nil {
		return nil, err
	}
	out := make([]*runtime.PodSandboxStats, 0, len(list))
	for _, sb := range list {
		if req.Filter != nil && req.Filter.Id != "" && req.Filter.Id != sb.ID {
			continue
		}
		out = append(out, &runtime.PodSandboxStats{
			Attributes: &runtime.PodSandboxAttributes{
				Id:          sb.ID,
				Metadata:    sandboxMetaToProto(sb.Metadata),
				Labels:      sb.Labels,
				Annotations: sb.Annotations,
			},
			Linux: &runtime.LinuxPodSandboxStats{},
		})
	}
	return &runtime.ListPodSandboxStatsResponse{Stats: out}, nil
}

// emptyContainerStats returns a stats message with identity but zero metrics.
func emptyContainerStats(rec *store.Container) *runtime.ContainerStats {
	now := time.Now().UnixNano()
	return &runtime.ContainerStats{
		Attributes: &runtime.ContainerAttributes{
			Id:       rec.ID,
			Metadata: &runtime.ContainerMetadata{Name: rec.Name},
		},
		Cpu: &runtime.CpuUsage{
			Timestamp:            now,
			UsageCoreNanoSeconds: &runtime.UInt64Value{Value: 0},
		},
		Memory: &runtime.MemoryUsage{
			Timestamp:       now,
			WorkingSetBytes: &runtime.UInt64Value{Value: 0},
		},
		WritableLayer: &runtime.FilesystemUsage{
			Timestamp: now,
			FsId:      &runtime.FilesystemIdentifier{Mountpoint: rec.Rootfs},
			UsedBytes: &runtime.UInt64Value{Value: 0},
		},
	}
}
