package cri

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/mini-docker/mini-docker/pkg/image"
)

func TestImageServiceListAndStatus(t *testing.T) {
	root := t.TempDir()
	svc := newImageService(root)

	// Seed the store with a manifest directly
	is, err := image.New(root)
	if err != nil {
		t.Fatal(err)
	}
	m := &image.Manifest{
		Name:      "nginx:alpine",
		Reference: "index.docker.io/library/nginx:alpine",
		Digest:    "sha256:abc123",
		LayerRefs: []string{filepath.Join("layers", "sha256_deadbeef")},
		Size:      1024,
		CreatedAt: time.Now().UTC(),
	}
	if err := is.SaveManifest(m); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// ListImages
	resp, err := svc.ListImages(ctx, &runtime.ListImagesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Images) != 1 {
		t.Fatalf("want 1 image, got %d", len(resp.Images))
	}
	got := resp.Images[0]
	if got.Id != m.Digest {
		t.Errorf("Id = %q, want %q", got.Id, m.Digest)
	}
	if got.Size_ != uint64(m.Size) {
		t.Errorf("Size_ = %d, want %d", got.Size_, m.Size)
	}

	// ImageStatus (found)
	st, err := svc.ImageStatus(ctx, &runtime.ImageStatusRequest{
		Image: &runtime.ImageSpec{Image: "nginx:alpine"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Image == nil || st.Image.Id != m.Digest {
		t.Fatalf("ImageStatus failed: %+v", st)
	}

	// ImageStatus (not found — returns empty, no error)
	st2, err := svc.ImageStatus(ctx, &runtime.ImageStatusRequest{
		Image: &runtime.ImageSpec{Image: "nonexistent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st2.Image != nil {
		t.Errorf("expected nil image for missing ref, got %+v", st2.Image)
	}

	// RemoveImage
	_, err = svc.RemoveImage(ctx, &runtime.RemoveImageRequest{
		Image: &runtime.ImageSpec{Image: "nginx:alpine"},
	})
	if err != nil {
		t.Fatal(err)
	}

	resp2, _ := svc.ListImages(ctx, &runtime.ListImagesRequest{})
	if len(resp2.Images) != 0 {
		t.Errorf("expected empty after remove, got %d", len(resp2.Images))
	}

	// RemoveImage (idempotent)
	_, err = svc.RemoveImage(ctx, &runtime.RemoveImageRequest{
		Image: &runtime.ImageSpec{Image: "nginx:alpine"},
	})
	if err != nil {
		t.Errorf("RemoveImage should be idempotent, got %v", err)
	}
}

func TestImageFilter(t *testing.T) {
	img := &runtime.Image{
		Id:       "sha256:abc",
		RepoTags: []string{"nginx:alpine", "index.docker.io/library/nginx:alpine"},
	}
	tests := []struct {
		name   string
		filter *runtime.ImageFilter
		want   bool
	}{
		{"nil", nil, true},
		{"exact match short", &runtime.ImageFilter{Image: &runtime.ImageSpec{Image: "nginx:alpine"}}, true},
		{"exact match full", &runtime.ImageFilter{Image: &runtime.ImageSpec{Image: "index.docker.io/library/nginx:alpine"}}, true},
		{"id match", &runtime.ImageFilter{Image: &runtime.ImageSpec{Image: "sha256:abc"}}, true},
		{"miss", &runtime.ImageFilter{Image: &runtime.ImageSpec{Image: "redis"}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := matchImageFilter(img, tc.filter)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPullImageValidation(t *testing.T) {
	svc := newImageService(t.TempDir())
	ctx := context.Background()

	_, err := svc.PullImage(ctx, &runtime.PullImageRequest{})
	if err == nil {
		t.Fatal("expected error for missing image")
	}
	_, err = svc.PullImage(ctx, &runtime.PullImageRequest{
		Image: &runtime.ImageSpec{Image: ""},
	})
	if err == nil {
		t.Fatal("expected error for empty image name")
	}
}
