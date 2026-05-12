package cri

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/mini-docker/mini-docker/pkg/sandbox"
	"github.com/mini-docker/mini-docker/pkg/store"
)

func TestContainerMetaPersistence(t *testing.T) {
	root := t.TempDir()
	mgr, err := sandbox.NewManager(root, "")
	if err != nil {
		t.Fatal(err)
	}
	svc := newRuntimeService(mgr)

	id := "test-container-001"
	containerDir := filepath.Join(root, "containers", id)
	if err := os.MkdirAll(containerDir, 0755); err != nil {
		t.Fatal(err)
	}

	meta := &ContainerMeta{
		SandboxID: "sandbox-abc",
		ImageRef:  "nginx:alpine",
		State:     "CREATED",
		CreatedAt: time.Now().UnixNano(),
		Labels:    map[string]string{"app": "web"},
	}
	if err := svc.saveContainerMeta(id, meta); err != nil {
		t.Fatal(err)
	}

	loaded, err := svc.loadContainerMeta(id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SandboxID != "sandbox-abc" {
		t.Errorf("SandboxID = %q, want sandbox-abc", loaded.SandboxID)
	}
	if loaded.ImageRef != "nginx:alpine" {
		t.Errorf("ImageRef = %q, want nginx:alpine", loaded.ImageRef)
	}
	if loaded.Labels["app"] != "web" {
		t.Errorf("Labels[app] = %q, want web", loaded.Labels["app"])
	}
}

func TestContainerStateToProto(t *testing.T) {
	tests := []struct {
		state store.State
		want  runtime.ContainerState
	}{
		{store.StateCreated, runtime.ContainerState_CONTAINER_CREATED},
		{store.StateRunning, runtime.ContainerState_CONTAINER_RUNNING},
		{store.StateExited, runtime.ContainerState_CONTAINER_EXITED},
		{"Unknown", runtime.ContainerState_CONTAINER_UNKNOWN},
	}
	for _, tt := range tests {
		got := containerStateToProto(tt.state)
		if got != tt.want {
			t.Errorf("containerStateToProto(%q) = %v, want %v", tt.state, got, tt.want)
		}
	}
}

func TestMatchContainerFilter(t *testing.T) {
	c := &runtime.Container{
		Id:           "abc123",
		PodSandboxId: "sb-001",
		State:        runtime.ContainerState_CONTAINER_RUNNING,
		Labels:       map[string]string{"app": "web", "env": "prod"},
	}

	tests := []struct {
		name   string
		filter *runtime.ContainerFilter
		want   bool
	}{
		{"nil filter", nil, true},
		{"match id", &runtime.ContainerFilter{Id: "abc123"}, true},
		{"mismatch id", &runtime.ContainerFilter{Id: "xyz"}, false},
		{"match sandbox", &runtime.ContainerFilter{PodSandboxId: "sb-001"}, true},
		{"mismatch sandbox", &runtime.ContainerFilter{PodSandboxId: "sb-999"}, false},
		{"match state", &runtime.ContainerFilter{State: &runtime.ContainerStateValue{State: runtime.ContainerState_CONTAINER_RUNNING}}, true},
		{"mismatch state", &runtime.ContainerFilter{State: &runtime.ContainerStateValue{State: runtime.ContainerState_CONTAINER_EXITED}}, false},
		{"match labels", &runtime.ContainerFilter{LabelSelector: map[string]string{"app": "web"}}, true},
		{"mismatch labels", &runtime.ContainerFilter{LabelSelector: map[string]string{"app": "db"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchContainerFilter(c, tt.filter)
			if got != tt.want {
				t.Errorf("matchContainerFilter = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCreateContainerValidation(t *testing.T) {
	root := t.TempDir()
	mgr, err := sandbox.NewManager(root, "")
	if err != nil {
		t.Fatal(err)
	}
	svc := newRuntimeService(mgr)

	ctx := context.Background()

	// Missing sandbox ID
	_, err = svc.CreateContainer(ctx, &runtime.CreateContainerRequest{})
	if err == nil {
		t.Fatal("expected error for empty sandbox id")
	}

	// Missing config
	_, err = svc.CreateContainer(ctx, &runtime.CreateContainerRequest{
		PodSandboxId: "nonexistent",
	})
	if err == nil {
		t.Fatal("expected error for missing config")
	}

	// Non-existent sandbox
	_, err = svc.CreateContainer(ctx, &runtime.CreateContainerRequest{
		PodSandboxId: "nonexistent",
		Config: &runtime.ContainerConfig{
			Image: &runtime.ImageSpec{Image: "nginx"},
		},
	})
	if err == nil {
		t.Fatal("expected error for non-existent sandbox")
	}
}

func TestListContainersWithStore(t *testing.T) {
	root := t.TempDir()
	mgr, err := sandbox.NewManager(root, "")
	if err != nil {
		t.Fatal(err)
	}
	svc := newRuntimeService(mgr)

	// Pre-populate store with some containers
	st, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	containers := []*store.Container{
		{ID: "ctr-001", Name: "web", State: store.StateRunning, CreatedAt: now, PID: 0},
		{ID: "ctr-002", Name: "db", State: store.StateExited, CreatedAt: now},
	}
	for _, c := range containers {
		cDir := filepath.Join(root, "containers", c.ID)
		_ = os.MkdirAll(cDir, 0755)
		if err := st.Save(c); err != nil {
			t.Fatal(err)
		}
		// Save CRI meta
		meta := &ContainerMeta{
			SandboxID: "sb-test",
			ImageRef:  "nginx:latest",
			State:     string(c.State),
			CreatedAt: now.UnixNano(),
			Labels:    map[string]string{"name": c.Name},
		}
		_ = svc.saveContainerMeta(c.ID, meta)
	}

	ctx := context.Background()

	// List all
	resp, err := svc.ListContainers(ctx, &runtime.ListContainersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Containers) != 2 {
		t.Fatalf("want 2 containers, got %d", len(resp.Containers))
	}

	// Filter by sandbox
	resp2, err := svc.ListContainers(ctx, &runtime.ListContainersRequest{
		Filter: &runtime.ContainerFilter{PodSandboxId: "sb-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp2.Containers) != 2 {
		t.Fatalf("want 2 containers for sb-test, got %d", len(resp2.Containers))
	}

	// Filter by state
	resp3, err := svc.ListContainers(ctx, &runtime.ListContainersRequest{
		Filter: &runtime.ContainerFilter{
			State: &runtime.ContainerStateValue{State: runtime.ContainerState_CONTAINER_EXITED},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// ctr-001 has PID=0 so pidAlive returns false, it gets marked Exited too
	// Both should be EXITED now
	if len(resp3.Containers) != 2 {
		t.Fatalf("want 2 exited containers, got %d", len(resp3.Containers))
	}

	// Filter by label
	resp4, err := svc.ListContainers(ctx, &runtime.ListContainersRequest{
		Filter: &runtime.ContainerFilter{LabelSelector: map[string]string{"name": "db"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp4.Containers) != 1 {
		t.Fatalf("want 1 container with name=db, got %d", len(resp4.Containers))
	}
}
