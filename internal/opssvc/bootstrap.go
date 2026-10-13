package opssvc

import (
	"context"
	"log"
	"strings"
	"time"
)

// Bootstrap tuning. Writing the file configs is a handful of file writes; but a
// VM sandbox (macOS/Windows) boots in MINUTES, so the bounded wait for the guest
// worker to answer must be generous. Linux images answer immediately.
const bootstrapWaitTimeoutMs = 300_000 // 5m: bounded wait for the worker to serve

// workerExecutor is the worker surface the sandbox bootstrap needs. It is
// satisfied by *WorkerClient; it is an interface so the bootstrap is testable
// with a fake and so the library layer never imports the worker protocol.
type workerExecutor interface {
	WriteFile(ctx context.Context, path string, content []byte) error
	Info(ctx context.Context) (workerInfo, error)
}

// workerInfo is the subset of the worker's Info the bootstrap needs.
type workerInfo struct {
	OS   string
	Home string
}

// bootstrapFile is one package-manager config file the bootstrap writes.
type bootstrapFile struct {
	Path    string
	Content string
}

// bootstrapFilesFor returns the FILE-type package-manager configs for the
// sandbox worker's OS, all pointing at the artifact mirror `base`. They are
// written directly via the worker's file service (no shell), so they work on
// any image. Paths are ABSOLUTE (the worker resolves relative paths under its
// workspace; config dirs live outside it).
func bootstrapFilesFor(os, home, base string) []bootstrapFile {
	switch os {
	case "darwin":
		return darwinBootstrapFiles(orDefault(home, "/Users/docker"), base)
	case "windows":
		return windowsBootstrapFiles(orDefault(home, `C:\Users\Docker`), base)
	default:
		return linuxBootstrapFiles(orDefault(home, "/root"), base)
	}
}

// linuxBootstrapFiles is the full artifact-protocol set for Linux sandboxes.
func linuxBootstrapFiles(h, base string) []bootstrapFile {
	return []bootstrapFile{
		// apt (Debian): ONLY the artifact mirror.
		{"/etc/apt/sources.list.d/artifact.list", "deb [trusted=yes] " + base + "/artifacts/debian/debian trixie main\n"},
		// apk (Alpine 3.24).
		{"/etc/apk/repositories", base + "/artifacts/apk/v3.24/main\n"},
		// maven (java).
		{h + "/.m2/settings.xml", "<settings><mirrors><mirror><id>artifact</id><mirrorOf>*</mirrorOf><url>" + base + "/artifacts/maven/</url></mirror></mirrors></settings>"},
		// gradle: Google + Gradle-Plugin-Portal BEFORE Central (shared cache).
		{h + "/.gradle/init.gradle", "allprojects{repositories{clear();" +
			"maven{url \"" + base + "/artifacts/maven/_upstream/dl.google.com/\"; allowInsecureProtocol = true};" +
			"maven{url \"" + base + "/artifacts/maven/_upstream/plugins.gradle.org/\"; allowInsecureProtocol = true};" +
			"maven{url \"" + base + "/artifacts/maven/\"; allowInsecureProtocol = true}}}\n" +
			"settingsEvaluated{s->s.pluginManagement{repositories{clear();" +
			"maven{url \"" + base + "/artifacts/maven/_upstream/dl.google.com/\"; allowInsecureProtocol = true};" +
			"maven{url \"" + base + "/artifacts/maven/_upstream/plugins.gradle.org/\"; allowInsecureProtocol = true};" +
			"maven{url \"" + base + "/artifacts/maven/\"; allowInsecureProtocol = true}}}}"},
		// cargo (rust).
		{h + "/.cargo/config.toml", "[source.crates-io]\nreplace-with=\"artifact\"\n[source.artifact]\nregistry=\"sparse+" + base + "/artifacts/cargo/index/\"\n"},
		// rubygems.
		{h + "/.gemrc", "---\n:sources:\n- " + base + "/artifacts/rubygems/\n"},
		// composer.
		{h + "/.composer/config.json", "{\"repositories\":{\"packagist\":{\"type\":\"composer\",\"url\":\"" + base + "/artifacts/composer/\"}}}"},
		// nuget.
		{h + "/.nuget/NuGet/NuGet.Config", "<?xml version=\"1.0\"?><configuration><packageSources><clear/><add key=\"artifact\" value=\"" + base + "/artifacts/nuget/v3/index.json\"/></packageSources></configuration>"},
		// conda.
		{h + "/.condarc", "channels:\n  - " + base + "/artifacts/conda/pkgs/main\n"},
		// pip (file form, in addition to the env var).
		{h + "/.config/pip/pip.conf", "[global]\nindex-url = " + base + "/artifacts/pypi/simple/\ntrusted-host = " + hostOf(base) + "\n"},
		// npm (file form).
		{h + "/.npmrc", "registry=" + base + "/artifacts/npm/\n@jsr:registry=" + base + "/artifacts/jsr/\n"},
		// git: rewrite github.com to artifact's git mirror.
		{h + "/.gitconfig", "[url \"" + base + "/artifacts/git/github.com/\"]\n\tinsteadOf = https://github.com/\n"},
		// --- additional artifact-supported ecosystems ---
		// rpm (dnf/yum).
		{"/etc/yum.repos.d/artifact.repo", "[artifact]\nname=artifact\nbaseurl=" + base + "/artifacts/rpm/pub/fedora/linux/releases/43/Everything/x86_64/os/\nenabled=1\ngpgcheck=0\n"},
		// haskell (hackage).
		{h + "/.cabal/config", "repository artifact\n  url: " + base + "/artifacts/hackage/\n  secure: False\n"},
		{h + "/.stack/config.yaml", "package-indices:\n- name: artifact\n  download-prefix: " + base + "/artifacts/hackage/\n  http: " + base + "/artifacts/hackage/\n"},
		// nix.
		{h + "/.config/nix/nix.conf", "substituters = " + base + "/artifacts/nix/ https://cache.nixos.org\n"},
		// R (cran).
		{h + "/.Rprofile", "options(repos = c(CRAN = \"" + base + "/artifacts/cran/\"))\n"},
		// luarocks.
		{h + "/.luarocks/config-5.4.lua", "rocks_servers = { \"" + base + "/artifacts/luarocks/\" }\n"},
		// sbt / ivy.
		{h + "/.sbt/repositories", "[repositories]\n  artifact: " + base + "/artifacts/ivy/\n"},
		// bazel.
		{h + "/.bazelrc", "build --registry=" + base + "/artifacts/bazel/\n"},
		// Swift Package Manager uses libgit2 (NOT the git CLI), so the git
		// insteadOf rewrite does NOT apply. SwiftPM's own mirror config does — but
		// it is per-repository (no wildcard), so seed the common apple/* ones.
		{h + "/.swiftpm/configuration/mirrors.json", `{"version":1,"object":[` +
			`{"original":"https://github.com/apple/swift-argument-parser.git","mirror":"` + base + `/artifacts/git/github.com/apple/swift-argument-parser.git"},` +
			`{"original":"https://github.com/apple/swift-log.git","mirror":"` + base + `/artifacts/git/github.com/apple/swift-log.git"},` +
			`{"original":"https://github.com/apple/swift-nio.git","mirror":"` + base + `/artifacts/git/github.com/apple/swift-nio.git"},` +
			`{"original":"https://github.com/apple/swift-collections.git","mirror":"` + base + `/artifacts/git/github.com/apple/swift-collections.git"},` +
			`{"original":"https://github.com/apple/swift-crypto.git","mirror":"` + base + `/artifacts/git/github.com/apple/swift-crypto.git"},` +
			`{"original":"https://github.com/apple/swift-syntax.git","mirror":"` + base + `/artifacts/git/github.com/apple/swift-syntax.git"},` +
			`{"original":"https://github.com/apple/swift-atomics.git","mirror":"` + base + `/artifacts/git/github.com/apple/swift-atomics.git"},` +
			`{"original":"https://github.com/apple/swift-system.git","mirror":"` + base + `/artifacts/git/github.com/apple/swift-system.git"}]}`},
		// apt: DISABLE the image's own Debian sources so apt never reaches the
		// public internet (the worker can write but not delete; a comment line
		// disables a sources file).
		{"/etc/apt/sources.list", "# artifact-only: see /etc/apt/sources.list.d/artifact.list\n"},
		{"/etc/apt/sources.list.d/debian.sources", "# artifact-only: disabled (was deb.debian.org)\n"},
	}
}

// darwinBootstrapFiles is the macOS-guest set. Paths are the guest's (the worker
// runs IN the macOS guest). Homebrew's git remote is retargeted to the artifact
// git mirror by a `brew` wrapper the guest uses (see the image); here we cover
// the config-FILE tools that read well-known paths.
func darwinBootstrapFiles(h, base string) []bootstrapFile {
	return []bootstrapFile{
		// git: rewrite github.com to artifact's git mirror (also fixes brew's taps).
		{h + "/.gitconfig", "[url \"" + base + "/artifacts/git/github.com/\"]\n\tinsteadOf = https://github.com/\n"},
		// npm.
		{h + "/.npmrc", "registry=" + base + "/artifacts/npm/\n@jsr:registry=" + base + "/artifacts/jsr/\n"},
		// pip.
		{h + "/.config/pip/pip.conf", "[global]\nindex-url = " + base + "/artifacts/pypi/simple/\ntrusted-host = " + hostOf(base) + "\n"},
		// rubygems.
		{h + "/.gemrc", "---\n:sources:\n- " + base + "/artifacts/rubygems/\n"},
		// cargo.
		{h + "/.cargo/config.toml", "[source.crates-io]\nreplace-with=\"artifact\"\n[source.artifact]\nregistry=\"sparse+" + base + "/artifacts/cargo/index/\"\n"},
		// Homebrew: point the tap git remote at artifact (best-effort; brew
		// honors the global git insteadOf above for github.com taps).
		{h + "/.brew-artifact-upstream", base + "/artifacts/git/github.com/\n"},
	}
}

// windowsBootstrapFiles is the Windows-guest set (paths are Windows; the worker
// runs IN the Windows guest). %APPDATA% is `h\AppData\Roaming`.
func windowsBootstrapFiles(h, base string) []bootstrapFile {
	appdata := h + `\AppData\Roaming`
	return []bootstrapFile{
		// git.
		{h + `\.gitconfig`, "[url \"" + base + "/artifacts/git/github.com/\"]\n\tinsteadOf = https://github.com/\n"},
		// npm (userconfig).
		{appdata + `\npm\npmrc`, "registry=" + base + "/artifacts/npm/\n@jsr:registry=" + base + "/artifacts/jsr/\n"},
		// pip.
		{appdata + `\pip\pip.ini`, "[global]\nindex-url = " + base + "/artifacts/pypi/simple/\ntrusted-host = " + hostOf(base) + "\n"},
		// nuget.
		{appdata + `\NuGet\NuGet.Config`, "<?xml version=\"1.0\"?><configuration><packageSources><clear/><add key=\"artifact\" value=\"" + base + "/artifacts/nuget/v3/index.json\"/></packageSources></configuration>"},
		// cargo.
		{h + `\.cargo\config.toml`, "[source.crates-io]\nreplace-with=\"artifact\"\n[source.artifact]\nregistry=\"sparse+" + base + "/artifacts/cargo/index/\"\n"},
		// conda.
		{h + `\.condarc`, "channels:\n  - " + base + "/artifacts/conda/pkgs/main\n"},
	}
}

// orDefault returns s or def when s is empty.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// hostOf returns the host:port of a URL-ish base (for PIP_TRUSTED_HOST).
func hostOf(base string) string {
	s := base
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/"); i >= 0 {
		s = s[:i]
	}
	return s
}

// bootstrapEnabled reports whether the file-type bootstrap should run: it needs
// an upstream to point at, and is ON by default; SANDBOX_BOOTSTRAP=off disables
// it. With no upstream configured it never runs (pre-upstream behaviour).
func (s *Service) bootstrapEnabled() bool {
	return s.sandboxBootstrap && strings.TrimSpace(s.sandboxPackageUpstream) != ""
}

// runSandboxBootstrap runs the package-source setup job on a freshly created
// sandbox. It is BEST-EFFORT: any failure is logged as a warning and never
// fails sandbox creation (a sandbox without the bootstrap still works, just
// without the file-configured mirrors). It is safe to call on a context that is
// detached from the caller's request (see CreateSandbox).
func (s *Service) runSandboxBootstrap(ctx context.Context, name string) {
	if !s.bootstrapEnabled() {
		return
	}
	base := strings.TrimRight(strings.TrimSpace(s.sandboxPackageUpstream), "/")
	url, token, err := s.sbx.Resolve(ctx, name)
	if err != nil {
		log.Printf("warn: sandbox bootstrap %q: resolve worker: %v", name, err)
		return
	}
	if s.newWorker == nil {
		log.Printf("warn: sandbox bootstrap %q: no worker client configured", name)
		return
	}
	worker := s.newWorker(url, token)

	// VMs (macOS/Windows) boot in MINUTES; wait for the guest worker to answer
	// before writing. Best-effort: on timeout we fall back to the base set.
	info, err := waitForWorker(ctx, worker)
	if err != nil {
		log.Printf("warn: sandbox bootstrap %q: worker not reachable: %v", name, err)
		return
	}

	// Write the file configs DIRECTLY via the worker's file service — no shell
	// job. This avoids the worker's interpreter entirely (works on scratch
	// images) and has no job lifecycle to poll. Idempotent (overwrite), and
	// best-effort per file: a single failure is logged, not fatal.
	failed := 0
	for _, f := range bootstrapFilesFor(info.OS, info.Home, base) {
		if err := worker.WriteFile(ctx, f.Path, []byte(f.Content)); err != nil {
			log.Printf("warn: sandbox bootstrap %q: write %s: %v", name, f.Path, err)
			failed++
		}
	}
	if failed == 0 {
		log.Printf("sandbox bootstrap %q: package sources point at %s (os=%s)", name, base, info.OS)
	} else {
		log.Printf("warn: sandbox bootstrap %q: %d file(s) failed to write", name, failed)
	}
}

// waitForWorker polls the worker's Info until it answers (the sandbox is up and
// the worker is serving). VMs take minutes to boot; Linux images answer at once.
// The overall wait is bounded by the bootstrap context deadline.
func waitForWorker(ctx context.Context, worker workerExecutor) (workerInfo, error) {
	var lastErr error
	for {
		info, err := worker.Info(ctx)
		if err == nil {
			return info, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return workerInfo{}, lastErr
		case <-time.After(3 * time.Second):
		}
	}
}

// bootstrapContext returns a bounded context for the (fire-and-forget) bootstrap
// that is detached from the request context, so it survives the RPC returning.
func bootstrapContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx := context.WithoutCancel(parent)
	return context.WithTimeout(ctx, time.Duration(bootstrapWaitTimeoutMs)*time.Millisecond+10*time.Second)
}
