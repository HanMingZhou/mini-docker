package cri

import (
	"context"
	"testing"
	"time"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/mini-docker/mini-docker/pkg/sandbox"
)

// testRuntimeWithSandboxes 构造一个 RuntimeService 并预置几个 sandbox 记录。
func testRuntimeWithSandboxes(t *testing.T) *RuntimeService {
	t.Helper()
	root := t.TempDir()
	mgr, err := sandbox.NewManager(root, "")
	if err != nil {
		t.Fatal(err)
	}
	// 直接写落盘数据，绕过 Start（它会 fork pause，仅 Linux 可用）
	now := time.Now().UTC()
	preset := []*sandbox.Sandbox{
		{
			ID:        "aaaaaaaaaaaa",
			Metadata:  sandbox.Metadata{Name: "alpha", Namespace: "default", UID: "u1"},
			State:     sandbox.StateNotReady, // 标记 NotReady 跳过 pidAlive 检查
			CreatedAt: now,
			Labels:    map[string]string{"app": "web"},
		},
		{
			ID:        "bbbbbbbbbbbb",
			Metadata:  sandbox.Metadata{Name: "bravo", Namespace: "default", UID: "u2"},
			State:     sandbox.StateNotReady,
			CreatedAt: now.Add(time.Second),
			Labels:    map[string]string{"app": "db"},
		},
	}
	for _, sb := range preset {
		if err := writeSandboxForTest(mgr, sb); err != nil {
			t.Fatal(err)
		}
	}
	return newRuntimeService(mgr)
}

// writeSandboxForTest 把 sandbox 直接落盘（通过 sandbox 包提供的测试跳板）。
func writeSandboxForTest(m *sandbox.Manager, sb *sandbox.Sandbox) error {
	return sandbox.SaveForTest(m, sb)
}

func TestListPodSandboxAndStatus(t *testing.T) {
	rt := testRuntimeWithSandboxes(t)

	// ListPodSandbox
	resp, err := rt.ListPodSandbox(context.Background(), &runtime.ListPodSandboxRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("want 2 items, got %d", len(resp.Items))
	}

	// 按 Label 过滤
	resp2, err := rt.ListPodSandbox(context.Background(), &runtime.ListPodSandboxRequest{
		Filter: &runtime.PodSandboxFilter{LabelSelector: map[string]string{"app": "db"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp2.Items) != 1 || resp2.Items[0].Metadata.Name != "bravo" {
		t.Fatalf("label filter failed: %+v", resp2.Items)
	}

	// PodSandboxStatus
	st, err := rt.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{
		PodSandboxId: "aaaaaaaaaaaa",
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status.State != runtime.PodSandboxState_SANDBOX_NOTREADY {
		t.Fatalf("state = %v", st.Status.State)
	}
	if st.Status.Metadata.Name != "alpha" {
		t.Fatalf("name = %s", st.Status.Metadata.Name)
	}
}
