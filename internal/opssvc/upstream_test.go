package opssvc

import (
	"strings"
	"testing"
)

// TestSandboxUpstreamEnv: with an upstream configured, the standard-env subset
// is produced, trimmed, and carries the parsed host as PIP_TRUSTED_HOST.
func TestSandboxUpstreamEnv(t *testing.T) {
	env := sandboxUpstreamEnv("http://artifact-pvc.worker.svc.cluster.local/")
	if env == nil {
		t.Fatal("expected env for a configured upstream")
	}
	want := map[string]string{
		"ARTIFACT_UPSTREAM":   "http://artifact-pvc.worker.svc.cluster.local",
		"PIP_INDEX_URL":       "http://artifact-pvc.worker.svc.cluster.local/artifacts/pypi/simple/",
		"PIP_TRUSTED_HOST":    "artifact-pvc.worker.svc.cluster.local",
		"NPM_CONFIG_REGISTRY": "http://artifact-pvc.worker.svc.cluster.local/artifacts/npm/",
		"GOPROXY":             "http://artifact-pvc.worker.svc.cluster.local/artifacts/go",
		"GOSUMDB":             "off",
		"HEX_MIRROR":          "http://artifact-pvc.worker.svc.cluster.local/artifacts/hex/",
		"PUB_HOSTED_URL":      "http://artifact-pvc.worker.svc.cluster.local/artifacts/pub",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, env[k], v)
		}
	}
}

// TestSandboxUpstreamEnvEmpty: no upstream configured -> nothing injected.
func TestSandboxUpstreamEnvEmpty(t *testing.T) {
	if env := sandboxUpstreamEnv(""); env != nil {
		t.Fatalf("empty upstream must yield nil, got %v", env)
	}
	if env := sandboxUpstreamEnv("   "); env != nil {
		t.Fatalf("blank upstream must yield nil, got %v", env)
	}
}

// TestMergeEnv: the caller's explicit keys WIN over the injected defaults; unset
// defaults are still added; no defaults is a no-op (nil stays nil).
func TestMergeEnv(t *testing.T) {
	defaults := map[string]string{"GOPROXY": "http://artifact/artifacts/go", "GOSUMDB": "off"}
	got := mergeEnv(defaults, map[string]string{"GOPROXY": "https://proxy.golang.org", "MY_VAR": "1"})
	if got["GOPROXY"] != "https://proxy.golang.org" {
		t.Errorf("caller GOPROXY must win, got %q", got["GOPROXY"])
	}
	if got["MY_VAR"] != "1" {
		t.Errorf("caller-only var lost: %v", got)
	}
	if got["GOSUMDB"] != "off" {
		t.Errorf("default GOSUMDB missing: %v", got)
	}

	if got := mergeEnv(defaults, nil); got["GOSUMDB"] != "off" {
		t.Errorf("nil caller must still get the defaults: %v", got)
	}
	if got := mergeEnv(nil, nil); got != nil {
		t.Errorf("no defaults + nil caller must stay nil, got %v", got)
	}
	caller := map[string]string{"A": "b"}
	if got := mergeEnv(nil, caller); got["A"] != "b" || len(got) != 1 {
		t.Errorf("no defaults must not inject anything, got %v", got)
	}
}

// TestBootstrapFilesCoverAllEcosystems checks the file configs point every
// file-type tool at the base and cover the full artifact protocol set.
func TestBootstrapFilesCoverAllEcosystems(t *testing.T) {
	const base = "http://artifact.worker.svc.cluster.local"
	files := map[string]string{}
	for _, f := range bootstrapFilesFor("linux", "/root", base) {
		files[f.Path] = f.Content
	}
	for _, want := range []string{
		"/etc/apt/sources.list.d/artifact.list",
		"/etc/apk/repositories",
		"/root/.m2/settings.xml",
		"/root/.gradle/init.gradle",
		"/root/.cargo/config.toml",
		"/root/.gemrc",
		"/root/.composer/config.json",
		"/root/.nuget/NuGet/NuGet.Config",
		"/root/.condarc",
		"/root/.config/pip/pip.conf",
		"/root/.npmrc",
		"/root/.gitconfig",
		"/etc/yum.repos.d/artifact.repo",
		"/root/.cabal/config",
		"/root/.stack/config.yaml",
		"/root/.config/nix/nix.conf",
		"/root/.Rprofile",
		"/root/.luarocks/config-5.4.lua",
		"/root/.sbt/repositories",
		"/root/.bazelrc",
		"/root/.swiftpm/configuration/mirrors.json",
	} {
		if _, ok := files[want]; !ok {
			t.Errorf("bootstrap missing file %q", want)
		}
	}
	// Every file must point at the artifact base, EXCEPT the two apt-source
	// SUPPRESSION files (comment lines that disable the image's own Debian
	// sources so apt never reaches the public internet).
	suppress := map[string]bool{
		"/etc/apt/sources.list":                  true,
		"/etc/apt/sources.list.d/debian.sources": true,
	}
	for p, c := range files {
		if suppress[p] {
			continue
		}
		if !strings.Contains(c, base) {
			t.Errorf("file %q does not reference the base", p)
		}
	}
	// The NAMED maven targets were removed by artifact MR #40: upstream mirrors
	// go through /artifacts/maven/_upstream/<host>/.
	for _, banned := range []string{"/artifacts/maven.google/", "/artifacts/maven.gradle/"} {
		for p, c := range files {
			if strings.Contains(c, banned) {
				t.Errorf("file %q must NOT reference the removed named target %q", p, banned)
			}
		}
	}
}

// TestBootstrapFilesPerOS: macOS/Windows get OS-specific paths (guest files),
// all pointing at the base.
func TestBootstrapFilesPerOS(t *testing.T) {
	const base = "http://artifact.worker.svc.cluster.local"
	cases := map[string]string{
		"darwin":  "/Users/docker/.npmrc",
		"windows": `C:\Users\Docker\.gitconfig`,
	}
	for os, wantPath := range cases {
		files := map[string]string{}
		for _, f := range bootstrapFilesFor(os, "", base) {
			files[f.Path] = f.Content
		}
		if _, ok := files[wantPath]; !ok {
			t.Errorf("%s: expected %q among %v", os, wantPath, keys(files))
		}
		for p, c := range files {
			if !strings.Contains(c, base) {
				t.Errorf("%s: file %q does not reference the base", os, p)
			}
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestBootstrapEnabled checks the enable rules: needs an upstream AND the flag.
func TestBootstrapEnabled(t *testing.T) {
	on := &Service{sandboxPackageUpstream: "http://artifact", sandboxBootstrap: true}
	if !on.bootstrapEnabled() {
		t.Error("upstream + bootstrap-on must be enabled")
	}
	off := &Service{sandboxPackageUpstream: "http://artifact", sandboxBootstrap: false}
	if off.bootstrapEnabled() {
		t.Error("SANDBOX_BOOTSTRAP=off must disable it")
	}
	none := &Service{sandboxBootstrap: true}
	if none.bootstrapEnabled() {
		t.Error("no upstream must disable it (pre-upstream behaviour)")
	}
}
