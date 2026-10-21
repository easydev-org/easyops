package imagebuild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// tarGz builds a gzip'd tar with a single top-level dir (Forgejo style).
func tarGz(t *testing.T, top string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		p := top + "/" + name
		if err := tw.WriteHeader(&tar.Header{Name: p, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractStripsTopDir(t *testing.T) {
	dir, cleanup, err := Extract(tarGz(t, "repo-abc123", map[string]string{
		"Dockerfile":  "FROM scratch\n",
		"sub/main.go": "package main\n",
	}), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if b, err := os.ReadFile(filepath.Join(dir, "Dockerfile")); err != nil || string(b) != "FROM scratch\n" {
		t.Fatalf("Dockerfile = %q err=%v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "main.go")); err != nil {
		t.Fatalf("nested file missing: %v", err)
	}
}

// tarGzWithGlobalHeader prepends a real PAX global header, exactly as Forgejo's
// archive endpoint does (`pax_global_header`). Go's archive/tar SURFACES that
// header as the first entry, which used to be mistaken for the top dir name.
// The block is crafted by hand because tar.Writer refuses to emit a global
// header without PAXRecords.
func tarGzWithGlobalHeader(t *testing.T, top string, files map[string]string) []byte {
	t.Helper()
	var inner bytes.Buffer
	tw := tar.NewWriter(&inner)
	for name, body := range files {
		p := top + "/" + name
		if err := tw.WriteHeader(&tar.Header{Name: p, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	blk := make([]byte, 512)
	copy(blk[0:], "pax_global_header")
	copy(blk[100:], "0000644\x00") // mode
	copy(blk[108:], "0000000\x00") // uid
	copy(blk[116:], "0000000\x00") // gid
	copy(blk[124:], "00000000000\x00")
	copy(blk[136:], "00000000000\x00")
	for i := 148; i < 156; i++ {
		blk[i] = ' '
	}
	blk[156] = 'g' // TypeXGlobalHeader
	copy(blk[257:], "ustar\x0000")
	for i := 329; i < 337; i++ {
		blk[i] = ' '
	}
	var sum int
	for _, b := range blk {
		sum += int(b)
	}
	copy(blk[148:], []byte(fmt.Sprintf("%06o\x00 ", sum)))

	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	if _, err := gz.Write(blk); err != nil {
		t.Fatal(err)
	}
	if _, err := gz.Write(inner.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestExtractSkipsPaxGlobalHeader(t *testing.T) {
	dir, cleanup, err := Extract(tarGzWithGlobalHeader(t, "weixin-agent", map[string]string{
		"Dockerfile": "FROM scratch\n",
		"README.md":  "hi\n",
	}), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if b, err := os.ReadFile(filepath.Join(dir, "Dockerfile")); err != nil || string(b) != "FROM scratch\n" {
		t.Fatalf("Dockerfile at root = %q err=%v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "weixin-agent", "Dockerfile")); err == nil {
		t.Fatal("top dir was NOT stripped (pax header confused the stripper)")
	}
}

// A tar WITHOUT a wrapper dir (root-level files only) must NOT be stripped.
func TestExtractKeepsRootWhenNoWrapper(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: 13, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("FROM scratch\n")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	dir, cleanup, err := Extract(buf.Bytes(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if b, err := os.ReadFile(filepath.Join(dir, "Dockerfile")); err != nil || string(b) != "FROM scratch\n" {
		t.Fatalf("root Dockerfile lost: %q err=%v", b, err)
	}
}

func TestExtractRejectsOversize(t *testing.T) {
	_, cleanup, err := Extract(tarGz(t, "r", map[string]string{"big": "0123456789"}), 5)
	if err == nil {
		cleanup()
		t.Fatal("expected oversize error")
	}
}

func TestFullRef(t *testing.T) {
	b := &Builder{RegistryHost: "git.agent.svc.cluster.local/"}
	if got := b.FullRef("root/myimg", "v1"); got != "git.agent.svc.cluster.local/root/myimg:v1" {
		t.Fatalf("FullRef = %q", got)
	}
}

func TestBuildValidatesContext(t *testing.T) {
	b := &Builder{RegistryHost: "reg"}
	dir := t.TempDir()
	// Missing dockerfile.
	if _, err := b.Build(t.Context(), Request{ContextDir: dir, Repo: "org/x", Tag: "1"}); err == nil {
		t.Fatal("expected missing-dockerfile error")
	}
}

func TestImportValidatesInputs(t *testing.T) {
	b := &Builder{RegistryHost: "reg"}
	cases := []ImportRequest{
		{Repo: "org/x", Tag: "1"},                            // no source
		{Source: "docker.io/library/redis:7", Tag: "7"},      // no repo
		{Source: "docker.io/library/redis:7", Repo: "org/x"}, // no tag
	}
	for _, c := range cases {
		if _, err := b.Import(t.Context(), c); err == nil {
			t.Fatalf("expected validation error for %+v", c)
		}
	}
}

func TestRegistryHostOf(t *testing.T) {
	cases := map[string]string{
		"docker.io/library/redis:7":  "docker.io",
		"library/redis":              "docker.io",
		"redis:7":                    "docker.io",
		"ghcr.io/foo/bar:v1":         "ghcr.io",
		"localhost:5000/foo/bar:tag": "localhost:5000",
		"registry.example.com/x/y:1": "registry.example.com",
	}
	for in, want := range cases {
		if got := registryHostOf(in); got != want {
			t.Fatalf("registryHostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScheme(t *testing.T) {
	if got := (&Builder{}).scheme(); got != "https" {
		t.Fatalf("default scheme = %q", got)
	}
	if got := (&Builder{RegistryScheme: "http"}).scheme(); got != "http" {
		t.Fatalf("explicit scheme = %q", got)
	}
}

// The three tests below replace an older pair that exercised a REMOVED design:
// easyops used to shell out to a `buildctl` binary and hand it a merged
// DOCKER_CONFIG directory (Buildctl / dockerConfigAuth / mergeDockerConfig).
// The builder now links the BuildKit Go client and passes an in-memory auth
// provider (dockerConfigJSON), so the old assertions could not compile and,
// more importantly, were testing something that no longer exists.

// TestDockerConfigJSONCarriesTheDeploymentCreds: RegistryUser/RegistryPass must
// land under EVERY URL shape buildkit may look a host up by.
//
// WHY every shape: the auth provider is asked for a key the caller has in hand,
// and different code paths pass "host", "http://host", "https://host/v2/", ...
// A single key works for the push but misses the pull (or the reverse).
func TestDockerConfigJSONCarriesTheDeploymentCreds(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", "") // isolate from any ambient config
	b := &Builder{RegistryHost: "reg.example", RegistryUser: "root", RegistryPass: "tok"}

	raw, err := b.dockerConfigJSON()
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	want := base64.StdEncoding.EncodeToString([]byte("root:tok"))
	for _, key := range []string{
		"reg.example",
		"https://reg.example",
		"http://reg.example",
		"https://reg.example/v2/",
		"http://reg.example/v2/",
	} {
		if cfg.Auths[key].Auth != want {
			t.Errorf("auth for %q = %q, want the deployment creds", key, cfg.Auths[key].Auth)
		}
	}
}

// TestDockerConfigJSONOmitsEmptyCreds: with no configured credentials the auths
// map must stay EMPTY rather than carry an ":" blob, which buildkit would
// present as an anonymous login and turn a clean 401 into a confusing one.
func TestDockerConfigJSONOmitsEmptyCreds(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", "")
	b := &Builder{RegistryHost: "reg.example"}

	raw, err := b.dockerConfigJSON()
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Auths map[string]any `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Auths) != 0 {
		t.Errorf("auths = %v, want empty", cfg.Auths)
	}
}

// TestDockerConfigJSONKeepsAmbientAuths: a DOCKER_CONFIG on disk holds
// credentials for OTHER registries (an upstream mirror, a second registry).
// Those must survive, and the deployment's credential must WIN for its own host
// — it is the authoritative one.
func TestDockerConfigJSONKeepsAmbientAuths(t *testing.T) {
	dir := t.TempDir()
	ambient := base64.StdEncoding.EncodeToString([]byte("someone:else"))
	deployment := base64.StdEncoding.EncodeToString([]byte("root:tok"))
	cfg := `{"auths":{
		"ghcr.io":{"auth":"` + ambient + `"},
		"reg.example":{"auth":"` + ambient + `"}
	}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dir)

	b := &Builder{RegistryHost: "reg.example", RegistryUser: "root", RegistryPass: "tok"}
	raw, err := b.dockerConfigJSON()
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Auths["ghcr.io"].Auth != ambient {
		t.Error("an unrelated registry's ambient credential must be preserved")
	}
	if out.Auths["reg.example"].Auth != deployment {
		t.Errorf("the deployment credential must WIN for its own host, got %q",
			out.Auths["reg.example"].Auth)
	}
}
