//go:build integration

package rpce2e

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	easyopsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	"github.com/easy-platform/easyops/internal/opssvc"
	"github.com/easy-platform/easyops/k8s/imagebuild"
)

// buildkitAddr returns the daemon address, or skips when it is absent.
func buildkitAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("EASYOPS_TEST_BUILDKIT_ADDR")
	if addr == "" {
		t.Skip("EASYOPS_TEST_BUILDKIT_ADDR is not set: run scripts/k3d-buildkit.sh")
	}
	return addr
}

// newBuildHarness is newHarness with a REAL image builder wired in.
//
// It reuses the same server stack; only Deps.Builder differs, which is the point:
// the build RPCs are the only ones that need a daemon, and everything else stays
// exactly as the other suites run it.
func newBuildHarness(t *testing.T) *harness {
	t.Helper()
	if os.Getenv("EASYOPS_KUBECONFIG") == "" && os.Getenv("KUBECONFIG") == "" {
		t.Skip("no cluster configured")
	}
	addr := buildkitAddr(t)
	reg := os.Getenv("EASYOPS_TEST_REGISTRY")
	if reg == "" {
		t.Skip("EASYOPS_TEST_REGISTRY is not set")
	}
	return newHarnessWith(t, func(d *opssvc.Deps) {
		// The registry is plain HTTP inside the cluster (k3d provides it that
		// way), so the builder must push insecurely.
		d.Builder = &imagebuild.Builder{
			Addr:           addr,
			RegistryHost:   reg,
			RegistryScheme: "http",
			Timeout:        10 * time.Minute,
		}
	})
}

// contextTar builds an in-memory build context.
func contextTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Dockerfile that writes a file, so the built image is provably OURS (not a
// cached or pre-existing one).
const imageDockerfile = `FROM busybox:1.36
ARG CONTENT=default
RUN echo "$CONTENT" > /marker.txt
CMD ["cat", "/marker.txt"]
`

// TestRPC_BuildImageReal pushes a real image and verifies it landed in the
// registry by pulling the manifest back over HTTP.
func TestRPC_BuildImageReal(t *testing.T) {
	h := newBuildHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	repo := fmt.Sprintf("easyops-fixtures/built-%d", time.Now().UnixNano())
	tag := "v1"
	archive := contextTar(t, map[string]string{
		"Dockerfile": imageDockerfile,
	})

	res, err := h.Client.BuildImage(ctx, &easyopsv1.BuildImageRequest{
		Archive: archive, Repo: repo, Tag: tag,
		BuildArgs: map[string]string{"CONTENT": "built-by-easyops"},
	})
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	wantSuffix := repo + ":" + tag
	if !strings.HasSuffix(res.GetImageRef(), wantSuffix) {
		t.Errorf("image_ref = %q, want it to end with %q", res.GetImageRef(), wantSuffix)
	}
	if res.GetLog() == "" {
		t.Error("a successful build should return some log output")
	}

	// The image must really be in the registry: ask it for the manifest.
	regHost := os.Getenv("EASYOPS_TEST_REGISTRY")
	man := httpGet(t, "http://"+hostPortForTests(t)+"/v2/"+repo+"/manifests/"+tag,
		map[string]string{"Accept": "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json"})
	if !strings.Contains(man.body, "schemaVersion") {
		t.Fatalf("the registry does not have the manifest for %s/%s:%s (host=%s): %s",
			regHost, repo, tag, hostPortForTests(t), man.body)
	}
}

// TestRPC_BuildImageRealFailsOnABadDockerfile: a build error must come back as a
// real error (not a success with an empty ref), and must not be Internal.
func TestRPC_BuildImageRealFailsOnABadDockerfile(t *testing.T) {
	h := newBuildHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	archive := contextTar(t, map[string]string{
		"Dockerfile": "FROM busybox:1.36\nRUN exit 42\n",
	})
	_, err := h.Client.BuildImage(ctx, &easyopsv1.BuildImageRequest{
		Archive: archive, Repo: "easyops-fixtures/should-not-exist", Tag: "v1",
	})
	if err == nil {
		t.Fatal("a failing RUN must fail the build")
	}
	// The daemon's failure is a caller-visible build failure, not a server fault.
	if code := connect.CodeOf(err); code == connect.CodeInternal {
		t.Errorf("a build failure must not be Internal: %v", err)
	}
}

// TestRPC_BuildImageStreamReal receives at least one progress event and a final
// image ref: the streaming contract is that the caller SEES progress.
func TestRPC_BuildImageStreamReal(t *testing.T) {
	h := newBuildHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	repo := fmt.Sprintf("easyops-fixtures/stream-%d", time.Now().UnixNano())
	stream, err := h.Client.BuildImageStream(ctx, &easyopsv1.BuildImageRequest{
		Archive: contextTar(t, map[string]string{"Dockerfile": imageDockerfile}),
		Repo:    repo, Tag: "v1",
	})
	if err != nil {
		t.Fatalf("BuildImageStream: %v", err)
	}
	defer stream.Close()

	var events int
	var finalRef string
	for {
		ev, err := stream.Receive()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream receive: %v", err)
		}
		events++
		if ev.GetImageRef() != "" {
			finalRef = ev.GetImageRef()
		}
	}
	if events == 0 {
		t.Error("the stream produced no events")
	}
	if !strings.HasSuffix(finalRef, repo+":v1") {
		t.Errorf("final image ref = %q, want %q", finalRef, repo+":v1")
	}
}

// TestRPC_BuildFilesReal exports a FILE TREE (no image), which is the CI
// artifact path: the build's product comes back through the RPC.
func TestRPC_BuildFilesReal(t *testing.T) {
	h := newBuildHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	dockerfile := `FROM busybox:1.36 AS build
RUN mkdir -p /out && echo "hello-from-build" > /out/artifact.txt
FROM scratch AS export
COPY --from=build /out/ /
`
	res, err := h.Client.BuildFiles(ctx, &easyopsv1.BuildImageRequest{
		Archive: contextTar(t, map[string]string{"Dockerfile": dockerfile}),
		Repo:    "easyops-fixtures/files", Tag: "v1",
		Output: "files", OutputStage: "export",
	})
	if err != nil {
		t.Fatalf("BuildFiles: %v", err)
	}
	if res.GetTruncated() {
		t.Fatal("the tree must not be truncated")
	}
	if res.GetFileCount() == 0 || len(res.GetFiles()) == 0 {
		t.Fatalf("no files exported (count=%d bytes=%d)", res.GetFileCount(), len(res.GetFiles()))
	}
	// The tar must really contain our artifact with our content.
	tr := tar.NewReader(bytes.NewReader(res.GetFiles()))
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read exported tar: %v", err)
		}
		if strings.HasSuffix(hdr.Name, "artifact.txt") {
			b, _ := io.ReadAll(tr)
			if !strings.Contains(string(b), "hello-from-build") {
				t.Errorf("artifact content = %q", string(b))
			}
			found = true
		}
	}
	if !found {
		t.Error("the exported tree does not contain artifact.txt")
	}
}

// TestRPC_ImportImageReal re-pushes an image from the registry under a new name
// (the `FROM <source>` path), then proves the copy exists.
func TestRPC_ImportImageReal(t *testing.T) {
	h := newBuildHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	reg := os.Getenv("EASYOPS_TEST_REGISTRY")
	src := fmt.Sprintf("%s/easyops-fixtures/worker:1", reg)
	dst := fmt.Sprintf("easyops-fixtures/imported-%d", time.Now().UnixNano())

	res, err := h.Client.ImportImage(ctx, &easyopsv1.ImportImageRequest{
		Source: src, Repo: dst, Tag: "v1",
	})
	if err != nil {
		t.Fatalf("ImportImage: %v", err)
	}
	if !strings.HasSuffix(res.GetImageRef(), dst+":v1") {
		t.Errorf("image_ref = %q", res.GetImageRef())
	}
	man := httpGet(t, "http://"+hostPortForTests(t)+"/v2/"+dst+"/manifests/v1",
		map[string]string{"Accept": "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json"})
	if !strings.Contains(man.body, "schemaVersion") {
		t.Fatalf("the imported image is not in the registry: %s", man.body)
	}
}

// TestRPC_CreateBuildReal runs an ASYNC build all the way to completion through
// the build registry (CreateBuild -> poll GetBuild -> the image exists).
func TestRPC_CreateBuildReal(t *testing.T) {
	h := newBuildHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	repo := fmt.Sprintf("easyops-fixtures/async-%d", time.Now().UnixNano())
	created, err := h.Client.CreateBuild(ctx, &easyopsv1.CreateBuildRequest{
		Archive: contextTar(t, map[string]string{"Dockerfile": imageDockerfile}),
		Repo:    repo, Tag: "v1",
	})
	if err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}
	id := created.GetBuild().GetId()
	if id == "" {
		t.Fatal("CreateBuild returned no id")
	}

	// Poll until terminal. This is the contract: the id is stable and the record
	// is readable while the build runs and after it finishes.
	deadline := time.Now().Add(6 * time.Minute)
	var last string
	for time.Now().Before(deadline) {
		got, err := h.Client.GetBuild(ctx, &easyopsv1.GetBuildRequest{Id: id})
		if err != nil {
			t.Fatalf("GetBuild: %v", err)
		}
		if !got.GetFound() {
			t.Fatalf("the build record disappeared while running")
		}
		last = got.GetBuild().GetState()
		// The documented terminal states are done|failed|canceled.
		if last == "done" || last == "failed" || last == "canceled" {
			if last != "done" {
				t.Fatalf("async build state = %s (error: %s)", last, got.GetBuild().GetError())
			}
			break
		}
		time.Sleep(2 * time.Second)
	}
	if last != "done" {
		t.Fatalf("async build never finished (last state %q)", last)
	}

	man := httpGet(t, "http://"+hostPortForTests(t)+"/v2/"+repo+"/manifests/v1",
		map[string]string{"Accept": "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json"})
	if !strings.Contains(man.body, "schemaVersion") {
		t.Fatalf("the async build pushed no image: %s", man.body)
	}
}

// TestRPC_CancelBuildReal cancels a long-running build and asserts the record
// ends up canceled rather than succeeding.
func TestRPC_CancelBuildReal(t *testing.T) {
	h := newBuildHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// `sleep` keeps the build running long enough to cancel.
	archive := contextTar(t, map[string]string{
		"Dockerfile": "FROM busybox:1.36\nRUN sleep 120 && echo done\n",
	})
	created, err := h.Client.CreateBuild(ctx, &easyopsv1.CreateBuildRequest{
		Archive: archive, Repo: "easyops-fixtures/cancel", Tag: "v1",
	})
	if err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}
	id := created.GetBuild().GetId()

	if _, err := h.Client.CancelBuild(ctx, &easyopsv1.CancelBuildRequest{Id: id}); err != nil {
		t.Fatalf("CancelBuild: %v", err)
	}

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		got, err := h.Client.GetBuild(ctx, &easyopsv1.GetBuildRequest{Id: id})
		if err != nil {
			t.Fatalf("GetBuild: %v", err)
		}
		switch got.GetBuild().GetState() {
		case "canceled", "failed":
			return // cancellation took effect (failed is acceptable: the daemon aborts)
		case "done":
			t.Fatal("a canceled build must not end up done")
		}
		time.Sleep(time.Second)
	}
	t.Fatal("the build did not stop after CancelBuild")
}
