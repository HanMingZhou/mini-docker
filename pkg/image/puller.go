package image

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// PullOptions controls remote image pulling.
type PullOptions struct {
	// Auth is optional basic auth; nil falls back to ~/.docker/config.json via authn.DefaultKeychain.
	Auth authn.Authenticator

	// Platform (e.g. "linux/amd64"); empty means "linux" + runtime GOARCH.
	Platform string

	// Progress gets called per-layer with human-readable messages.
	// May be nil.
	Progress func(msg string)
}

// Pull downloads an image from a remote registry and stores it locally.
//
// The stored image name is the short name requested by the caller (e.g. "nginx"
// or "nginx:alpine"). Layers are content-addressed and shared under
// <root>/layers/<digest>/ so pulling the same layer twice is a no-op.
func (s *Store) Pull(ref string, opts PullOptions) (*Manifest, error) {
	if ref == "" {
		return nil, fmt.Errorf("empty image reference")
	}

	// Normalize: "nginx" -> "index.docker.io/library/nginx:latest"
	tag, err := name.ParseReference(ref, name.WeakValidation)
	if err != nil {
		return nil, fmt.Errorf("parse reference %q: %w", ref, err)
	}

	auth := opts.Auth
	if auth == nil {
		// Uses ~/.docker/config.json; anonymous for public images.
		a, err := authn.DefaultKeychain.Resolve(tag.Context())
		if err != nil {
			return nil, fmt.Errorf("resolve auth: %w", err)
		}
		auth = a
	}

	remoteOpts := []remote.Option{
		remote.WithAuth(auth),
	}
	if opts.Platform != "" {
		p, err := v1.ParsePlatform(opts.Platform)
		if err != nil {
			return nil, fmt.Errorf("parse platform %q: %w", opts.Platform, err)
		}
		remoteOpts = append(remoteOpts, remote.WithPlatform(*p))
	} else {
		remoteOpts = append(remoteOpts, remote.WithPlatform(v1.Platform{
			OS:           "linux",
			Architecture: defaultArch(),
		}))
	}

	progress := opts.Progress
	if progress == nil {
		progress = func(string) {}
	}

	progress(fmt.Sprintf("pulling %s", tag.Name()))

	img, err := remote.Image(tag, remoteOpts...)
	if err != nil {
		return nil, fmt.Errorf("fetch image: %w", err)
	}

	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("list layers: %w", err)
	}

	if err := os.MkdirAll(s.LayersDir(), 0755); err != nil {
		return nil, err
	}

	var (
		layerRefs []string // paths relative to <root>/, each like "layers/<digest>"
		totalSize int64
	)

	for i, layer := range layers {
		digest, err := layer.DiffID() // uncompressed digest — stable across registries
		if err != nil {
			return nil, fmt.Errorf("layer %d diffid: %w", i, err)
		}
		layerKey := strings.ReplaceAll(digest.String(), ":", "_") // "sha256_<hex>"
		layerDir := filepath.Join(s.LayersDir(), layerKey)
		rel := filepath.Join("layers", layerKey)

		if _, err := os.Stat(filepath.Join(layerDir, ".done")); err == nil {
			progress(fmt.Sprintf("  layer %d/%d %s (cached)", i+1, len(layers), shortDigest(digest.String())))
			layerRefs = append(layerRefs, rel)
			if sz, err := layer.Size(); err == nil {
				totalSize += sz
			}
			continue
		}

		progress(fmt.Sprintf("  layer %d/%d %s", i+1, len(layers), shortDigest(digest.String())))

		if err := os.MkdirAll(layerDir, 0755); err != nil {
			return nil, err
		}

		rc, err := layer.Uncompressed()
		if err != nil {
			_ = os.RemoveAll(layerDir)
			return nil, fmt.Errorf("layer %d uncompressed: %w", i, err)
		}
		if err := extractTarStream(rc, layerDir); err != nil {
			rc.Close()
			_ = os.RemoveAll(layerDir)
			return nil, fmt.Errorf("extract layer %d: %w", i, err)
		}
		rc.Close()

		// Touch a sentinel to mark completion (for the cached check above).
		_ = os.WriteFile(filepath.Join(layerDir, ".done"), []byte(digest.String()), 0644)

		layerRefs = append(layerRefs, rel)
		if sz, err := layer.Size(); err == nil {
			totalSize += sz
		}
	}

	// Save manifest. We keep the full reference as Reference, and use a
	// short canonical name as the on-disk image name.
	imgName := canonicalShortName(tag)
	digest, _ := img.Digest()

	// Extract image config (CMD/ENTRYPOINT/ENV/WorkingDir) from the OCI image.
	// This is critical for Kubelet: when a Pod spec has no command, Kubelet
	// expects the runtime to use the image's default.
	var imgCfg ImageConfig
	if cfg, cfgErr := img.ConfigFile(); cfgErr == nil && cfg != nil {
		imgCfg.Cmd = cfg.Config.Cmd
		imgCfg.Entrypoint = cfg.Config.Entrypoint
		imgCfg.Env = cfg.Config.Env
		imgCfg.WorkingDir = cfg.Config.WorkingDir
		imgCfg.User = cfg.Config.User
	}

	m := &Manifest{
		Name:      imgName,
		Reference: tag.Name(),
		Digest:    digest.String(),
		LayerRefs: layerRefs,
		Size:      totalSize,
		CreatedAt: time.Now().UTC(),
		Config:    imgCfg,
	}
	if err := s.SaveManifest(m); err != nil {
		return nil, err
	}

	progress(fmt.Sprintf("done: %s (%d layers, %s)", imgName, len(layerRefs), humanSize(totalSize)))
	return m, nil
}

// canonicalShortName returns a filesystem-friendly image name from a reference.
// e.g. "index.docker.io/library/nginx:latest" -> "nginx:latest"
//
//	"gcr.io/foo/bar:v1" -> "bar:v1"
//
// This lets users refer to images by the name they typed ("nginx") while we
// still track the full reference in the manifest.
func canonicalShortName(ref name.Reference) string {
	// ref.Name() is the full normalized form. We take the last path segment.
	full := ref.Name()
	// Strip registry (up to first "/" that's a host boundary)
	parts := strings.SplitN(full, "/", 2)
	if len(parts) == 2 {
		full = parts[1]
	}
	// Strip repo namespace (keep last segment only)
	if i := strings.LastIndex(full, "/"); i >= 0 {
		full = full[i+1:]
	}
	return full
}

func shortDigest(d string) string {
	if len(d) > 19 {
		return d[:19]
	}
	return d
}

func humanSize(n int64) string {
	const (
		_  = iota
		kb = 1 << (10 * iota)
		mb
		gb
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.2f GB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.2f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.2f KB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
