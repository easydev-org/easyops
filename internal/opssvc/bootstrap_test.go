package opssvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/easy-platform/easyops/k8s/runtimeprofiles"
	"github.com/easy-platform/easyops/k8s/sandboxmgr"
)

// fakeExecutor records the files written via the worker file service.
type fakeExecutor struct {
	files    map[string]string
	writeErr error
	infoOS   string
	infoHome string
}

func (f *fakeExecutor) Info(_ context.Context) (workerInfo, error) {
	return workerInfo{OS: f.infoOS, Home: f.infoHome}, nil
}

func (f *fakeExecutor) WriteFile(_ context.Context, path string, content []byte) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	if f.files == nil {
		f.files = map[string]string{}
	}
	f.files[path] = string(content)
	return nil
}

// TestRunSandboxBootstrapWritesFiles: with a configured upstream the bootstrap
// writes the file configs pointing at the (trimmed) artifact base.
func TestRunSandboxBootstrapWritesFiles(t *testing.T) {
	cs := fake.NewSimpleClientset()
	sbx := sandboxmgr.NewWithClientset(cs, sandboxmgr.Config{Namespace: "worker"})
	ctx := context.Background()
	if _, _, err := sbx.Create(ctx, sandboxmgr.Spec{Name: "sb", Image: "img:1", Creator: "a"}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}
	fe := &fakeExecutor{}
	s := New(Deps{
		Sandbox:                sbx,
		Runtime:                runtimeprofiles.DefaultSettings(),
		SandboxPackageUpstream: "http://artifact.worker.svc.cluster.local/",
		NewWorker:              func(_, _ string) workerExecutor { return fe },
	})

	s.runSandboxBootstrap(ctx, "sb")

	apt, ok := fe.files["/etc/apt/sources.list.d/artifact.list"]
	if !ok || !strings.Contains(apt, "http://artifact.worker.svc.cluster.local/artifacts/debian/debian trixie main") {
		t.Fatalf("apt source not written/pointing at the base: %q", apt)
	}
	if _, ok := fe.files["/root/.cargo/config.toml"]; !ok {
		t.Fatalf("cargo config not written")
	}
	if _, ok := fe.files["/etc/yum.repos.d/artifact.repo"]; !ok {
		t.Fatalf("rpm repo not written")
	}
}

// TestRunSandboxBootstrapDisabled: no upstream -> nothing runs.
func TestRunSandboxBootstrapDisabled(t *testing.T) {
	cs := fake.NewSimpleClientset()
	sbx := sandboxmgr.NewWithClientset(cs, sandboxmgr.Config{Namespace: "worker"})
	ctx := context.Background()
	if _, _, err := sbx.Create(ctx, sandboxmgr.Spec{Name: "sb", Image: "img:1"}); err != nil {
		t.Fatal(err)
	}
	fe := &fakeExecutor{}
	called := false
	s := New(Deps{
		Sandbox:   sbx,
		Runtime:   runtimeprofiles.DefaultSettings(),
		NewWorker: func(_, _ string) workerExecutor { called = true; return fe },
	})
	s.runSandboxBootstrap(ctx, "sb")
	if called || len(fe.files) != 0 {
		t.Fatalf("bootstrap must not run without an upstream")
	}
}

// TestRunSandboxBootstrapFailureDoesNotPanic: a failing setup job (or a failing
// Execute) must be swallowed (logged), never propagated — sandbox creation must
// not fail because the bootstrap did.
func TestRunSandboxBootstrapFailureDoesNotPanic(t *testing.T) {
	cs := fake.NewSimpleClientset()
	sbx := sandboxmgr.NewWithClientset(cs, sandboxmgr.Config{Namespace: "worker"})
	ctx := context.Background()
	if _, _, err := sbx.Create(ctx, sandboxmgr.Spec{Name: "sb", Image: "img:1"}); err != nil {
		t.Fatal(err)
	}

	// Every write fails.
	bad := &fakeExecutor{writeErr: errors.New("worker unreachable")}
	s := New(Deps{
		Sandbox: sbx, Runtime: runtimeprofiles.DefaultSettings(),
		SandboxPackageUpstream: "http://artifact",
		NewWorker:              func(_, _ string) workerExecutor { return bad },
	})
	s.runSandboxBootstrap(ctx, "sb") // must not panic
}
