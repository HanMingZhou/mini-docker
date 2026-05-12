package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/spf13/cobra"

	"github.com/mini-docker/mini-docker/pkg/image"
	"github.com/mini-docker/mini-docker/pkg/store"
)

func newImagePushCmd() *cobra.Command {
	var destination string
	cmd := &cobra.Command{
		Use:   "push <local-image> [flags]",
		Short: "Push a local image to a remote registry",
		Long: `Push uploads a local image to a remote registry, re-using auth from
~/.docker/config.json. The local image must already have layer directories
in the shared layers pool (i.e. it came from 'pull', 'commit', or 'build').

If --to is omitted, the image's stored Reference is used.`,
		Example: `  mydocker image push my-custom --to myuser/my-custom:v1
  mydocker image push nginx:alpine --to registry.internal/nginx:alpine`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return pushImage(args[0], destination)
		},
	}
	cmd.Flags().StringVar(&destination, "to", "", "destination registry reference (default: image's stored reference)")
	return cmd
}

func pushImage(localRef, destRef string) error {
	is, err := image.New(store.Root())
	if err != nil {
		return err
	}
	m, err := is.Resolve(localRef)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("no such local image: %s", localRef)
	}

	if destRef == "" {
		destRef = m.Reference
	}
	if destRef == "" {
		return fmt.Errorf("image has no stored reference; use --to to specify destination")
	}

	tag, err := name.ParseReference(destRef, name.WeakValidation)
	if err != nil {
		return fmt.Errorf("parse reference %q: %w", destRef, err)
	}

	// Build a v1.Image from our local layer dirs by tarring each layer.
	img, err := buildV1Image(is, m)
	if err != nil {
		return fmt.Errorf("build image: %w", err)
	}

	auth, err := authn.DefaultKeychain.Resolve(tag.Context())
	if err != nil {
		return fmt.Errorf("resolve auth: %w", err)
	}

	fmt.Fprintf(os.Stderr, "pushing %s → %s\n", localRef, tag.Name())
	if err := remote.Write(tag, img, remote.WithAuth(auth)); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	fmt.Fprintln(os.Stderr, "done")
	return nil
}

// buildV1Image converts our local layer directories into a v1.Image. Each
// directory is turned into an uncompressed tar stream (with hardlink dedup)
// and wrapped as a v1.Layer. The image config is taken from Manifest.Config.
func buildV1Image(is *image.Store, m *image.Manifest) (v1.Image, error) {
	// Resolve absolute layer paths in bottom-up order.
	layerPaths, err := is.LayerPaths(m.Name)
	if err != nil {
		return nil, err
	}

	layers := make([]v1.Layer, 0, len(layerPaths))
	for _, p := range layerPaths {
		l, err := tarLayerFromDir(p)
		if err != nil {
			return nil, fmt.Errorf("layer %s: %w", filepath.Base(p), err)
		}
		layers = append(layers, l)
	}

	// Start from an empty image, append layers, then overlay config.
	base := empty.Image
	img, err := mutate.AppendLayers(base, layers...)
	if err != nil {
		return nil, err
	}

	// Apply image config (CMD/ENV/WORKDIR).
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	cfg.Created = v1.Time{Time: m.CreatedAt}
	if cfg.Config.Env == nil {
		cfg.Config.Env = []string{}
	}
	cfg.Config.Env = append(cfg.Config.Env, m.Config.Env...)
	cfg.Config.Cmd = m.Config.Cmd
	cfg.Config.Entrypoint = m.Config.Entrypoint
	cfg.Config.WorkingDir = m.Config.WorkingDir
	cfg.Config.User = m.Config.User
	cfg.OS = "linux"
	if cfg.Architecture == "" {
		cfg.Architecture = "amd64"
	}

	return mutate.ConfigFile(img, cfg)
}

// tarLayerFromDir returns a v1.Layer whose content is a gzip-compressed tar
// of src. We use go-containerregistry's tarball.LayerFromOpener so it can
// stream the data on demand.
func tarLayerFromDir(src string) (v1.Layer, error) {
	// Precompute the tar bytes (in-memory). Busybox-sized layers are small.
	// For large layers a streaming approach would be better, but this is
	// fine for a teaching project.
	rawTar, err := buildLayerTar(src)
	if err != nil {
		return nil, err
	}

	// gzip-compress for the actual blob upload.
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(rawTar); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	gzData := gzBuf.Bytes()

	return tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(gzData)), nil
	}, tarball.WithMediaType(types.DockerLayer))
}

// buildLayerTar walks src and returns a tar byte slice with hardlink dedup.
// Shares logic with save.go's tarDirIntoArchive but without the outer wrapper.
func buildLayerTar(src string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	seen := map[uint64]string{}

	err := filepath.Walk(src, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		rel, _ := filepath.Rel(src, path)
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel

		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			hdr.Linkname = link
			return tw.WriteHeader(hdr)
		}
		if info.Mode().IsRegular() {
			if stat, ok := info.Sys().(*syscallStatT); ok && stat != nil && stat.Nlink > 1 {
				ino := stat.Ino
				if first, seenBefore := seen[ino]; seenBefore {
					hdr.Typeflag = tar.TypeLink
					hdr.Linkname = first
					hdr.Size = 0
					return tw.WriteHeader(hdr)
				}
				seen[ino] = rel
			}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(tw, f)
			_ = f.Close()
			if copyErr != nil {
				return copyErr
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Unused imports we keep around for future enhancements (manifest digests etc.)
var (
	_ = sha256.Sum256
	_ = hex.EncodeToString
	_ = json.Marshal
	_ = strings.HasPrefix
	_ = time.Now
)
