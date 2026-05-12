package cri

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/mini-docker/mini-docker/pkg/image"
)

// --- ImageService real implementation ---------------------------------------
//
// Replaces the stub list/imagefsinfo in server.go by method precedence;
// server.go still registers ImageService via the embedded Unimplemented*.
// Go does not allow duplicate method definitions on the same type, so we
// override by re-declaring each RPC on *ImageService here. The stubs in
// server.go have been removed to avoid conflicts.

// PullImage pulls an image from a remote registry. Auth falls back to
// ~/.docker/config.json via authn.DefaultKeychain.
func (s *ImageService) PullImage(_ context.Context, req *runtime.PullImageRequest) (*runtime.PullImageResponse, error) {
	if req.Image == nil || req.Image.Image == "" {
		return nil, errors.New("PullImage: missing image")
	}
	ref := req.Image.Image

	is, err := image.New(s.root)
	if err != nil {
		return nil, err
	}

	opts := image.PullOptions{
		Progress: func(msg string) {
			fmt.Fprintln(os.Stderr, "[pull]", msg)
		},
	}
	if req.Auth != nil {
		opts.Auth = authFromCRI(req.Auth)
	}

	m, err := is.Pull(ref, opts)
	if err != nil {
		return nil, fmt.Errorf("PullImage %q: %w", ref, err)
	}

	return &runtime.PullImageResponse{ImageRef: m.Name}, nil
}

// ListImages returns all local images.
func (s *ImageService) ListImages(_ context.Context, req *runtime.ListImagesRequest) (*runtime.ListImagesResponse, error) {
	is, err := image.New(s.root)
	if err != nil {
		return nil, err
	}
	list, err := is.List()
	if err != nil {
		return nil, err
	}
	out := make([]*runtime.Image, 0, len(list))
	for _, m := range list {
		img := manifestToProto(m)
		if !matchImageFilter(img, req.Filter) {
			continue
		}
		out = append(out, img)
	}
	return &runtime.ListImagesResponse{Images: out}, nil
}

// ImageStatus returns status for a single image.
func (s *ImageService) ImageStatus(_ context.Context, req *runtime.ImageStatusRequest) (*runtime.ImageStatusResponse, error) {
	if req.Image == nil || req.Image.Image == "" {
		return nil, errors.New("ImageStatus: missing image")
	}
	is, err := image.New(s.root)
	if err != nil {
		return nil, err
	}
	m, err := is.Resolve(req.Image.Image)
	if err != nil {
		return nil, err
	}
	if m == nil {
		// CRI convention: not found -> nil status, no error
		return &runtime.ImageStatusResponse{}, nil
	}
	return &runtime.ImageStatusResponse{Image: manifestToProto(m)}, nil
}

// RemoveImage deletes a local image.
func (s *ImageService) RemoveImage(_ context.Context, req *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error) {
	if req.Image == nil || req.Image.Image == "" {
		return nil, errors.New("RemoveImage: missing image")
	}
	is, err := image.New(s.root)
	if err != nil {
		return nil, err
	}
	m, err := is.Resolve(req.Image.Image)
	if err != nil {
		return nil, err
	}
	if m == nil {
		// idempotent: not found is ok
		return &runtime.RemoveImageResponse{}, nil
	}
	if err := is.Remove(m.Name); err != nil {
		return nil, err
	}
	return &runtime.RemoveImageResponse{}, nil
}

// ImageFsInfo reports image storage usage. Kubelet uses this for disk pressure.
func (s *ImageService) ImageFsInfo(_ context.Context, _ *runtime.ImageFsInfoRequest) (*runtime.ImageFsInfoResponse, error) {
	imagesDir := filepath.Join(s.root, "images")
	layersDir := filepath.Join(s.root, "layers")

	used, err := dirSize(imagesDir) // manifests + (legacy) embedded layers
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if n, err := dirSize(layersDir); err == nil {
		used += n
	}

	mountPoint := s.root
	if _, err := os.Stat(mountPoint); err != nil {
		mountPoint = "/"
	}

	return &runtime.ImageFsInfoResponse{
		ImageFilesystems: []*runtime.FilesystemUsage{
			{
				Timestamp:  0,
				FsId:       &runtime.FilesystemIdentifier{Mountpoint: mountPoint},
				UsedBytes:  &runtime.UInt64Value{Value: uint64(used)},
				InodesUsed: &runtime.UInt64Value{Value: 0},
			},
		},
	}, nil
}

// --- helpers ----------------------------------------------------------------

func manifestToProto(m *image.Manifest) *runtime.Image {
	// Use full reference as the primary tag when available, fall back to short name.
	tags := []string{m.Name}
	if m.Reference != "" && m.Reference != m.Name {
		tags = append(tags, m.Reference)
	}
	return &runtime.Image{
		Id:          m.Digest,
		RepoTags:    tags,
		RepoDigests: digestList(m),
		Size_:       uint64(m.Size),
	}
}

func digestList(m *image.Manifest) []string {
	if m.Digest == "" || m.Reference == "" {
		return nil
	}
	// Strip any existing tag, then append @<digest>
	ref := m.Reference
	if i := strings.LastIndex(ref, ":"); i > 0 {
		// only strip if ":" appears after last "/"
		if j := strings.LastIndex(ref, "/"); j < i {
			ref = ref[:i]
		}
	}
	return []string{ref + "@" + m.Digest}
}

func matchImageFilter(img *runtime.Image, f *runtime.ImageFilter) bool {
	if f == nil || f.Image == nil || f.Image.Image == "" {
		return true
	}
	q := f.Image.Image
	for _, t := range img.RepoTags {
		if t == q || strings.HasPrefix(t, q+":") {
			return true
		}
	}
	return img.Id == q
}

func authFromCRI(cfg *runtime.AuthConfig) authn.Authenticator {
	if cfg == nil {
		return nil
	}
	if cfg.IdentityToken != "" {
		return &authn.Bearer{Token: cfg.IdentityToken}
	}
	if cfg.Auth != "" {
		// base64(username:password) — let authn decode via Basic with empty
		// username/password is wrong; fall back to anonymous.
		// go-containerregistry does not expose a decoder here, but the
		// common case for CRI is Username/Password below.
	}
	if cfg.Username != "" || cfg.Password != "" {
		return &authn.Basic{Username: cfg.Username, Password: cfg.Password}
	}
	return authn.Anonymous
}

func dirSize(p string) (int64, error) {
	var total int64
	err := filepath.Walk(p, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}
