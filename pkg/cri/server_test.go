package cri

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/mini-docker/mini-docker/pkg/sandbox"
)

// startBufServer 启动一个内存 gRPC server，避免在 Windows 上开 unix socket。
func startBufServer(t *testing.T) (runtime.RuntimeServiceClient, runtime.ImageServiceClient, func()) {
	t.Helper()

	const bufSize = 1 << 20
	lis := bufconn.Listen(bufSize)

	// 用 t.TempDir() 作为 sandbox root，测试互不影响
	sbMgr, err := sandbox.NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}

	gs := grpc.NewServer()
	runtime.RegisterRuntimeServiceServer(gs, newRuntimeService(sbMgr))
	runtime.RegisterImageServiceServer(gs, newImageService(t.TempDir()))

	go func() {
		_ = gs.Serve(lis)
	}()

	dial := func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}
	conn, err := grpc.DialContext(context.Background(), "bufnet",
		grpc.WithContextDialer(dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cleanup := func() {
		_ = conn.Close()
		gs.Stop()
		_ = lis.Close()
	}
	return runtime.NewRuntimeServiceClient(conn), runtime.NewImageServiceClient(conn), cleanup
}

func TestVersion(t *testing.T) {
	rt, _, cleanup := startBufServer(t)
	defer cleanup()

	resp, err := rt.Version(context.Background(), &runtime.VersionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.RuntimeName != RuntimeName {
		t.Errorf("RuntimeName = %q, want %q", resp.RuntimeName, RuntimeName)
	}
	if resp.RuntimeApiVersion != RuntimeAPIVersion {
		t.Errorf("ApiVersion = %q, want %q", resp.RuntimeApiVersion, RuntimeAPIVersion)
	}
}

func TestStatusReady(t *testing.T) {
	rt, _, cleanup := startBufServer(t)
	defer cleanup()

	resp, err := rt.Status(context.Background(), &runtime.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range resp.Status.Conditions {
		got[c.Type] = c.Status
	}
	if !got[runtime.RuntimeReady] {
		t.Errorf("RuntimeReady not true")
	}
	// NetworkReady depends on CNI configuration being loaded. In tests we don't
	// load any CNI config, so it's expected to be false. Just assert the
	// condition exists.
	if _, ok := got[runtime.NetworkReady]; !ok {
		t.Errorf("NetworkReady condition missing")
	}
}

func TestListsEmpty(t *testing.T) {
	rt, img, cleanup := startBufServer(t)
	defer cleanup()

	if _, err := rt.ListPodSandbox(context.Background(), &runtime.ListPodSandboxRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.ListContainers(context.Background(), &runtime.ListContainersRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := img.ListImages(context.Background(), &runtime.ListImagesRequest{}); err != nil {
		t.Fatal(err)
	}
}
