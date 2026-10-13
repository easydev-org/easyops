package opssvc

import (
	"net/url"
	"strings"
)

// sandboxUpstreamEnv is the STANDARD-ENV subset of the package-manager upstream
// wiring. It points every tool that reads a standard environment variable at the
// deployment's artifact mirror (`base`, e.g.
// `http://artifact-pvc.worker.svc.cluster.local`), so a fresh sandbox can fetch
// packages with no per-session setup.
//
// This is the ENV half of the upstream wiring; the FILE half (apt/apk/rpm,
// maven, gradle, cargo, rubygems, composer, nuget, conda, git) is written by the
// sandbox bootstrap setup job (see bootstrap.go). The two are complementary:
// env-var-addressable tools are covered here, file-configured tools by the
// bootstrap. `ARTIFACT_UPSTREAM` is injected so the bootstrap can read one
// stable value and expand the file configs from it.
//
// The keys are the standard, well-known env names for each ecosystem; a caller
// can still override any of them (see mergeEnv).
func sandboxUpstreamEnv(base string) map[string]string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return nil
	}
	host := base
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		host = u.Host
	}
	return map[string]string{
		// A stable, tool-agnostic marker the bootstrap reads to write the
		// file-based configs (apt/apk/maven/gradle/cargo/nuget/rubygems/
		// composer/conda/git).
		"ARTIFACT_UPSTREAM": base,
		// Python (pip): the simple index + the plaintext host as a trusted host
		// (artifact is served over plain HTTP in-cluster).
		"PIP_INDEX_URL":    base + "/artifacts/pypi/simple/",
		"PIP_TRUSTED_HOST": host,
		// Node (npm): the registry (pull-through + hosted share one URL). npm
		// matches `npm_config_*` case-insensitively, so the upper-case form works.
		"NPM_CONFIG_REGISTRY": base + "/artifacts/npm/",
		// Go: module proxy + no checksum DB (the mirror is not sum.golang.org).
		"GOPROXY": base + "/artifacts/go",
		"GOSUMDB": "off",
		// Elixir (hex).
		"HEX_MIRROR": base + "/artifacts/hex/",
		// Dart (pub).
		"PUB_HOSTED_URL": base + "/artifacts/pub",
		// Julia (Pkg server).
		"JULIA_PKG_SERVER": base + "/artifacts/juliapkg",
		// Perl (cpanm mirror).
		"PERL_CPANM_OPT": "--mirror " + base + "/artifacts/cpan/",
	}
}

// mergeEnv merges `defaults` UNDER `caller`: an explicitly-set caller key always
// WINS, so a caller can override any injected default. With no defaults the
// caller map is returned unchanged (nil stays nil).
func mergeEnv(defaults, caller map[string]string) map[string]string {
	if len(defaults) == 0 {
		return caller
	}
	if len(caller) == 0 {
		return defaults
	}
	out := make(map[string]string, len(defaults)+len(caller))
	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range caller {
		out[k] = v
	}
	return out
}
