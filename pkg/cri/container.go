package cri

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/mini-docker/mini-docker/pkg/cgroup"
	"github.com/mini-docker/mini-docker/pkg/container"
	"github.com/mini-docker/mini-docker/pkg/image"
	"github.com/mini-docker/mini-docker/pkg/namespace"
	"github.com/mini-docker/mini-docker/pkg/sandbox"
	"github.com/mini-docker/mini-docker/pkg/store"
)

// ContainerMeta holds CRI-level container metadata persisted alongside the store.Container.
type ContainerMeta struct {
	SandboxID   string            `json:"sandbox_id"`
	ImageRef    string            `json:"image_ref"`
	State       string            `json:"state"` // CREATED / RUNNING / EXITED
	CreatedAt   int64             `json:"created_at"`
	StartedAt   int64             `json:"started_at,omitempty"`
	FinishedAt  int64             `json:"finished_at,omitempty"`
	ExitCode    int32             `json:"exit_code"`
	Reason      string            `json:"reason,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	LogPath     string            `json:"log_path,omitempty"`
}

// CreateContainer creates a new container within a sandbox.
func (s *RuntimeService) CreateContainer(_ context.Context, req *runtime.CreateContainerRequest) (*runtime.CreateContainerResponse, error) {
	if req.PodSandboxId == "" {
		return nil, fmt.Errorf("CreateContainer: empty pod_sandbox_id")
	}
	if req.Config == nil {
		return nil, fmt.Errorf("CreateContainer: missing config")
	}
	cfg := req.Config

	// Validate sandbox exists and is ready
	sb, err := s.sandboxMgr.Get(req.PodSandboxId)
	if err != nil {
		return nil, fmt.Errorf("CreateContainer: sandbox %s: %w", req.PodSandboxId, err)
	}
	if sb.State != sandbox.StateReady {
		return nil, fmt.Errorf("CreateContainer: sandbox %s is not ready", req.PodSandboxId)
	}

	// Resolve image
	imgName := ""
	if cfg.Image != nil {
		imgName = cfg.Image.Image
	}
	if imgName == "" {
		return nil, fmt.Errorf("CreateContainer: missing image")
	}

	id := newContainerID()
	containerDir := filepath.Join(s.root(), "containers", id)
	if err := os.MkdirAll(containerDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir container dir: %w", err)
	}

	// Prepare rootfs from image using overlayfs
	imgStore, err := image.New(s.root())
	if err != nil {
		_ = os.RemoveAll(containerDir)
		return nil, fmt.Errorf("open image store: %w", err)
	}
	mergedRoot, err := imgStore.PrepareRootfs(imgName, containerDir)
	if err != nil {
		_ = os.RemoveAll(containerDir)
		return nil, fmt.Errorf("prepare rootfs for image %s: %w", imgName, err)
	}

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

	// Determine log path
	logPath := ""
	if cfg.LogPath != "" {
		logDir := ""
		if req.SandboxConfig != nil {
			logDir = req.SandboxConfig.LogDirectory
		}
		if logDir != "" {
			logPath = filepath.Join(logDir, cfg.LogPath)
		} else {
			logPath = filepath.Join(containerDir, cfg.LogPath)
		}
	} else {
		logPath = filepath.Join(containerDir, "container.log")
	}

	// Build command. CRI semantics (similar to Docker/Kubelet):
	//   - If pod spec has Command: use it as entrypoint
	//   - Otherwise: fall back to image's Entrypoint
	//   - If pod spec has Args: use them (appended to command)
	//   - Otherwise: fall back to image's Cmd
	// When both pod and image have entrypoint/cmd, pod overrides image.
	// The final command is: [entrypoint...] + [args...]
	imgManifest, _ := imgStore.Resolve(imgName)
	var entrypoint, args []string
	if len(cfg.Command) > 0 {
		entrypoint = cfg.Command
	} else if imgManifest != nil {
		entrypoint = imgManifest.Config.Entrypoint
	}
	if len(cfg.Args) > 0 {
		args = cfg.Args
	} else if imgManifest != nil && len(cfg.Command) == 0 {
		// Only use image Cmd when pod hasn't overridden command
		args = imgManifest.Config.Cmd
	}
	cmd := append([]string{}, entrypoint...)
	cmd = append(cmd, args...)
	if len(cmd) == 0 {
		_ = image.CleanupRootfs(containerDir)
		_ = os.RemoveAll(containerDir)
		return nil, fmt.Errorf("CreateContainer: no command specified (image has no default CMD/ENTRYPOINT)")
	}

	// Build env (image env first, then pod env overrides)
	var envSlice []string
	if imgManifest != nil {
		envSlice = append(envSlice, imgManifest.Config.Env...)
	}
	for _, kv := range cfg.Envs {
		envSlice = append(envSlice, kv.Key+"="+kv.Value)
	}

	// Apply image working_dir if pod didn't set one
	workingDir := cfg.WorkingDir
	if workingDir == "" && imgManifest != nil {
		workingDir = imgManifest.Config.WorkingDir
	}

	// Persist container metadata
	now := time.Now().UTC()
	rec := &store.Container{
		ID:           id,
		Name:         containerName(cfg, id),
		Rootfs:       mergedRoot,
		WorkingDir:   workingDir,
		Cmd:          cmd,
		Env:          envSlice,
		State:        store.StateCreated,
		CreatedAt:    now,
		LogPath:      logPath,
		CgroupParent: sb.CgroupParent,
		CgroupDriver: string(s.cgroupDriver),
		Resources:    resourcesFromCRI(cfg.Linux),
	}

	st, err := store.New(s.root())
	if err != nil {
		_ = image.CleanupRootfs(containerDir)
		_ = os.RemoveAll(containerDir)
		return nil, err
	}
	if err := st.Save(rec); err != nil {
		_ = image.CleanupRootfs(containerDir)
		_ = os.RemoveAll(containerDir)
		return nil, err
	}

	// Save CRI-specific metadata
	meta := &ContainerMeta{
		SandboxID:   req.PodSandboxId,
		ImageRef:    imgName,
		State:       "CREATED",
		CreatedAt:   now.UnixNano(),
		Labels:      cfg.Labels,
		Annotations: cfg.Annotations,
		LogPath:     logPath,
	}
	if err := s.saveContainerMeta(id, meta); err != nil {
		_ = image.CleanupRootfs(containerDir)
		_ = os.RemoveAll(containerDir)
		return nil, err
	}

	return &runtime.CreateContainerResponse{ContainerId: id}, nil
}

// StartContainer starts a previously created container.
func (s *RuntimeService) StartContainer(_ context.Context, req *runtime.StartContainerRequest) (*runtime.StartContainerResponse, error) {
	if req.ContainerId == "" {
		return nil, fmt.Errorf("StartContainer: empty container_id")
	}

	st, err := store.New(s.root())
	if err != nil {
		return nil, err
	}
	rec, err := st.Resolve(req.ContainerId)
	if err != nil {
		return nil, fmt.Errorf("StartContainer: %w", err)
	}
	if rec.State != store.StateCreated {
		return nil, fmt.Errorf("StartContainer: container %s is in state %s, want Created", rec.ID, rec.State)
	}

	meta, err := s.loadContainerMeta(rec.ID)
	if err != nil {
		return nil, fmt.Errorf("StartContainer: load meta: %w", err)
	}

	// Get sandbox to join its namespaces
	sb, err := s.sandboxMgr.Get(meta.SandboxID)
	if err != nil {
		return nil, fmt.Errorf("StartContainer: sandbox %s: %w", meta.SandboxID, err)
	}

	// Build JoinNS map: container joins sandbox's net/ipc/uts
	joinNS := map[string]string{
		"net": fmt.Sprintf("/proc/%d/ns/net", sb.PID),
		"ipc": fmt.Sprintf("/proc/%d/ns/ipc", sb.PID),
		"uts": fmt.Sprintf("/proc/%d/ns/uts", sb.PID),
	}

	// Build env from the stored record (populated at CreateContainer time).
	envSlice := rec.Env

	cfg := container.Config{
		ID:           rec.ID,
		Name:         rec.ID, // systemd unit 名用 ID，避免中文/特殊字符
		Rootfs:       rec.Rootfs,
		WorkingDir:   rec.WorkingDir,
		Cmd:          rec.Cmd,
		Env:          envSlice,
		TTY:          false,
		Detach:       true,
		LogPath:      rec.LogPath,
		CRILog:       true, // CRI 规定格式，让 `kubectl logs` / `crictl logs` 能用
		JoinNS:       joinNS,
		CgroupParent: rec.CgroupParent,
		CgroupDriver: cgroup.Driver(rec.CgroupDriver),
		InitBinary:   s.mydockerBinary,
		Namespaces: namespace.Flags{
			PID:     true,
			Mount:   true,
			Network: true, // will be cleared by JoinNS
			IPC:     true, // will be cleared by JoinNS
			UTS:     true, // will be cleared by JoinNS
		},
		Resources: rec.Resources,
	}

	handle, err := container.Start(cfg)
	if err != nil {
		return nil, fmt.Errorf("StartContainer: start: %w", err)
	}
	// Release handle — 容器进程 detach 运行。
	// 不能用 cmd.Wait()（会关闭 fd 导致子进程 SIGPIPE）。
	// 用后台 goroutine 定期 waitpid 检测退出。
	_ = handle.Release()

	containerID := rec.ID
	containerPID := handle.PID
	go s.waitContainerExit(containerID, containerPID)

	// Update store
	rec.PID = handle.PID
	rec.State = store.StateRunning
	rec.CgroupPath = handle.CgroupPath
	if err := st.Save(rec); err != nil {
		fmt.Fprintf(os.Stderr, "warn: save container state: %v\n", err)
	}

	// Update CRI meta
	meta.State = "RUNNING"
	meta.StartedAt = time.Now().UTC().UnixNano()
	_ = s.saveContainerMeta(rec.ID, meta)

	return &runtime.StartContainerResponse{}, nil
}

// StopContainer stops a running container.
func (s *RuntimeService) StopContainer(_ context.Context, req *runtime.StopContainerRequest) (*runtime.StopContainerResponse, error) {
	if req.ContainerId == "" {
		return nil, fmt.Errorf("StopContainer: empty container_id")
	}

	st, err := store.New(s.root())
	if err != nil {
		return nil, err
	}
	rec, err := st.Resolve(req.ContainerId)
	if err != nil {
		return nil, fmt.Errorf("StopContainer: %w", err)
	}

	if rec.State != store.StateRunning || !pidAlive(rec.PID) {
		// Already stopped — idempotent
		rec.State = store.StateExited
		rec.FinishedAt = time.Now().UTC()
		_ = st.Save(rec)
		s.markContainerExited(rec.ID, 0)
		return &runtime.StopContainerResponse{}, nil
	}

	// Send SIGTERM, wait up to timeout, then SIGKILL.
	// Kubelet's CRI gRPC default deadline is short (~10s for stop); we need
	// to finish within that. If caller didn't specify, default to 2s so
	// SIGKILL fallback lands well before the RPC deadline.
	timeout := int(req.Timeout)
	if timeout < 0 {
		timeout = 0
	}
	if req.Timeout == 0 {
		timeout = 2
	}
	_ = sendTerm(rec.PID)

	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(rec.PID) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(rec.PID) {
		_ = sendKill(rec.PID)
		// Short wait for process to actually die after SIGKILL
		for i := 0; i < 20; i++ {
			if !pidAlive(rec.PID) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	rec.State = store.StateExited
	rec.FinishedAt = time.Now().UTC()
	_ = st.Save(rec)
	s.markContainerExited(rec.ID, 137)
	container.CleanupDetached(rec.PID)

	return &runtime.StopContainerResponse{}, nil
}

// RemoveContainer removes a stopped container.
func (s *RuntimeService) RemoveContainer(_ context.Context, req *runtime.RemoveContainerRequest) (*runtime.RemoveContainerResponse, error) {
	if req.ContainerId == "" {
		return nil, fmt.Errorf("RemoveContainer: empty container_id")
	}

	st, err := store.New(s.root())
	if err != nil {
		return nil, err
	}
	rec, err := st.Resolve(req.ContainerId)
	if err != nil {
		// Idempotent: not found is OK
		return &runtime.RemoveContainerResponse{}, nil
	}

	// Kill if still running
	if rec.State == store.StateRunning && pidAlive(rec.PID) {
		_ = sendKill(rec.PID)
		time.Sleep(200 * time.Millisecond)
	}

	// Cleanup overlay rootfs
	containerDir := st.ContainerDir(rec.ID)
	_ = image.CleanupRootfs(containerDir)

	// Remove cgroup
	if rec.CgroupPath != "" {
		_ = os.Remove(rec.CgroupPath)
	}

	// Remove container directory (includes meta + logs)
	_ = st.Remove(rec.ID)

	return &runtime.RemoveContainerResponse{}, nil
}

// ContainerStatus returns the status of a container.
func (s *RuntimeService) ContainerStatus(_ context.Context, req *runtime.ContainerStatusRequest) (*runtime.ContainerStatusResponse, error) {
	if req.ContainerId == "" {
		return nil, fmt.Errorf("ContainerStatus: empty container_id")
	}

	st, err := store.New(s.root())
	if err != nil {
		return nil, err
	}
	rec, err := st.Resolve(req.ContainerId)
	if err != nil {
		return nil, fmt.Errorf("ContainerStatus: %w", err)
	}

	// Refresh state
	if rec.State == store.StateRunning && !pidAlive(rec.PID) {
		rec.State = store.StateExited
		rec.FinishedAt = time.Now().UTC()
		_ = st.Save(rec)
		s.markContainerExited(rec.ID, 255)
		container.CleanupDetached(rec.PID)
	}

	meta, _ := s.loadContainerMeta(rec.ID)

	// status.Image must be non-empty or Kubelet rejects the container.
	// Fall back: meta.ImageRef → rec.ImageName → "unknown".
	imageName := ""
	if meta != nil {
		imageName = meta.ImageRef
	}
	if imageName == "" {
		imageName = rec.ImageName
	}
	if imageName == "" {
		imageName = "unknown"
	}

	status := &runtime.ContainerStatus{
		Id: rec.ID,
		Metadata: &runtime.ContainerMetadata{
			Name: rec.Name,
		},
		State:      containerStateToProto(rec.State),
		CreatedAt:  rec.CreatedAt.UnixNano(),
		StartedAt:  0,
		FinishedAt: 0,
		ExitCode:   int32(rec.ExitCode),
		Image:      &runtime.ImageSpec{Image: imageName},
		ImageRef:   imageName,
		LogPath:    rec.LogPath,
	}
	if meta != nil {
		status.StartedAt = meta.StartedAt
		status.FinishedAt = meta.FinishedAt
		status.ExitCode = meta.ExitCode
		status.Labels = meta.Labels
		status.Annotations = meta.Annotations
	}
	if !rec.FinishedAt.IsZero() {
		status.FinishedAt = rec.FinishedAt.UnixNano()
	}

	return &runtime.ContainerStatusResponse{Status: status}, nil
}

// ListContainers returns all containers matching the filter.
func (s *RuntimeService) ListContainers(_ context.Context, req *runtime.ListContainersRequest) (*runtime.ListContainersResponse, error) {
	st, err := store.New(s.root())
	if err != nil {
		return nil, err
	}
	all, err := st.List()
	if err != nil {
		return nil, err
	}

	out := make([]*runtime.Container, 0, len(all))
	for _, rec := range all {
		// Refresh state
		if rec.State == store.StateRunning && !pidAlive(rec.PID) {
			rec.State = store.StateExited
			rec.FinishedAt = time.Now().UTC()
			_ = st.Save(rec)
		}

		meta, _ := s.loadContainerMeta(rec.ID)

		// Image must be non-empty for Kubelet
		imageName := ""
		if meta != nil {
			imageName = meta.ImageRef
		}
		if imageName == "" {
			imageName = rec.ImageName
		}
		if imageName == "" {
			imageName = "unknown"
		}

		c := &runtime.Container{
			Id:           rec.ID,
			PodSandboxId: "",
			Metadata:     &runtime.ContainerMetadata{Name: rec.Name},
			Image:        &runtime.ImageSpec{Image: imageName},
			ImageRef:     imageName,
			State:        containerStateToProto(rec.State),
			CreatedAt:    rec.CreatedAt.UnixNano(),
			Labels:       nil,
			Annotations:  nil,
		}
		if meta != nil {
			c.PodSandboxId = meta.SandboxID
			c.Labels = meta.Labels
			c.Annotations = meta.Annotations
		}

		if !matchContainerFilter(c, req.Filter) {
			continue
		}
		out = append(out, c)
	}

	return &runtime.ListContainersResponse{Containers: out}, nil
}

// --- helpers -----------------------------------------------------------------

func (s *RuntimeService) root() string {
	return s.sandboxMgr.RootPath()
}

func (s *RuntimeService) containerMetaPath(id string) string {
	return filepath.Join(s.root(), "containers", id, "cri_meta.json")
}

func (s *RuntimeService) saveContainerMeta(id string, meta *ContainerMeta) error {
	return writeJSON(meta, s.containerMetaPath(id))
}

func (s *RuntimeService) loadContainerMeta(id string) (*ContainerMeta, error) {
	data, err := os.ReadFile(s.containerMetaPath(id))
	if err != nil {
		return nil, err
	}
	var meta ContainerMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func (s *RuntimeService) markContainerExited(id string, exitCode int32) {
	meta, err := s.loadContainerMeta(id)
	if err != nil {
		return
	}
	meta.State = "EXITED"
	meta.FinishedAt = time.Now().UTC().UnixNano()
	meta.ExitCode = exitCode
	_ = s.saveContainerMeta(id, meta)
}

func containerStateToProto(state store.State) runtime.ContainerState {
	switch state {
	case store.StateCreated:
		return runtime.ContainerState_CONTAINER_CREATED
	case store.StateRunning:
		return runtime.ContainerState_CONTAINER_RUNNING
	case store.StateExited:
		return runtime.ContainerState_CONTAINER_EXITED
	default:
		return runtime.ContainerState_CONTAINER_UNKNOWN
	}
}

func matchContainerFilter(c *runtime.Container, f *runtime.ContainerFilter) bool {
	if f == nil {
		return true
	}
	if f.Id != "" && f.Id != c.Id {
		return false
	}
	if f.PodSandboxId != "" && f.PodSandboxId != c.PodSandboxId {
		return false
	}
	if f.State != nil && f.State.State != c.State {
		return false
	}
	for k, v := range f.LabelSelector {
		if c.Labels[k] != v {
			return false
		}
	}
	return true
}

func containerName(cfg *runtime.ContainerConfig, id string) string {
	if cfg.Metadata != nil && cfg.Metadata.Name != "" {
		return cfg.Metadata.Name
	}
	return id[:8]
}

// resourcesFromCRI 把 CRI 的 LinuxContainerConfig.Resources 转成我们的 cgroup.Resources。
// 只支持 cgroup v2 语义的 memory.max 和 cpu.max。
func resourcesFromCRI(linux *runtime.LinuxContainerConfig) cgroup.Resources {
	if linux == nil || linux.Resources == nil {
		return cgroup.Resources{}
	}
	r := linux.Resources
	out := cgroup.Resources{
		MemoryBytes: r.MemoryLimitInBytes,
	}
	// CRI CPU 字段有多个，优先用 cpu_quota / cpu_period，否则用 shares 转近似值。
	if r.CpuQuota > 0 {
		out.CPUQuotaMicros = r.CpuQuota
		out.CPUPeriodMicros = r.CpuPeriod
	}
	return out
}

func newContainerID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// waitContainerExit 在后台轮询 waitpid 检测容器进程退出。
// 不使用 Go 的 cmd.Wait()（它会关闭 stdout/stderr fd 导致子进程 SIGPIPE）。
// 使用 syscall.Wait4 直接 reap 进程，同时获取退出码。
func (s *RuntimeService) waitContainerExit(containerID string, pid int) {
	for {
		time.Sleep(2 * time.Second)
		if !pidAlive(pid) {
			// 进程已不存在（被别人 reap 了或从未存在）
			s.handleContainerExited(containerID, 255)
			return
		}
		// 尝试 waitpid WNOHANG — 如果进程是我们的子进程且已退出，会 reap 它
		var ws syscall.WaitStatus
		wpid, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		if wpid == pid && err == nil {
			// 成功 reap
			exitCode := 255
			if ws.Exited() {
				exitCode = ws.ExitStatus()
			} else if ws.Signaled() {
				exitCode = 128 + int(ws.Signal())
			}
			s.handleContainerExited(containerID, int32(exitCode))
			return
		}
		// wpid == 0: 进程还在运行
		// wpid == -1, err == ECHILD: 不是我们的子进程（已被 reparent）
		if wpid == -1 {
			// 不是我们的子进程，只能靠 pidAlive 检测
			continue
		}
	}
}

func (s *RuntimeService) handleContainerExited(containerID string, exitCode int32) {
	st, err := store.New(s.root())
	if err != nil {
		return
	}
	rec, err := st.Resolve(containerID)
	if err != nil {
		return
	}
	if rec.State == store.StateRunning {
		rec.State = store.StateExited
		rec.ExitCode = int(exitCode)
		rec.FinishedAt = time.Now().UTC()
		_ = st.Save(rec)
		s.markContainerExited(containerID, exitCode)
		container.CleanupDetached(rec.PID)
	}
}

// writeJSON atomically writes a JSON file.
func writeJSON(v interface{}, path string) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	_ = os.MkdirAll(dir, 0755)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
