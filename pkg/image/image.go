// Package image 管理本地镜像与容器 rootfs 的组装。
//
// Level 2 阶段的镜像 = 本地目录结构：
//
//	<root>/images/<name>/layers/<index>/   每层解压后的文件树
//	<root>/images/<name>/manifest.json     镜像元数据（层顺序等）
//
// 容器 rootfs 用 OverlayFS 组装：
//
//	lowerdir = layers/0:layers/1:...  （从下往上，layer 0 在最底）
//	upperdir = <container>/upper
//	workdir  = <container>/work
//	merged   = <container>/merged     ← 容器内的 /
//
// Level 3 再对接 OCI registry / manifest，这里保持极简。
package image

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Manifest 是一个镜像的元数据文件内容。
type Manifest struct {
	Name      string    `json:"name"`
	Reference string    `json:"reference,omitempty"`  // 完整引用（如 index.docker.io/library/nginx:latest）
	Digest    string    `json:"digest,omitempty"`     // image config digest（sha256:...）
	Layers    []string  `json:"layers"`               // 相对 images/<name>/ 的路径，顺序：下到上
	LayerRefs []string  `json:"layer_refs,omitempty"` // 共享层目录路径（相对 <root>/layers/），与 Layers 一一对应
	Size      int64     `json:"size,omitempty"`       // 镜像总大小（字节）
	CreatedAt time.Time `json:"created_at"`

	// --- image config（默认值，run 时可被覆盖）---
	Config ImageConfig `json:"config,omitempty"`
}

// ImageConfig 是 OCI image config 的极简子集：容器启动时的默认行为。
type ImageConfig struct {
	Env        []string `json:"env,omitempty"`         // KEY=VALUE
	Cmd        []string `json:"cmd,omitempty"`         // 默认命令
	Entrypoint []string `json:"entrypoint,omitempty"`  // 默认 entrypoint
	WorkingDir string   `json:"working_dir,omitempty"` // 默认 cwd
	User       string   `json:"user,omitempty"`        // 默认 user（暂未实现）
}

// Store 管理本地镜像。
type Store struct {
	root string // 数据根目录，镜像放在 <root>/images/
}

// New 返回一个镜像 Store，目录不存在会自动创建。
func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("image store root is empty")
	}
	dir := filepath.Join(root, "images")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

// Root 返回 Store 的根目录。
func (s *Store) Root() string { return s.root }

// LayersDir 返回共享层目录 <root>/layers/，按 digest 寻址。
func (s *Store) LayersDir() string { return filepath.Join(s.root, "layers") }

// ImageDir 返回镜像根目录（可能尚未创建）。
func (s *Store) ImageDir(name string) string {
	return filepath.Join(s.root, "images", name)
}

// Exists 判断镜像是否存在。支持任意引用形式（通过 Resolve）。
func (s *Store) Exists(ref string) bool {
	if _, err := os.Stat(filepath.Join(s.ImageDir(ref), "manifest.json")); err == nil {
		return true
	}
	m, _ := s.Resolve(ref)
	return m != nil
}

// List 返回所有镜像的 manifest。
func (s *Store) List() ([]*Manifest, error) {
	base := filepath.Join(s.root, "images")
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]*Manifest, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := s.loadManifest(e.Name())
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: load image %s: %v\n", e.Name(), err)
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func (s *Store) loadManifest(name string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(s.ImageDir(name), "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// LoadManifest returns the manifest for a named image (exported wrapper).
func (s *Store) LoadManifest(name string) (*Manifest, error) {
	return s.loadManifest(name)
}

// Resolve 根据用户提供的引用找到本地镜像。支持四种形式：
//   - 短名（manifest.Name）                         : "nginx:alpine"
//   - 完整 registry 引用（manifest.Reference）      : "index.docker.io/library/nginx:alpine"
//   - digest（manifest.Digest 或 sha256:<hex>）     : "sha256:abc..."
//   - 无 tag 的短名，自动补 ":latest"               : "nginx"
//
// 未找到返回 nil, nil（不是错误）。
func (s *Store) Resolve(ref string) (*Manifest, error) {
	if ref == "" {
		return nil, nil
	}
	images, err := s.List()
	if err != nil {
		return nil, err
	}

	// 1. Exact match on short name or full reference or digest
	for _, m := range images {
		if m.Name == ref || m.Reference == ref || m.Digest == ref {
			return m, nil
		}
	}

	// 2. Try with default ":latest" suffix
	if !containsRune(ref, ':') && !containsRune(ref, '@') {
		refWithTag := ref + ":latest"
		for _, m := range images {
			if m.Name == refWithTag || m.Reference == refWithTag {
				return m, nil
			}
		}
	}

	// 3. Try matching by last path segment (e.g. "pause:3.9" matches
	//    "registry.k8s.io/pause:3.9")
	for _, m := range images {
		if lastSegment(m.Reference) == ref || lastSegment(m.Name) == ref {
			return m, nil
		}
	}

	return nil, nil
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

// lastSegment returns "nginx:alpine" from "index.docker.io/library/nginx:alpine".
func lastSegment(ref string) string {
	if ref == "" {
		return ""
	}
	// Find last '/' that separates host/path from image name.
	for i := len(ref) - 1; i >= 0; i-- {
		if ref[i] == '/' {
			return ref[i+1:]
		}
	}
	return ref
}

// SaveManifest atomically writes a manifest.
func (s *Store) SaveManifest(m *Manifest) error {
	if m.Name == "" {
		return errors.New("manifest name is empty")
	}
	dir := s.ImageDir(m.Name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	target := filepath.Join(dir, "manifest.json")
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// Remove 删除一个镜像（只在没有容器引用它时才安全，调用方自行检查）。
// 对于拉取的镜像（使用 LayerRefs），会尝试 GC 没有被其它镜像引用的共享层。
func (s *Store) Remove(name string) error {
	if !s.Exists(name) {
		return fmt.Errorf("no such image: %s", name)
	}
	m, _ := s.loadManifest(name)
	if err := os.RemoveAll(s.ImageDir(name)); err != nil {
		return err
	}
	if m != nil && len(m.LayerRefs) > 0 {
		s.gcOrphanLayers()
	}
	return nil
}

// gcOrphanLayers 扫描 <root>/layers/ 下所有层，删除没有任何镜像引用的。
// 失败不视为致命错误（只打 warn）。
func (s *Store) gcOrphanLayers() {
	// 收集所有镜像引用的层
	refs := make(map[string]bool)
	images, _ := s.List()
	for _, m := range images {
		for _, lr := range m.LayerRefs {
			// LayerRefs 形如 "layers/<key>"
			key := filepath.Base(lr)
			refs[key] = true
		}
	}
	entries, err := os.ReadDir(s.LayersDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !refs[e.Name()] {
			_ = os.RemoveAll(filepath.Join(s.LayersDir(), e.Name()))
		}
	}
}

// Import 从 tar/tar.gz 归档导入一个镜像。
// 导入后镜像只有一层（layer 0），和 `docker export` 导出的 rootfs 行为一致。
// Level 3 的 PullImage 实现会调另一条路径（多层 + whiteout 处理）。
func (s *Store) Import(name, tarPath string) error {
	if name == "" {
		return errors.New("image name is empty")
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("image name cannot contain slashes: %q", name)
	}
	if s.Exists(name) {
		return fmt.Errorf("image %s already exists", name)
	}

	imgDir := s.ImageDir(name)
	layerDir := filepath.Join(imgDir, "layers", "0")
	if err := os.MkdirAll(layerDir, 0755); err != nil {
		return err
	}

	if err := extractTar(tarPath, layerDir); err != nil {
		_ = os.RemoveAll(imgDir)
		return fmt.Errorf("extract %s: %w", tarPath, err)
	}

	m := &Manifest{
		Name:      name,
		Layers:    []string{filepath.Join("layers", "0")},
		CreatedAt: time.Now().UTC(),
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(imgDir, "manifest.json"), data, 0644); err != nil {
		_ = os.RemoveAll(imgDir)
		return err
	}
	return nil
}

// LayerPaths 返回镜像所有 layer 的绝对路径，从下到上。
// 支持任意引用形式。
func (s *Store) LayerPaths(ref string) ([]string, error) {
	m, err := s.loadManifest(ref)
	if err != nil {
		// Fall back to Resolve for non-direct matches
		rm, rerr := s.Resolve(ref)
		if rerr != nil {
			return nil, rerr
		}
		if rm == nil {
			return nil, fmt.Errorf("image %s has no layers", ref)
		}
		m = rm
	}
	if len(m.Layers) == 0 && len(m.LayerRefs) == 0 {
		return nil, fmt.Errorf("image %s has no layers", m.Name)
	}
	if len(m.LayerRefs) > 0 {
		out := make([]string, 0, len(m.LayerRefs))
		for _, rel := range m.LayerRefs {
			out = append(out, filepath.Join(s.root, rel))
		}
		return out, nil
	}
	out := make([]string, 0, len(m.Layers))
	for _, rel := range m.Layers {
		out = append(out, filepath.Join(s.ImageDir(m.Name), rel))
	}
	return out, nil
}

// RootfsPaths 描述容器 rootfs 相关的目录。
type RootfsPaths struct {
	Upper  string // 读写层
	Work   string // overlayfs 内部工作目录
	Merged string // 容器内可见的 /
}

// ContainerRootfsPaths 生成一个容器对应的 rootfs 路径集合（不创建目录）。
// containerDir 约定为 <root>/containers/<id>。
func ContainerRootfsPaths(containerDir string) RootfsPaths {
	return RootfsPaths{
		Upper:  filepath.Join(containerDir, "upper"),
		Work:   filepath.Join(containerDir, "work"),
		Merged: filepath.Join(containerDir, "merged"),
	}
}

// PrepareRootfs 为一个容器挂好 overlayfs，返回 merged 目录路径。
// 失败时会自行清理已创建的目录和挂载。
func (s *Store) PrepareRootfs(imageName, containerDir string) (string, error) {
	return prepareRootfs(s, imageName, containerDir)
}

// CleanupRootfs 卸载 overlayfs 并清理 upper/work/merged。
// 对不存在或已卸载的情况幂等。
func CleanupRootfs(containerDir string) error {
	return cleanupRootfs(containerDir)
}

// formatLowerDirs 把 layers 拼成 overlay mount 需要的 lowerdir 字符串。
// overlayfs 的 lowerdir 顺序是 **顶层在前**，即 layers 数组倒过来。
func formatLowerDirs(layers []string) string {
	rev := make([]string, len(layers))
	for i, l := range layers {
		rev[len(layers)-1-i] = l
	}
	return strings.Join(rev, ":")
}
