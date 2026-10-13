// Package imagebuild builds a container image from a repository Dockerfile
// using the cluster buildkitd, and pushes it to the deployment registry.
//
// It links the BuildKit GO CLIENT (github.com/moby/buildkit/client) and talks
// to buildkitd over its gRPC API directly — there is NO `buildctl` binary and
// no shelling out. The easyops image therefore needs no buildctl: the k8s
// client and the buildkit client are both Go libraries.
package imagebuild

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/tonistiigi/fsutil"

	"github.com/docker/cli/cli/config/types"
)

// Builder builds images via the buildkit Go client against a buildkitd address.
type Builder struct {
	// Addr is the buildkitd address, e.g. tcp://buildkitd.agent.svc.cluster.local:1234.
	Addr string
	// RegistryHost is the registry host, e.g. artifact.worker.svc.cluster.local.
	RegistryHost string
	// RegistryScheme is the scheme for the registry host ("https" default;
	// "http" for the in-cluster plaintext registry).
	RegistryScheme string
	// Timeout bounds a build (default 30m).
	Timeout time.Duration
	// MaxContextBytes caps the extracted context size (default 512 MiB).
	MaxContextBytes int64
	// RegistryUser/RegistryPass authenticate registry requests. The registry is
	// artifact; the credential is the shared artifact write token
	// (RegistryUser is usually empty, RegistryPass the token).
	RegistryUser string
	RegistryPass string
}

// Request is one build.
type Request struct {
	ContextDir string
	Dockerfile string
	Context    string
	Repo       string
	Tag        string
	BuildArgs  map[string]string
}

// Result is a completed build.
type Result struct {
	ImageRef string
	Log      string
}

// FullRef is the destination reference for a repo path + tag.
func (b *Builder) FullRef(repo, tag string) string {
	return strings.TrimRight(b.RegistryHost, "/") + "/" + strings.Trim(repo, "/") + ":" + tag
}

// scheme is the registry scheme (https unless explicitly set to http).
func (b *Builder) scheme() string {
	if b.RegistryScheme != "" {
		return b.RegistryScheme
	}
	return "https"
}

// dockerConfigJSON builds the registry auth config served by the buildkit auth
// session for the destination push (and any private sources).
func (b *Builder) dockerConfigJSON() ([]byte, error) {
	auths := map[string]any{}
	if host := b.RegistryHost; host != "" && (b.RegistryPass != "" || b.RegistryUser != "") {
		enc := base64.StdEncoding.EncodeToString([]byte(b.RegistryUser + ":" + b.RegistryPass))
		for _, key := range []string{host, "https://" + host, "http://" + host, "https://" + host + "/v1/", "https://" + host + "/v2/", "http://" + host + "/v2/"} {
			auths[key] = map[string]any{"auth": enc}
		}
	}
	if p := os.Getenv("DOCKER_CONFIG"); p != "" {
		if raw, err := os.ReadFile(filepath.Join(p, "config.json")); err == nil {
			var cfg struct {
				Auths map[string]any `json:"auths"`
			}
			if json.Unmarshal(raw, &cfg) == nil {
				for k, v := range cfg.Auths {
					if _, ok := auths[k]; !ok {
						auths[k] = v
					}
				}
			}
		}
	}
	return json.Marshal(map[string]any{"auths": auths})
}

// validate resolves the context/dockerfile dirs and the destination ref.
func (b *Builder) validate(req Request) (ref, ctxDir, dockerfileDir, filename string, err error) {
	if req.Repo == "" || req.Tag == "" {
		return "", "", "", "", errors.New("repo and tag required")
	}
	ctxDir = filepath.Join(req.ContextDir, filepath.FromSlash(strings.Trim(req.Context, "/")))
	if fi, e := os.Stat(ctxDir); e != nil || !fi.IsDir() {
		return "", "", "", "", fmt.Errorf("build context %q not found in repository", req.Context)
	}
	dockerfile := req.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	dockerfileDir = filepath.Join(req.ContextDir, filepath.Dir(filepath.FromSlash(strings.Trim(dockerfile, "/"))))
	filename = filepath.Base(filepath.FromSlash(dockerfile))
	if _, e := os.Stat(filepath.Join(req.ContextDir, filepath.FromSlash(dockerfile))); e != nil {
		return "", "", "", "", fmt.Errorf("dockerfile %q not found in repository", dockerfile)
	}
	ref = b.FullRef(req.Repo, req.Tag)
	return ref, ctxDir, dockerfileDir, filename, nil
}

// solve runs one buildkit Solve with the dockerfile frontend and streams the
// progress. `emit` (optional) receives log chunks as they are produced.
func (b *Builder) solve(ctx context.Context, req Request, emit func(string)) (string, string, error) {
	ref, ctxDir, dockerfileDir, filename, err := b.validate(req)
	if err != nil {
		return "", "", err
	}
	addr := b.Addr
	if addr == "" {
		addr = "tcp://buildkitd.agent.svc.cluster.local:1234"
	}
	timeout := b.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	bctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cli, err := bkclient.New(bctx, addr)
	if err != nil {
		return "", "", fmt.Errorf("buildkit dial %s: %w", addr, err)
	}
	defer cli.Close()

	cfg, err := b.dockerConfigJSON()
	if err != nil {
		return "", "", err
	}
	ctxFS, err := fsutil.NewFS(ctxDir)
	if err != nil {
		return "", "", fmt.Errorf("context fs: %w", err)
	}
	dfFS, err := fsutil.NewFS(dockerfileDir)
	if err != nil {
		return "", "", fmt.Errorf("dockerfile fs: %w", err)
	}
	ap := authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{
		AuthConfigProvider: dockerAuthFromJSON(cfg),
	})

	attrs := map[string]string{"filename": filename}
	for k, v := range req.BuildArgs {
		attrs["build-arg:"+k] = v
	}
	expAttrs := map[string]string{"name": ref, "push": "true"}
	if b.scheme() == "http" {
		// The in-cluster registry is plaintext HTTP.
		expAttrs["registry.insecure"] = "true"
	}

	ch := make(chan *bkclient.SolveStatus)
	var mu sync.Mutex
	var logBuf strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		for st := range ch {
			for _, v := range st.Vertexes {
				if v.Error != "" {
					mu.Lock()
					logBuf.WriteString(v.Error + "\n")
					mu.Unlock()
				}
			}
			for _, l := range st.Logs {
				mu.Lock()
				logBuf.Write(l.Data)
				mu.Unlock()
				if emit != nil {
					emit(string(l.Data))
				}
			}
		}
	}()

	_, serr := cli.Solve(bctx, nil, bkclient.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: attrs,
		LocalMounts: map[string]fsutil.FS{
			"context":    ctxFS,
			"dockerfile": dfFS,
		},
		Exports: []bkclient.ExportEntry{{
			Type:  "image",
			Attrs: expAttrs,
		}},
		Session: []session.Attachable{ap},
	}, ch)
	<-done
	if serr != nil {
		mu.Lock()
		defer mu.Unlock()
		return ref, "", fmt.Errorf("buildkit solve: %w\n%s", serr, tailBytes(logBuf.String(), 16*1024))
	}
	mu.Lock()
	defer mu.Unlock()
	return ref, tailBytes(logBuf.String(), 16*1024), nil
}

// dockerAuthFromJSON returns an AuthConfigProvider backed by raw docker-config
// JSON ({"auths":{host:{"auth":"base64(user:pass)"|"username"/"password"}}}).
func dockerAuthFromJSON(raw []byte) authprovider.AuthConfigProvider {
	return func(_ context.Context, host string, _ []string, _ authprovider.ExpireCachedAuthCheck) (types.AuthConfig, error) {
		var cfg struct {
			Auths map[string]struct {
				Auth     string `json:"auth"`
				Username string `json:"username"`
				Password string `json:"password"`
			} `json:"auths"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return types.AuthConfig{}, nil
		}
		for _, k := range []string{host, "https://" + host, "http://" + host, "https://" + host + "/v1/", "https://" + host + "/v2/", "http://" + host + "/v2/"} {
			a, ok := cfg.Auths[k]
			if !ok {
				continue
			}
			if a.Username != "" || a.Password != "" {
				return types.AuthConfig{Username: a.Username, Password: a.Password}, nil
			}
			if a.Auth != "" {
				if u, p, ok := decodeBasic(a.Auth); ok {
					return types.AuthConfig{Username: u, Password: p}, nil
				}
			}
		}
		return types.AuthConfig{}, nil
	}
}

func decodeBasic(s string) (string, string, bool) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", "", false
	}
	i := strings.IndexByte(string(b), ':')
	if i < 0 {
		return "", "", false
	}
	return string(b[:i]), string(b[i+1:]), true
}

// Build runs the build and pushes the image.
func (b *Builder) Build(ctx context.Context, req Request) (Result, error) {
	ref, log, err := b.solve(ctx, req, nil)
	if err != nil {
		return Result{}, err
	}
	return Result{ImageRef: ref, Log: log}, nil
}

// BuildStream runs the build, streaming output to `emit`.
func (b *Builder) BuildStream(ctx context.Context, req Request, emit func(chunk string)) (string, error) {
	ref, _, err := b.solve(ctx, req, emit)
	return ref, err
}

// ImportRequest mirrors one upstream image into the deployment registry.
type ImportRequest struct {
	Source    string
	Repo      string
	Tag       string
	AuthUser  string
	AuthToken string
}

// Import re-serves an upstream image under the deployment registry with a no-op
// `FROM <Source>` build, preserving the upstream config.
func (b *Builder) Import(ctx context.Context, req ImportRequest) (Result, error) {
	if req.Source == "" || req.Repo == "" || req.Tag == "" {
		return Result{}, errors.New("source, repo and tag required")
	}
	ref := b.FullRef(req.Repo, req.Tag)
	dir, err := os.MkdirTemp("", "import-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)
	cf := "ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(cf), 0o644); err != nil {
		return Result{}, err
	}
	if req.AuthUser != "" || req.AuthToken != "" {
		host := registryHostOf(req.Source)
		enc := base64.StdEncoding.EncodeToString([]byte(req.AuthUser + ":" + req.AuthToken))
		cfg := map[string]any{"auths": map[string]any{host: map[string]any{"auth": enc}}}
		raw, _ := json.Marshal(cfg)
		tmp, _ := os.MkdirTemp("", "dockerconfig-")
		_ = os.WriteFile(filepath.Join(tmp, "config.json"), raw, 0o600)
		defer os.RemoveAll(tmp)
		old := os.Getenv("DOCKER_CONFIG")
		_ = os.Setenv("DOCKER_CONFIG", tmp)
		defer os.Setenv("DOCKER_CONFIG", old)
	}
	if _, _, err := b.solve(ctx, Request{
		ContextDir: dir, Dockerfile: "Dockerfile", Repo: req.Repo, Tag: req.Tag,
		BuildArgs: map[string]string{"BASE_IMAGE": req.Source},
	}, nil); err != nil {
		return Result{}, err
	}
	return Result{ImageRef: ref}, nil
}

// registryHostOf derives the registry host from an image ref.
func registryHostOf(ref string) string {
	slash := strings.Index(ref, "/")
	if slash < 0 {
		return "docker.io"
	}
	first := ref[:slash]
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return first
	}
	return "docker.io"
}

// tail returns the last n bytes of s.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
