package image

import (
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
)

func TestCanonicalShortName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"nginx", "nginx:latest"},
		{"nginx:alpine", "nginx:alpine"},
		{"library/nginx:1.25", "nginx:1.25"},
		{"gcr.io/foo/bar:v1", "bar:v1"},
		{"registry.k8s.io/pause:3.9", "pause:3.9"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			ref, err := name.ParseReference(tc.input, name.WeakValidation)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got := canonicalShortName(ref)
			if got != tc.want {
				t.Errorf("canonicalShortName(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestShortDigest(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"sha256:abc", "sha256:abc"},
		{"sha256:abcdefabcdefabcdefabcdef", "sha256:abcdefabcdef"},
	}
	for _, tc := range tests {
		if got := shortDigest(tc.in); got != tc.want {
			t.Errorf("shortDigest(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHumanSize(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.00 KB"},
		{1024 * 1024, "1.00 MB"},
		{1024 * 1024 * 1024, "1.00 GB"},
	}
	for _, tc := range tests {
		if got := humanSize(tc.n); got != tc.want {
			t.Errorf("humanSize(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestStoreResolve(t *testing.T) {
	root := t.TempDir()
	is, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	manifests := []*Manifest{
		{Name: "nginx:alpine", Reference: "index.docker.io/library/nginx:alpine", Digest: "sha256:aaa"},
		{Name: "pause:3.9", Reference: "registry.k8s.io/pause:3.9", Digest: "sha256:bbb"},
		{Name: "busybox:latest", Reference: "index.docker.io/library/busybox:latest", Digest: "sha256:ccc"},
	}
	for _, m := range manifests {
		if err := is.SaveManifest(m); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		ref      string
		wantName string
	}{
		{"nginx:alpine", "nginx:alpine"},                         // short name
		{"index.docker.io/library/nginx:alpine", "nginx:alpine"}, // full reference
		{"sha256:aaa", "nginx:alpine"},                           // digest
		{"busybox", "busybox:latest"},                            // auto-append :latest
		{"registry.k8s.io/pause:3.9", "pause:3.9"},               // Kubelet-style full ref
		{"pause:3.9", "pause:3.9"},                               // short form of Kubelet ref
		{"nonexistent", ""},                                      // miss
	}
	for _, tc := range cases {
		t.Run(tc.ref, func(t *testing.T) {
			m, err := is.Resolve(tc.ref)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantName == "" {
				if m != nil {
					t.Errorf("expected nil, got %+v", m)
				}
				return
			}
			if m == nil {
				t.Fatalf("expected %q, got nil", tc.wantName)
			}
			if m.Name != tc.wantName {
				t.Errorf("got %q, want %q", m.Name, tc.wantName)
			}
		})
	}
}
