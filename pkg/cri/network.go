package cri

import (
	"context"
	"time"

	"github.com/mini-docker/mini-docker/pkg/network"
	"github.com/mini-docker/mini-docker/pkg/sandbox"
)

// cniHook adapts *network.Manager to sandbox.NetworkHook.
type cniHook struct {
	mgr *network.Manager
}

// Ensure cniHook implements sandbox.NetworkHook.
var _ sandbox.NetworkHook = (*cniHook)(nil)

func (h *cniHook) Setup(podID, netnsPath string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return h.mgr.Setup(ctx, podID, netnsPath, "eth0", nil)
}

func (h *cniHook) Teardown(podID, netnsPath string) error {
	// Kubelet / crictl 默认 stopp timeout 是 10s；我们留 5s 给 CNI，剩下
	// 给 SIGTERM→SIGKILL。如果 netns 已经不在（pause 早死了），libcni
	// 会返回 "no such file" 我们上层视为幂等成功。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return h.mgr.Teardown(ctx, podID, netnsPath, "eth0")
}
