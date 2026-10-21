package opssvc

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect/v2"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	"github.com/easy-platform/easyops/k8s/sandboxmgr"
)

// defaultGoldenDiskCachePVC is the shared, platform-wide golden-disk cache PVC
// (single-node RWO). Name/size overridable via SANDBOX_GOLDEN_PVC /
// SANDBOX_GOLDEN_PVC_SIZE.
const defaultGoldenDiskCachePVC = "sandbox-golden-cache"

// ensureGoldenDiskCache creates the shared cache PVC if absent (idempotent). It
// must hold the biggest golden base (~15 GiB macOS) — default 40Gi.
func (s *Service) ensureGoldenDiskCache(ctx context.Context, name string) error {
	size := s.goldenDiskCacheSize
	if size == "" {
		size = "40Gi"
	}
	if _, err := s.services.CreatePVC(ctx, name, size, s.pvcStorageClass, "", ""); err != nil {
		return fmt.Errorf("ensure golden-disk cache pvc %q: %w", name, err)
	}
	return nil
}

// ---- sandboxes ----

// CreateSandbox creates a sandbox owned by `owner`. The owner is opaque: it is
// stamped as the object's creator and never validated.
func (s *Service) CreateSandbox(ctx context.Context, req *opsv1.CreateSandboxRequest) (*opsv1.CreateSandboxResponse, error) {
	m := req
	if m.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "name is required")
	}
	if m.GetGpuCount() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "gpu_count must be >= 0")
	}
	if m.GetReadyTimeoutSeconds() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "ready_timeout_seconds must be >= 0")
	}
	image := m.GetImage()
	if image == "" {
		image = s.defaultImage
	}
	if image == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "image is required and no default is configured")
	}
	// Enforce that the image comes from artifact (and, for sandboxes, the
	// dedicated sandbox org); a bare `<org>/<name>:<tag>` is rewritten to the
	// configured registry. See image_enforce.go.
	image, err := s.enforceSandboxImage(image)
	if err != nil {
		return nil, err
	}
	// VM images (Windows/macOS) fetch their guest disk at runtime from
	// GOLDEN_DISK_URL; mount a SHARED golden-disk cache PVC so the base is
	// downloaded once and reused by every VM sandbox on the node.
	goldenPVC := ""
	if m.GetGoldenDisk() {
		name := s.goldenDiskCachePVC
		if name == "" {
			name = defaultGoldenDiskCachePVC
		}
		// The shared-cache PVC needs the services backend. If it is not
		// configured (nil), log loudly and continue WITHOUT the cache (never
		// panic): a VM sandbox still runs, just without the shared base cache.
		if s.services == nil {
			log.Printf("warn: sandbox %q: golden_disk requested but the services backend is not configured; skipping shared cache PVC %q", m.GetName(), name)
		} else if err := s.ensureGoldenDiskCache(ctx, name); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
		} else {
			log.Printf("sandbox %q: golden_disk=true, shared cache PVC %q ready (storageClass=%q)", m.GetName(), name, s.pvcStorageClass)
			goldenPVC = name
		}
	}
	vols, err := s.resolveSandboxVolumes(ctx, m.GetOwner(), m.GetVolumes())
	if err != nil {
		return nil, err
	}
	sb, token, err := s.sbx.Create(ctx, sandboxmgr.Spec{
		Name:          m.GetName(),
		Image:         image,
		CPU:           m.GetCpu(),
		Memory:        m.GetMemory(),
		Env:           mergeEnv(sandboxUpstreamEnv(s.sandboxPackageUpstream), m.GetEnv()),
		Creator:       m.GetOwner(),
		Session:       m.GetSession(),
		Runtime:       s.runtime.Render(m.GetKvm(), int(m.GetGpuCount())),
		GoldenDiskPVC: goldenPVC,
		Volumes:       vols,
	})
	if err != nil {
		if errors.Is(err, sandboxmgr.ErrExists) {
			return nil, connect.NewError(connect.CodeAlreadyExists, err.Error()).WithCause(err)
		}
		return nil, mapK8sErr(err)
	}
	// A positive ready timeout waits (bounded) for the worker to accept a TCP
	// connection; on timeout the sandbox is left in place for the caller to
	// status/delete.
	if t := m.GetReadyTimeoutSeconds(); t > 0 {
		if err := s.sbx.WaitReady(ctx, m.GetName(), time.Duration(t)*time.Second); err != nil {
			return nil, connect.NewError(connect.CodeDeadlineExceeded, err.Error()).WithCause(err)
		}
		if cur, ok, gerr := s.sbx.Get(ctx, m.GetName()); gerr == nil && ok {
			sb = cur
		}
	}
	// Package-source bootstrap (best-effort, detached from this request so it
	// survives the RPC returning). Failures are logged, never surfaced: a
	// sandbox without the file-type mirrors still works.
	if s.bootstrapEnabled() {
		bctx, cancel := bootstrapContext(ctx)
		go func() {
			defer cancel()
			s.runSandboxBootstrap(bctx, m.GetName())
		}()
	}
	return &opsv1.CreateSandboxResponse{Sandbox: toSandbox(sb), Token: token}, nil
}

// ListSandboxes lists the sandboxes visible to `owner`, newest-first. An empty
// owner means "no owner filter" (all sandboxes) — there is no per-owner auth.
func (s *Service) ListSandboxes(ctx context.Context, req *opsv1.ListSandboxesRequest) (*opsv1.ListSandboxesResponse, error) {
	out, err := s.listSandboxes(ctx, req.GetOwner(), req.GetSession())
	if err != nil {
		return nil, err
	}
	return &opsv1.ListSandboxesResponse{Sandboxes: out}, nil
}

// GetSandbox returns one sandbox if it is visible to `owner`.
func (s *Service) GetSandbox(ctx context.Context, req *opsv1.GetSandboxRequest) (*opsv1.GetSandboxResponse, error) {
	sb, ok, err := s.ownedSandbox(ctx, req.GetOwner(), req.GetName())
	if err != nil {
		return nil, err
	}
	if !ok {
		return &opsv1.GetSandboxResponse{Found: false}, nil
	}
	return &opsv1.GetSandboxResponse{Sandbox: toSandbox(sb), Found: true}, nil
}

// ResolveSandbox returns a sandbox's URL + bearer token, if visible to `owner`.
func (s *Service) ResolveSandbox(ctx context.Context, req *opsv1.ResolveSandboxRequest) (*opsv1.ResolveSandboxResponse, error) {
	if _, ok, err := s.ownedSandbox(ctx, req.GetOwner(), req.GetName()); err != nil {
		return nil, err
	} else if !ok {
		return nil, connect.Errorf(connect.CodeNotFound, "sandbox %q not found", req.GetName())
	}
	url, token, err := s.sbx.Resolve(ctx, req.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error()).WithCause(err)
	}
	return &opsv1.ResolveSandboxResponse{Url: url, Token: token}, nil
}

// DeleteSandbox deletes a sandbox if visible to `owner`.
func (s *Service) DeleteSandbox(ctx context.Context, req *opsv1.DeleteSandboxRequest) (*opsv1.DeleteSandboxResponse, error) {
	if _, ok, err := s.writableSandbox(ctx, req.GetOwner(), req.GetName()); err != nil {
		return nil, err
	} else if !ok {
		return &opsv1.DeleteSandboxResponse{Deleted: false}, nil
	}
	ok, err := s.sbx.Delete(ctx, req.GetName())
	if err != nil {
		return nil, mapK8sErr(err)
	}
	return &opsv1.DeleteSandboxResponse{Deleted: ok}, nil
}

// WatchWorkspace lives in watch.go (it spans sandboxes, services and PVCs).

// ---- sandbox helpers ----

// ownedSandbox fetches a sandbox and reports whether it exists AND is visible to
// `owner`. A sandbox owned by someone else is reported as not found (so a
// foreign name never leaks).
func (s *Service) ownedSandbox(ctx context.Context, owner, name string) (sandboxmgr.Sandbox, bool, error) {
	sb, ok, err := s.sbx.Get(ctx, name)
	if err != nil {
		return sandboxmgr.Sandbox{}, false, mapK8sErr(err)
	}
	if !ok || !owned(sb.Creator, owner) {
		return sandboxmgr.Sandbox{}, false, nil
	}
	return sb, true, nil
}

// writableSandbox resolves a sandbox and reports whether `owner` may MUTATE it.
// An unowned sandbox (creator "") is NOT writable (A1).
func (s *Service) writableSandbox(ctx context.Context, owner, name string) (sandboxmgr.Sandbox, bool, error) {
	sb, ok, err := s.sbx.Get(ctx, name)
	if err != nil {
		return sandboxmgr.Sandbox{}, false, mapK8sErr(err)
	}
	if !ok || !writable(sb.Creator, owner) {
		return sandboxmgr.Sandbox{}, false, nil
	}
	return sb, true, nil
}

// listSandboxes lists the sandboxes visible to `owner` (optionally narrowed to
// `session`), newest-first.
func (s *Service) listSandboxes(ctx context.Context, owner, session string) ([]*opsv1.Sandbox, error) {
	sbxs, err := s.sbx.List(ctx)
	if err != nil {
		return nil, mapK8sErr(err)
	}
	out := make([]*opsv1.Sandbox, 0, len(sbxs))
	for _, sb := range sbxs {
		if !owned(sb.Creator, owner) {
			continue
		}
		if session != "" && sb.Session != session {
			continue
		}
		out = append(out, toSandbox(sb))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].GetCreatedAt() > out[j].GetCreatedAt() })
	return out, nil
}

// toSandbox maps a library Sandbox to its wire form.
func toSandbox(sb sandboxmgr.Sandbox) *opsv1.Sandbox {
	return &opsv1.Sandbox{
		Name:             sb.Name,
		Image:            sb.Image,
		Phase:            sb.Phase,
		Ready:            sb.Ready,
		Url:              sb.URL,
		Owner:            sb.Creator,
		Session:          sb.Session,
		CreatedAt:        sb.CreatedAt,
		Restarts:         sb.Restarts,
		Message:          sb.Message,
		ExitCode:         sb.ExitCode,
		TerminatedReason: sb.TerminatedReason,
		LastStateReason:  sb.LastStateReason,
		Container:        sb.Container,
		StartedAt:        sb.StartedAt,
		FinishedAt:       sb.FinishedAt,
	}
}

// resolveSandboxVolumes validates each requested PVC mount: the claim must exist
// and belong to the caller's tenant (owner). Mirrors the service-deploy path so
// a sandbox can never mount (read or write) another tenant's volume.
func (s *Service) resolveSandboxVolumes(ctx context.Context, owner string, mounts []*opsv1.VolumeMountSpec) ([]sandboxmgr.VolumeMount, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	// Resolving a mount needs the services backend (PVC lookup). Guard it like
	// every other PVC path so a missing backend is a clean Unavailable, not a nil
	// deref.
	if s.services == nil {
		return nil, errNoServiceBackend()
	}
	out := make([]sandboxmgr.VolumeMount, 0, len(mounts))
	for i, m := range mounts {
		if m.GetPvc() == "" {
			return nil, connect.Errorf(connect.CodeInvalidArgument, "volumes[%d]: pvc is required", i)
		}
		if m.GetMountPath() == "" || !strings.HasPrefix(m.GetMountPath(), "/") {
			return nil, connect.Errorf(connect.CodeInvalidArgument, "volumes[%d]: mount_path must be an absolute path", i)
		}
		pvc, err := s.services.GetPVC(ctx, m.GetPvc())
		if err != nil || (pvc.Creator != "" && pvc.Creator != owner) {
			return nil, connect.Errorf(connect.CodeNotFound, "volumes[%d]: pvc %q not found", i, m.GetPvc())
		}
		out = append(out, sandboxmgr.VolumeMount{
			PVC: pvc.Name, MountPath: m.GetMountPath(),
			ReadOnly: m.GetReadOnly(), SubPath: m.GetSubPath(),
		})
	}
	return out, nil
}
