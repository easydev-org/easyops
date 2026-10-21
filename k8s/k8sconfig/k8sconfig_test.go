package k8sconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeKubeconfig writes a minimal, valid kubeconfig pointing at `server` and
// returns its path.
func writeKubeconfig(t *testing.T, server string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")
	content := `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: ` + server + `
contexts:
- name: ctx
  context:
    cluster: c
    user: u
current-context: ctx
users:
- name: u
  user:
    token: test-token
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestEASYOPSKubeconfigUsed checks that EASYOPS_KUBECONFIG is used and its
// server/host is reflected in the returned config.
func TestEASYOPSKubeconfigUsed(t *testing.T) {
	path := writeKubeconfig(t, "https://easyops-kubeconfig.example:6443")
	t.Setenv("EASYOPS_KUBECONFIG", path)
	t.Setenv("KUBECONFIG", "")

	rc, err := RESTConfig()
	if err != nil {
		t.Fatalf("RESTConfig: %v", err)
	}
	if rc.Host != "https://easyops-kubeconfig.example:6443" {
		t.Fatalf("host = %q, want the kubeconfig server", rc.Host)
	}
	if rc.BearerToken != "test-token" {
		t.Fatalf("token = %q, want test-token", rc.BearerToken)
	}
}

// TestKUBECONFIGIsIgnored checks that an ambient KUBECONFIG is NOT consulted:
// with EASYOPS_KUBECONFIG unset, a perfectly valid KUBECONFIG still yields an
// error (there is no KUBECONFIG fallback).
func TestKUBECONFIGIsIgnored(t *testing.T) {
	path := writeKubeconfig(t, "https://kubeconfig-ignored.example:6443")
	t.Setenv("EASYOPS_KUBECONFIG", "")
	t.Setenv("KUBECONFIG", path)

	rc, err := RESTConfig()
	if err == nil {
		t.Fatalf("expected an error when only KUBECONFIG is set, got config %+v", rc)
	}
	if !strings.Contains(err.Error(), "EASYOPS_KUBECONFIG") {
		t.Fatalf("error must name EASYOPS_KUBECONFIG, got: %s", err)
	}
}

// TestNoKubeconfigIsAnError checks that with EASYOPS_KUBECONFIG unset RESTConfig
// errors out — there is NO in-cluster ServiceAccount fallback either, even when
// the in-cluster environment looks available.
func TestNoKubeconfigIsAnError(t *testing.T) {
	t.Setenv("EASYOPS_KUBECONFIG", "")
	t.Setenv("KUBECONFIG", "")
	// Pretend the in-cluster env is present so a fallback WOULD have been tried.
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")

	rc, err := RESTConfig()
	if err == nil {
		t.Fatalf("expected an error with no EASYOPS_KUBECONFIG, got config %+v", rc)
	}
	if !strings.Contains(err.Error(), "requires an explicit kubeconfig") {
		t.Fatalf("error must explain a kubeconfig is required, got: %s", err)
	}
}

// TestEASYOPSKubeconfigUnusable checks that an explicit EASYOPS_KUBECONFIG that
// cannot be loaded returns an error naming the source.
func TestEASYOPSKubeconfigUnusable(t *testing.T) {
	t.Setenv("EASYOPS_KUBECONFIG", filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("KUBECONFIG", "")

	rc, err := RESTConfig()
	if err == nil {
		t.Fatalf("expected an error for an unusable explicit kubeconfig, got config %+v", rc)
	}
	if !strings.Contains(err.Error(), "EASYOPS_KUBECONFIG") {
		t.Fatalf("error must name the source, got: %s", err)
	}
}

// TestSource checks the read-only source description: EASYOPS_KUBECONFIG, else
// "none" (KUBECONFIG is not reported because it is not consulted).
func TestSource(t *testing.T) {
	t.Run("EASYOPS_KUBECONFIG", func(t *testing.T) {
		t.Setenv("EASYOPS_KUBECONFIG", "/etc/easyops/kubeconfig")
		t.Setenv("KUBECONFIG", "/tmp/other")
		if got := Source(); got != "EASYOPS_KUBECONFIG=/etc/easyops/kubeconfig" {
			t.Fatalf("Source() = %q", got)
		}
	})
	t.Run("none ignores KUBECONFIG", func(t *testing.T) {
		t.Setenv("EASYOPS_KUBECONFIG", "")
		t.Setenv("KUBECONFIG", "/tmp/other")
		if got := Source(); got != "none" {
			t.Fatalf("Source() = %q, want none (KUBECONFIG must be ignored)", got)
		}
	})
}
