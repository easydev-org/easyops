// Package imagebuild builds a container image from a repository Dockerfile
// using the cluster buildkitd, and pushes it to the deployment registry.
//
// It links the BuildKit GO CLIENT (github.com/moby/buildkit/client) and talks
// to buildkitd over its gRPC API directly — there is NO `buildctl` binary and
// no shelling out. The easyops image therefore needs no buildctl: the k8s
// client and the buildkit client are both Go libraries.
package imagebuild

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	// Cache enables registry cache export/import, which is what makes the cache
	// survive a buildkitd restart (or a different daemon picking up the build).
	// The daemon's local cache alone dies with its PVC.
	Cache CacheOptions

	// Output selects what the build PRODUCES:
	//
	//	"image" (default) - push an image to the registry.
	//	"files"           - export a FILE TREE instead of an image.
	//
	// "files" is for artifacts that are not containers: a compiled binary, a
	// frontend dist/, a test report. The Dockerfile names the stage to export
	// via OutputStage (conventionally "export"):
	//
	//	FROM golang:1.26 AS build
	//	RUN go build -o /out/app ./cmd/app
	//	FROM scratch AS export
	//	COPY --from=build /out /
	//
	// The bytes arrive in THIS process: buildkit's local exporter streams the
	// tree through a session filesync, so the client receives it - nothing is
	// written to the daemon's disk.
	Output string
	// OutputStage is the stage to export when Output=files (default "export").
	OutputStage string
	// MaxOutputBytes caps the exported tree (0 = MaxFilesBytes).
	MaxOutputBytes int64
}

// Build outputs.
const (
	OutputImage = "image"
	OutputFiles = "files"
)

// MaxFilesBytes is the default cap on an exported file tree (128 MiB). The tree
// is assembled in memory because it is handed back over the RPC; a bigger
// artifact belongs in a registry (as an image) or in blob storage.
const MaxFilesBytes = 128 << 20

// CacheOptions controls registry cache import/export for one build.
//
// WHY this exists: `--mount=type=cache` directories (go/pkg/mod, npm) are NOT
// part of an exported cache — they live only inside the daemon while it runs.
// What CAN be exported is every intermediate LAYER (`mode=max`), so
// `RUN go mod download` still hits after the daemon is replaced; only the
// unpacking of those modules into a cache dir is repeated. Exporting also lets
// several daemons share one cache, and lets a fresh daemon warm up from the
// registry instead of rebuilding from scratch.
type CacheOptions struct {
	// Enabled turns cache import AND export on.
	Enabled bool
	// Repo is the registry repository the cache is stored in, e.g.
	// "easyagent/cache". Empty derives "<image repo>-cache".
	Repo string
	// Tag is the cache tag. Empty derives the image tag.
	Tag string
	// ImportOnly skips the export (useful for read-only builds).
	ImportOnly bool
	// Mode is "max" (all intermediate layers, default) or "min" (final only).
	Mode string
}

// cacheRef builds the registry reference the cache is stored under.
func (b *Builder) cacheRef(req Request) string {
	c := req.Cache
	repo := strings.Trim(c.Repo, "/")
	if repo == "" {
		// A file build may have no image repo; fall back to a stable name so the
		// cache ref stays well-formed (an empty repo would yield "/-cache").
		base := strings.Trim(req.Repo, "/")
		if base == "" {
			base = "builds"
		}
		repo = base + "-cache"
	}
	tag := c.Tag
	if tag == "" {
		tag = req.Tag
	}
	if tag == "" {
		tag = "cache"
	}
	return b.FullRef(repo, tag)
}

// Result is a completed build.
type Result struct {
	ImageRef string
	Log      string
	// Files is a tar archive of the exported tree when Request.Output=files.
	Files []byte
	// FileCount / FileBytes describe the exported tree.
	FileCount int
	FileBytes int64
	// FilesTruncated reports that the tree exceeded the cap and was NOT
	// exported, rather than silently handing back a partial tree.
	FilesTruncated bool
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
	// A FILE build produces no image, so repo/tag are optional: they only name
	// the registry cache. Requiring them would force a caller who just wants a
	// compiled binary to invent an image name.
	if req.Output != OutputFiles {
		if req.Repo == "" || req.Tag == "" {
			return "", "", "", "", errors.New("repo and tag required")
		}
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
	if req.Output != OutputFiles {
		ref = b.FullRef(req.Repo, req.Tag)
	}
	return ref, ctxDir, dockerfileDir, filename, nil
}

// solve runs one buildkit Solve with the dockerfile frontend and streams the
// progress. `emit` (optional) receives log chunks as they are produced.
func (b *Builder) solve(ctx context.Context, req Request, emit func(string), out *Result) (string, string, error) {
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

	// Choose the exporter. An image build pushes to the registry; a file build
	// exports a TREE back to this process.
	var exports []bkclient.ExportEntry
	filesMode := req.Output == OutputFiles
	var outDir string
	if filesMode {
		// buildkit's `local` exporter writes into a client-side directory through
		// a session filesync (solve.go: filesync.WithFSSyncDir). A temp dir here
		// is therefore the transport: the daemon streams the tree into it, and we
		// tar it up for the RPC. TempDir keeps it off any persistent volume.
		outDir, err = os.MkdirTemp("", "buildout-*")
		if err != nil {
			return "", "", fmt.Errorf("export dir: %w", err)
		}
		stage := req.OutputStage
		if stage == "" {
			stage = "export"
		}
		// `target` selects the stage; the dockerfile frontend understands it.
		attrs["target"] = stage
		exports = []bkclient.ExportEntry{{
			Type:      "local",
			OutputDir: outDir,
		}}
	} else {
		exports = []bkclient.ExportEntry{{
			Type:  "image",
			Attrs: expAttrs,
		}}
	}

	// Registry cache. Imports warm THIS daemon from the registry (it may be a
	// brand-new one); exports keep the cache alive past the daemon's own PVC.
	// Without this a daemon replacement (or a job-per-build model) starts cold.
	var cacheImports []bkclient.CacheOptionsEntry
	var cacheExports []bkclient.CacheOptionsEntry
	if req.Cache.Enabled {
		cacheAttrs := map[string]string{"ref": b.cacheRef(req)}
		if b.scheme() == "http" {
			cacheAttrs["registry.insecure"] = "true"
		}
		// "max" exports EVERY intermediate layer so `RUN go mod download`
		// survives a daemon replacement; "min" exports only final-image layers,
		// which is nearly useless as a build cache.
		mode := req.Cache.Mode
		if mode == "" {
			mode = "max"
		}
		cacheAttrs["mode"] = mode
		cacheImports = append(cacheImports, bkclient.CacheOptionsEntry{Type: "registry", Attrs: cacheAttrs})
		if !req.Cache.ImportOnly {
			cacheExports = append(cacheExports, bkclient.CacheOptionsEntry{Type: "registry", Attrs: cacheAttrs})
		}
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
		Exports:      exports,
		CacheImports: cacheImports,
		CacheExports: cacheExports,
		Session:      []session.Attachable{ap},
	}, ch)
	<-done
	if filesMode && outDir != "" {
		defer os.RemoveAll(outDir)
	}
	if serr != nil {
		mu.Lock()
		defer mu.Unlock()
		return ref, "", fmt.Errorf("buildkit solve: %w\n%s", serr, tailBytes(logBuf.String(), 16*1024))
	}
	if filesMode {
		limit := req.MaxOutputBytes
		if limit <= 0 {
			limit = MaxFilesBytes
		}
		files, count, bytes, truncated, ferr := tarDir(outDir, limit)
		if ferr != nil {
			return ref, "", ferr
		}
		if out != nil {
			out.Files = files
			out.FileCount = count
			out.FileBytes = bytes
			out.FilesTruncated = truncated
		}
		mu.Lock()
		defer mu.Unlock()
		return ref, tailBytes(logBuf.String(), 16*1024), nil
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
	var res Result
	ref, log, err := b.solve(ctx, req, nil, &res)
	if err != nil {
		return Result{}, err
	}
	res.ImageRef = ref
	res.Log = log
	return res, nil
}

// BuildStream runs the build, streaming output to `emit`.
func (b *Builder) BuildStream(ctx context.Context, req Request, emit func(chunk string)) (string, error) {
	ref, _, err := b.solve(ctx, req, emit, nil)
	return ref, err
}

// BuildFiles runs a FILE build and returns the exported tree as a tar.
func (b *Builder) BuildFiles(ctx context.Context, req Request) (Result, error) {
	req.Output = OutputFiles
	return b.Build(ctx, req)
}

// tarDir packs dir into an in-memory tar, refusing to exceed limit.
//
// The whole tree is buffered because it is handed back over the RPC in one
// message. Exceeding the cap FAILS the export (FilesTruncated) rather than
// returning a partial tree: a half-copied artifact is worse than a clear error,
// and the caller can switch to an image build for anything that big.
func tarDir(dir string, limit int64) (data []byte, count int, total int64, truncated bool, err error) {
	// A non-positive limit means "no practical cap": callers pass MaxFilesBytes
	// explicitly, but a zero here must not mean "reject every file".
	if limit <= 0 {
		limit = MaxFilesBytes
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if path == dir {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		hdr, herr := tar.FileInfoHeader(info, "")
		if herr != nil {
			return herr
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if info.Mode().IsRegular() {
			total += info.Size()
			if total > limit {
				truncated = true
				return errFileCap
			}
		}
		if werr := tw.WriteHeader(hdr); werr != nil {
			return werr
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, oerr := os.Open(path)
		if oerr != nil {
			return oerr
		}
		defer f.Close()
		if _, cerr := io.Copy(tw, f); cerr != nil {
			return cerr
		}
		count++
		return nil
	})
	if walkErr == errFileCap {
		return nil, 0, total, true, nil
	}
	if walkErr != nil {
		return nil, 0, 0, false, walkErr
	}
	if err := tw.Close(); err != nil {
		return nil, 0, 0, false, err
	}
	return buf.Bytes(), count, total, false, nil
}

// errFileCap is an internal sentinel: the exported tree exceeded the cap.
var errFileCap = errors.New("exported tree exceeds the size cap")

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
	}, nil, nil); err != nil {
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
