package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mini-docker/mini-docker/pkg/store"
)

func newCpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cp <src> <dst>",
		Short: "Copy files between the host and a container",
		Long: `Copy files/directories between a container and the local filesystem.
Either src or dst must be prefixed with <container>:.

Only running or exited (not yet rm'd) containers are supported, because the
file data lives in the container's overlay merged directory which is unmounted
on 'mydocker rm'.`,
		Example: `  mydocker cp web:/etc/nginx/nginx.conf ./nginx.conf
  mydocker cp ./my.conf web:/etc/nginx/nginx.conf
  mydocker cp web:/var/log/app.log ./logs/`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return cpFile(args[0], args[1])
		},
	}
}

// cpFile implements bidirectional copy.
//
// Path is "<container>:<path>" or a local path. Exactly one side must have
// the container prefix.
func cpFile(src, dst string) error {
	srcRef, srcPath, srcInCtr := splitContainerPath(src)
	dstRef, dstPath, dstInCtr := splitContainerPath(dst)

	if srcInCtr == dstInCtr {
		return fmt.Errorf("exactly one of src/dst must be <container>:<path>")
	}

	st, err := store.New(store.Root())
	if err != nil {
		return err
	}

	switch {
	case srcInCtr:
		c, err := st.Resolve(srcRef)
		if err != nil {
			return err
		}
		return cpFromContainer(c, srcPath, dst)
	case dstInCtr:
		c, err := st.Resolve(dstRef)
		if err != nil {
			return err
		}
		return cpToContainer(src, c, dstPath)
	default:
		return fmt.Errorf("unreachable")
	}
}

// splitContainerPath returns (ref, path, true) for "ref:path", otherwise
// (path, path, false).
// Note: we assume the container ref doesn't contain ':' — which matches our
// `mydocker run --name` constraint.
func splitContainerPath(s string) (ref, path string, hasContainer bool) {
	idx := strings.Index(s, ":")
	if idx < 0 {
		return s, s, false
	}
	// Guard against Windows-like drive letters; we only run on Linux but be tidy.
	before := s[:idx]
	after := s[idx+1:]
	if before == "" {
		return s, s, false
	}
	// A ":" used in URLs or protocols is not our case.
	if strings.HasPrefix(after, "/") {
		return before, after, true
	}
	// Also accept relative container paths (rare but possible)
	return before, after, true
}

func containerRootfsPath(c *store.Container, rel string) string {
	// merged dir is unmounted after 'rm'; for running/exited containers it's
	// still available on the host.
	return filepath.Join(c.Rootfs, strings.TrimPrefix(rel, "/"))
}

func cpFromContainer(c *store.Container, ctrPath, hostPath string) error {
	srcAbs := containerRootfsPath(c, ctrPath)
	info, err := os.Stat(srcAbs)
	if err != nil {
		return fmt.Errorf("container path %s: %w", ctrPath, err)
	}

	// If hostPath is a directory (existing) or ends with '/', copy INTO it.
	targetIsDir := strings.HasSuffix(hostPath, "/")
	if fi, err := os.Stat(hostPath); err == nil && fi.IsDir() {
		targetIsDir = true
	}
	dst := hostPath
	if targetIsDir {
		dst = filepath.Join(strings.TrimSuffix(hostPath, "/"), filepath.Base(ctrPath))
	}

	if info.IsDir() {
		return copyDir(srcAbs, dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	return copyFile(srcAbs, dst, info.Mode())
}

func cpToContainer(hostPath string, c *store.Container, ctrPath string) error {
	info, err := os.Stat(hostPath)
	if err != nil {
		return fmt.Errorf("host path %s: %w", hostPath, err)
	}

	dstAbs := containerRootfsPath(c, ctrPath)
	// If ctrPath ends with '/' or the existing target is a dir, drop source
	// name inside it.
	targetIsDir := strings.HasSuffix(ctrPath, "/")
	if fi, err := os.Stat(dstAbs); err == nil && fi.IsDir() {
		targetIsDir = true
	}
	if targetIsDir {
		dstAbs = filepath.Join(strings.TrimSuffix(dstAbs, "/"), filepath.Base(hostPath))
	}

	if info.IsDir() {
		return copyDir(hostPath, dstAbs)
	}
	if err := os.MkdirAll(filepath.Dir(dstAbs), 0755); err != nil {
		return err
	}
	return copyFile(hostPath, dstAbs, info.Mode())
}

// Make sure we link in os.File Copy to avoid unused import warnings if any.
var _ io.Reader = (*os.File)(nil)
