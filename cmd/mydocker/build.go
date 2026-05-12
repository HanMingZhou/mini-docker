package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mini-docker/mini-docker/pkg/build"
	"github.com/mini-docker/mini-docker/pkg/cgroup"
	"github.com/mini-docker/mini-docker/pkg/container"
	"github.com/mini-docker/mini-docker/pkg/image"
	"github.com/mini-docker/mini-docker/pkg/namespace"
	"github.com/mini-docker/mini-docker/pkg/store"
)

type buildOptions struct {
	tag        string
	dockerfile string
}

func newBuildCmd() *cobra.Command {
	var o buildOptions
	cmd := &cobra.Command{
		Use:   "build [flags] <context-dir>",
		Short: "Build an image from a Dockerfile",
		Long: `Build reads a Dockerfile from the build context and produces a new
image. The following instructions are supported:

  FROM <image>            (required, must be the first instruction)
  RUN <shell cmd>         (runs inside a throwaway container, commits result)
  COPY <src> <dst>        (copies from context into the current layer)
  ADD <src> <dst>         (alias for COPY; no URL / auto-extract support)
  ENV KEY VALUE           (or KEY=VALUE)
  WORKDIR <path>
  CMD <shell cmd> or CMD ["a","b"]
  ENTRYPOINT <...>        (same forms as CMD)
  USER <name>             (stored but not applied)

Unsupported: ARG, ONBUILD, HEALTHCHECK, STOPSIGNAL, LABEL, multi-stage.`,
		Example: `  mydocker build -t my-app .
  mydocker build -t my-app -f custom.Dockerfile .`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBuild(o, args[0])
		},
	}
	cmd.Flags().StringVarP(&o.tag, "tag", "t", "", "name (:tag) for the built image (required)")
	cmd.Flags().StringVarP(&o.dockerfile, "file", "f", "Dockerfile", "path to Dockerfile within context")
	_ = cmd.MarkFlagRequired("tag")
	return cmd
}

func runBuild(o buildOptions, contextDir string) error {
	// Parse
	absCtx, err := filepath.Abs(contextDir)
	if err != nil {
		return err
	}
	dfPath := o.dockerfile
	if !filepath.IsAbs(dfPath) {
		dfPath = filepath.Join(absCtx, dfPath)
	}
	df, err := os.Open(dfPath)
	if err != nil {
		return fmt.Errorf("open dockerfile: %w", err)
	}
	defer df.Close()
	insts, err := build.Parse(df)
	if err != nil {
		return err
	}
	if len(insts) == 0 {
		return fmt.Errorf("empty Dockerfile")
	}
	if insts[0].Op != "FROM" {
		return fmt.Errorf("Dockerfile must start with FROM")
	}

	is, err := image.New(store.Root())
	if err != nil {
		return err
	}

	// Final config accumulator + layer list
	var (
		config    image.ImageConfig
		layerRefs []string // accumulated across steps (shared <root>/layers/ paths)
	)

	// Step 1: FROM
	base := insts[0].Raw
	fmt.Printf("STEP 1/%d : FROM %s\n", len(insts), base)
	baseManifest, err := is.Resolve(base)
	if err != nil {
		return err
	}
	if baseManifest == nil {
		return fmt.Errorf("base image %q not found locally; run 'mydocker image pull %s' first", base, base)
	}
	// Inherit base config and layers
	config = baseManifest.Config
	layerRefs = append([]string(nil), baseManifest.LayerRefs...)
	// For base images imported via `image import` (no LayerRefs), we'd need to copy
	// layers into the shared pool. For simplicity, require LayerRefs (pulled) base.
	if len(layerRefs) == 0 && len(baseManifest.Layers) > 0 {
		return fmt.Errorf("base image %q was imported (not pulled); build requires a pulled base with shared layers", base)
	}

	// Create a temp image for intermediate steps
	tmpImgBase := "build-tmp-" + shortRand()
	intermediate := tmpImgBase
	// Mark whether we need to cleanup the intermediate at the end
	defer func() {
		// The final named image is the caller's --tag; the tmp ones get removed.
		_ = is.Remove(intermediate)
	}()

	for i, inst := range insts[1:] {
		step := i + 2
		fmt.Printf("STEP %d/%d : %s %s\n", step, len(insts), inst.Op, truncate(inst.Raw, 80))

		switch inst.Op {
		case "FROM":
			return fmt.Errorf("multiple FROM not supported (multi-stage build)")

		case "RUN":
			// Update currentImage to point to the latest intermediate (refreshing the layer refs)
			if err := saveIntermediate(is, intermediate, layerRefs, config); err != nil {
				return err
			}
			// Run the command in a container using the intermediate image
			cmdArgs := []string{"/bin/sh", "-c", inst.Raw}
			newLayer, err := runStep(is, intermediate, cmdArgs, config)
			if err != nil {
				return fmt.Errorf("RUN %q failed: %w", inst.Raw, err)
			}
			layerRefs = append(layerRefs, newLayer)

		case "COPY", "ADD":
			if len(inst.Args) < 2 {
				return fmt.Errorf("%s requires at least <src> <dst>", inst.Op)
			}
			dst := inst.Args[len(inst.Args)-1]
			srcs := inst.Args[:len(inst.Args)-1]
			newLayer, err := copyStep(absCtx, srcs, dst)
			if err != nil {
				return fmt.Errorf("%s failed: %w", inst.Op, err)
			}
			layerRefs = append(layerRefs, newLayer)

		case "ENV":
			k, v, ok := build.ParseEnvAssign(inst.Raw)
			if !ok {
				return fmt.Errorf("ENV: invalid syntax %q", inst.Raw)
			}
			config.Env = upsertEnv(config.Env, k, v)

		case "WORKDIR":
			config.WorkingDir = inst.Raw
			// Docker 语义：WORKDIR 如果目录不存在会自动创建。新建一个层，
			// 里面只包含这个空目录，这样后续 RUN/COPY 能看到它。
			layer, err := mkdirLayer(inst.Raw)
			if err != nil {
				return fmt.Errorf("WORKDIR mkdir: %w", err)
			}
			layerRefs = append(layerRefs, layer)

		case "CMD":
			config.Cmd = parseExecForm(inst.Raw)

		case "ENTRYPOINT":
			config.Entrypoint = parseExecForm(inst.Raw)

		case "USER":
			config.User = inst.Raw

		case "LABEL", "MAINTAINER", "ARG":
			// Silently ignored
			fmt.Printf("  (ignored: %s)\n", inst.Op)

		default:
			return fmt.Errorf("unsupported instruction: %s", inst.Op)
		}
	}

	// Final: save the target image
	if err := saveFinalImage(is, o.tag, layerRefs, config); err != nil {
		return err
	}
	fmt.Printf("Successfully built %s\n", o.tag)
	return nil
}

// saveIntermediate writes a manifest for the temp image so that `run --image`
// can use it as a rootfs. Overwrites if exists.
func saveIntermediate(is *image.Store, name string, layerRefs []string, cfg image.ImageConfig) error {
	// Remove any previous intermediate with this name
	if is.Exists(name) {
		_ = is.Remove(name)
	}
	return is.SaveManifest(&image.Manifest{
		Name:      name,
		LayerRefs: append([]string(nil), layerRefs...),
		Layers:    []string{}, // ensure non-empty LayerRefs is used
		Config:    cfg,
		CreatedAt: time.Now().UTC(),
	})
}

func saveFinalImage(is *image.Store, name string, layerRefs []string, cfg image.ImageConfig) error {
	if is.Exists(name) {
		_ = is.Remove(name)
	}
	return is.SaveManifest(&image.Manifest{
		Name:      name,
		LayerRefs: append([]string(nil), layerRefs...),
		Layers:    []string{},
		Config:    cfg,
		CreatedAt: time.Now().UTC(),
	})
}

// runStep runs `cmdArgs` in a throwaway container using `fromImage` as the
// rootfs, then copies the upper layer to the shared layer pool.
// Returns the layer path relative to <root>/.
func runStep(is *image.Store, fromImage string, cmdArgs []string, cfg image.ImageConfig) (string, error) {
	// Allocate container dir
	containerID := shortRand()
	containerDir := filepath.Join(store.Root(), "containers", containerID)
	if err := os.MkdirAll(containerDir, 0755); err != nil {
		return "", err
	}
	defer func() {
		_ = image.CleanupRootfs(containerDir)
		_ = os.RemoveAll(containerDir)
	}()

	merged, err := is.PrepareRootfs(fromImage, containerDir)
	if err != nil {
		return "", err
	}

	// Environment: inherit from config
	env := append([]string{}, cfg.Env...)

	containerCfg := container.Config{
		ID:         containerID,
		Name:       "build-" + containerID,
		Rootfs:     merged,
		Hostname:   "builder",
		WorkingDir: cfg.WorkingDir,
		Cmd:        cmdArgs,
		Env:        env,
		TTY:        false,
		Detach:     false,
		Namespaces: namespace.Flags{
			UTS:     true,
			PID:     true,
			Mount:   true,
			Network: false, // share host network so RUN steps can download
			IPC:     true,
		},
		Resources: cgroup.Resources{},
	}

	handle, err := container.Start(containerCfg)
	if err != nil {
		return "", err
	}
	code, werr := handle.Wait()
	if werr != nil {
		return "", werr
	}
	if code != 0 {
		return "", fmt.Errorf("step returned exit code %d", code)
	}

	// Copy the upper layer to the shared layers pool
	upperDir := filepath.Join(containerDir, "upper")
	layerKey := "build-" + shortRand()
	layerRel := filepath.Join("layers", layerKey)
	layerAbs := filepath.Join(store.Root(), layerRel)
	if err := os.MkdirAll(layerAbs, 0755); err != nil {
		return "", err
	}
	if err := copyDir(upperDir, layerAbs); err != nil {
		_ = os.RemoveAll(layerAbs)
		return "", err
	}
	return layerRel, nil
}

// copyStep implements COPY/ADD: copy files from the build context into a
// brand-new layer. No container involved.
func copyStep(contextDir string, srcs []string, dst string) (string, error) {
	layerKey := "build-" + shortRand()
	layerRel := filepath.Join("layers", layerKey)
	layerAbs := filepath.Join(store.Root(), layerRel)
	// Compose target inside the layer: e.g., COPY foo /etc/ → layer/etc/foo
	// COPY foo /etc/bar → layer/etc/bar (single-file)
	//
	// Simplified rule: if dst ends with '/', treat as dir. Otherwise if single
	// src and dst doesn't exist, treat dst as filename.
	dirMode := strings.HasSuffix(dst, "/")
	dstClean := strings.TrimSuffix(dst, "/")
	// Strip leading '/' to place under layer root
	dstClean = strings.TrimPrefix(dstClean, "/")

	for _, s := range srcs {
		src := s
		if !filepath.IsAbs(src) {
			src = filepath.Join(contextDir, src)
		}
		info, err := os.Stat(src)
		if err != nil {
			return "", fmt.Errorf("source %q: %w", s, err)
		}
		var target string
		if dirMode || len(srcs) > 1 || info.IsDir() {
			target = filepath.Join(layerAbs, dstClean, filepath.Base(s))
		} else {
			target = filepath.Join(layerAbs, dstClean)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return "", err
		}
		if info.IsDir() {
			if err := copyDir(src, target); err != nil {
				return "", err
			}
		} else {
			if err := copyFile(src, target, info.Mode()); err != nil {
				return "", err
			}
		}
	}
	return layerRel, nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// mkdirLayer creates a new layer containing just the given directory (empty).
func mkdirLayer(dir string) (string, error) {
	dir = strings.TrimPrefix(dir, "/")
	if dir == "" {
		return "", fmt.Errorf("empty directory")
	}
	layerKey := "build-" + shortRand()
	layerRel := filepath.Join("layers", layerKey)
	layerAbs := filepath.Join(store.Root(), layerRel)
	if err := os.MkdirAll(filepath.Join(layerAbs, dir), 0755); err != nil {
		return "", err
	}
	return layerRel, nil
}

// parseExecForm handles `CMD ["a","b"]` (JSON) or `CMD echo hi` (shell).
func parseExecForm(s string) []string {
	if arr := build.ParseJSONArray(s); arr != nil {
		return arr
	}
	// Shell form: wrap in /bin/sh -c
	return []string{"/bin/sh", "-c", s}
}

// upsertEnv appends or replaces `KEY=...` in env.
func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// shortRand generates a short random hex string for intermediate names.
func shortRand() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
