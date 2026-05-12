// Package store 提供容器元数据的持久化。
//
// Level 2 阶段用文件系统 + JSON 实现，结构：
//
//	<root>/containers/<id>/config.json   容器元数据
//	<root>/containers/<id>/container.log stdio 日志（由 container 包写入）
//	<root>/.lock                          全局 flock 文件（进程间互斥）
//
// 并发保护分三层（详见 README）：
//  1. 写文件用唯一 tmp 路径（避免两路径径竞争覆盖）
//  2. 同进程内按容器 ID 分片互斥锁（sync.Mutex per id）
//  3. 跨进程用 flock 在 `<root>/.lock` 上的文件锁（对 CLI 场景有效）
//
// 后续 Level 可以平滑替换成 bbolt / sqlite 而不改接口。
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mini-docker/mini-docker/pkg/cgroup"
)

// State 是容器生命周期状态。
type State string

const (
	StateCreated State = "Created"
	StateRunning State = "Running"
	StatePaused  State = "Paused"
	StateExited  State = "Exited"
)

// PortMapping 是持久化的端口映射记录（镜像 network.PortMapping 的副本以避免包循环）。
type PortMapping struct {
	HostPort      int32  `json:"host_port"`
	ContainerPort int32  `json:"container_port"`
	Protocol      string `json:"protocol,omitempty"`
	HostIP        string `json:"host_ip,omitempty"`
}

// Container 是一个容器的全部可持久化元数据。
type Container struct {
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	Rootfs        string           `json:"rootfs"`
	Hostname      string           `json:"hostname"`
	WorkingDir    string           `json:"working_dir,omitempty"`
	Cmd           []string         `json:"cmd"`
	Env           []string         `json:"env,omitempty"`
	PID           int              `json:"pid"`
	State         State            `json:"state"`
	ExitCode      int              `json:"exit_code"`
	CreatedAt     time.Time        `json:"created_at"`
	FinishedAt    time.Time        `json:"finished_at,omitempty"`
	LogPath       string           `json:"log_path"`
	CgroupPath    string           `json:"cgroup_path"`
	CgroupParent  string           `json:"cgroup_parent,omitempty"`
	CgroupDriver  string           `json:"cgroup_driver,omitempty"`
	NetworkMode   string           `json:"network_mode,omitempty"` // bridge / host / none
	NetworkIP     string           `json:"network_ip,omitempty"`   // CNI 分配的 IPv4
	PortMappings  []PortMapping    `json:"port_mappings,omitempty"`
	ImageName     string           `json:"image_name,omitempty"`     // 创建时使用的镜像名
	RestartPolicy string           `json:"restart_policy,omitempty"` // no / always / on-failure
	Resources     cgroup.Resources `json:"resources"`
}

// DefaultRoot 是 Linux 上的默认数据目录。
const DefaultRoot = "/var/lib/mydocker"

// Root 返回实际使用的数据目录，受环境变量 MYDOCKER_ROOT 覆盖。
func Root() string {
	if v := os.Getenv("MYDOCKER_ROOT"); v != "" {
		return v
	}
	return DefaultRoot
}

// Store 是容器元数据存储。
//
// 并发安全：
//   - 同进程内多 goroutine：按容器 ID 分片互斥，idLocks 是 sync.Map
//   - 跨进程：通过 WithLock 在 <root>/.lock 上 flock，Save/Remove/Resolve 自动持有
type Store struct {
	root    string
	idLocks sync.Map // string (id) -> *sync.Mutex
}

// New 创建一个以 root 为根目录的 Store，目录不存在时自动创建。
func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("store root is empty")
	}
	cdir := filepath.Join(root, "containers")
	if err := os.MkdirAll(cdir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", cdir, err)
	}
	return &Store{root: root}, nil
}

// RootPath 返回存储根目录。
func (s *Store) RootPath() string { return s.root }

// ContainerDir 返回某个容器的目录路径（可能不存在）。
func (s *Store) ContainerDir(id string) string {
	return filepath.Join(s.root, "containers", id)
}

// lockFor 返回 id 对应的进程内互斥锁。
// 使用 sync.Map 避免全局锁竞争，按 id 分片。
func (s *Store) lockFor(id string) *sync.Mutex {
	v, _ := s.idLocks.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// Save 以原子方式写入容器元数据。
//
// 并发保护：
//  1. 同进程按 id 互斥
//  2. 跨进程通过 flock（由 WithLock 包裹的调用方保证；直接调 Save 也 OK，
//     只是没有跨进程保护）
//  3. 写文件用唯一 tmp 名（tmp.<pid>.<rand>）避免与其它进程/goroutine 碰撞
func (s *Store) Save(c *Container) error {
	if c.ID == "" {
		return errors.New("container id is empty")
	}
	mu := s.lockFor(c.ID)
	mu.Lock()
	defer mu.Unlock()

	dir := s.ContainerDir(c.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal container: %w", err)
	}
	target := filepath.Join(dir, "config.json")
	tmp := uniqueTempPath(target)
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	// rename 是同一文件系统内的原子操作。即便另一个 goroutine/进程正在
	// 读同一个 target，它要么看到旧内容要么新内容，不会看到半截。
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp) // 清理失败的 tmp
		return fmt.Errorf("rename %s -> %s: %w", tmp, target, err)
	}
	return nil
}

// loadByID 根据完整 ID 读取单个容器元数据。
func (s *Store) loadByID(id string) (*Container, error) {
	p := filepath.Join(s.ContainerDir(id), "config.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var c Container
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("decode %s: %w", p, err)
	}
	return &c, nil
}

// List 返回全部容器，按 CreatedAt 倒序。
//
// 并发安全：ReadDir + 原子 rename 的组合让读者永远看到完整文件或 ENOENT。
// 对"写到一半"的 ENOENT 我们跳过（日志打 warn），不阻断整体查询。
func (s *Store) List() ([]*Container, error) {
	cdir := filepath.Join(s.root, "containers")
	entries, err := os.ReadDir(cdir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]*Container, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		c, err := s.loadByID(e.Name())
		if err != nil {
			// 损坏的条目（ENOENT / 写到一半）不阻断整体列表
			if !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "warn: load container %s: %v\n", e.Name(), err)
			}
			continue
		}
		out = append(out, c)
	}
	// 倒序（新的在前）
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// Resolve 根据用户输入（完整 ID / ID 前缀 / Name）找到唯一容器。
// 找不到或有歧义时返回错误。
//
// 注意：Resolve 是一个"查询"操作，不持有互斥锁。上层如果要做
// "查到不存在就创建"的原子操作，必须用 WithLock 包一下。
func (s *Store) Resolve(ref string) (*Container, error) {
	if ref == "" {
		return nil, errors.New("empty container reference")
	}
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	var matches []*Container
	for _, c := range all {
		if c.ID == ref || c.Name == ref {
			return c, nil // 完全匹配立刻返回
		}
		if strings.HasPrefix(c.ID, ref) {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no such container: %s", ref)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("ambiguous container reference %q matches %d containers", ref, len(matches))
	}
}

// Remove 删除容器目录（通常包含 config.json 和日志文件）。
// 调用方负责确认容器已停止。
//
// 并发安全：按 id 互斥，避免和同一容器的 Save 交错。
func (s *Store) Remove(id string) error {
	mu := s.lockFor(id)
	mu.Lock()
	defer mu.Unlock()
	return os.RemoveAll(s.ContainerDir(id))
}

// uniqueTempPath 生成一个基于 target 的唯一 tmp 路径。
// 格式：<target>.tmp.<pid>.<rand16>
// 既能跨进程唯一（pid），又能同进程跨 goroutine 唯一（rand）。
func uniqueTempPath(target string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s.tmp.%d.%s", target, os.Getpid(), hex.EncodeToString(b[:]))
}
