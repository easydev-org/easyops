package opssvc

import (
	"context"
	"fmt"

	"connectrpc.com/connect/v2"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	"github.com/easy-platform/easyops/internal/helmmgr"
	"github.com/easy-platform/easyops/internal/imagebuild"
)

// errNoHelmBackend is returned by helm RPCs when the helm backend is absent.
func errNoHelmBackend() error {
	return connect.NewError(connect.CodeUnavailable, "helm backend not configured")
}

// ---- helm ----

// HelmDeploy renders (dry_run) or applies a chart as a release. The repo tarball
// is supplied by the caller so easyops needs no VCS access.
func (s *Service) HelmDeploy(ctx context.Context, req *opsv1.HelmDeployRequest) (*opsv1.HelmDeployResponse, error) {
	if s.helm == nil {
		return nil, errNoHelmBackend()
	}
	m := req
	if !helmReleaseNameOK(m.GetRelease()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "release must be a DNS-1123 label")
	}
	if len(m.GetArchive()) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "archive is required")
	}
	// A HelmDeploy UPSERTS by name: refuse to overwrite an existing release owned
	// by ANOTHER tenant (cross-tenant overwrite guard). Within the tenant, the
	// namespace gate is the gateway's concern.
	if cur, ok, gerr := s.helm.Get(ctx, m.GetRelease()); gerr != nil {
		return nil, mapK8sErr(gerr)
	} else if ok && !writable(cur.Creator, m.GetOwner()) {
		return nil, connect.NewError(connect.CodePermissionDenied, "release not writable by this tenant")
	}
	dir, cleanup, err := imagebuild.Extract(m.GetArchive(), 0)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	defer cleanup()
	chartDir, err := chartDirIn(dir, m.GetChartPath())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	opts := helmmgr.RenderOptions{
		OrgNS:   m.GetNamespace(),
		Release: m.GetRelease(), ChartPath: m.GetChartPath(),
		ValuesYAML: m.GetValues(), Ref: m.GetRef(),
	}
	if m.GetDryRun() {
		manifest, objects, _, _, terr := s.helm.Template(chartDir, opts)
		if terr != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, terr.Error()).WithCause(terr)
		}
		return &opsv1.HelmDeployResponse{
			Manifest: manifest, Objects: objectNames(objects),
		}, nil
	}
	rel, err := s.helm.Apply(ctx, chartDir, opts, m.GetOwner(), m.GetSession())
	if err != nil {
		return nil, mapK8sErr(err)
	}
	manifest := ""
	if len(rel.History) > 0 {
		manifest = rel.History[len(rel.History)-1].Manifest
	}
	return &opsv1.HelmDeployResponse{
		Release: s.helmReleaseInfo(ctx, rel), Manifest: manifest,
	}, nil
}

// HelmList lists the releases visible to `owner` (newest-first).
func (s *Service) HelmList(ctx context.Context, req *opsv1.HelmListRequest) (*opsv1.HelmListResponse, error) {
	if s.helm == nil {
		return &opsv1.HelmListResponse{}, nil
	}
	all, err := s.helm.List(ctx)
	if err != nil {
		return nil, mapK8sErr(err)
	}
	out := make([]*opsv1.HelmReleaseInfo, 0, len(all))
	for _, r := range all {
		if !owned(r.Creator, req.GetOwner()) {
			continue
		}
		out = append(out, s.helmReleaseInfo(ctx, r))
	}
	return &opsv1.HelmListResponse{Releases: out}, nil
}

// listHelmReleases returns the releases visible to `owner` (newest-first).
// It returns an empty list (not an error) when the helm backend is not
// configured.
func (s *Service) listHelmReleases(ctx context.Context, owner string) ([]*opsv1.HelmReleaseInfo, error) {
	if s.helm == nil {
		return nil, nil
	}
	all, err := s.helm.List(ctx)
	if err != nil {
		return nil, mapK8sErr(err)
	}
	out := make([]*opsv1.HelmReleaseInfo, 0, len(all))
	for _, r := range all {
		if !owned(r.Creator, owner) {
			continue
		}
		out = append(out, s.helmReleaseInfo(ctx, r))
	}
	return out, nil
}

// HelmHistory returns a release and its revisions.
func (s *Service) HelmHistory(ctx context.Context, req *opsv1.HelmHistoryRequest) (*opsv1.HelmHistoryResponse, error) {
	if s.helm == nil {
		return nil, errNoHelmBackend()
	}
	rel, ok, err := s.helm.Get(ctx, req.GetRelease())
	if err != nil {
		return nil, mapK8sErr(err)
	}
	if !ok || !owned(rel.Creator, req.GetOwner()) {
		return nil, connect.NewError(connect.CodeNotFound, "release not found")
	}
	revs := make([]*opsv1.HelmRevisionInfo, 0, len(rel.History))
	for _, rv := range rel.History {
		objs := make([]string, 0, len(rv.Objects))
		for _, o := range rv.Objects {
			objs = append(objs, o.Kind+"/"+o.Name)
		}
		revs = append(revs, &opsv1.HelmRevisionInfo{
			Revision: int32(rv.Revision), Ref: rv.Ref, ChartPath: rv.ChartPath,
			Values: rv.Values, CreatedAt: rv.CreatedAt, Objects: objs,
		})
	}
	return &opsv1.HelmHistoryResponse{
		Release: s.helmReleaseInfo(ctx, rel), Revisions: revs,
	}, nil
}

// HelmRollback re-applies a prior revision.
func (s *Service) HelmRollback(ctx context.Context, req *opsv1.HelmRollbackRequest) (*opsv1.HelmRollbackResponse, error) {
	if s.helm == nil {
		return nil, errNoHelmBackend()
	}
	cur, ok, err := s.helm.Get(ctx, req.GetRelease())
	if err != nil {
		return nil, mapK8sErr(err)
	}
	if !ok || !writable(cur.Creator, req.GetOwner()) {
		return nil, connect.NewError(connect.CodePermissionDenied, "release not writable by this tenant")
	}
	target := int(req.GetRevision())
	if target == 0 {
		target = cur.Revision - 1
	}
	rel, err := s.helm.Rollback(ctx, req.GetRelease(), target)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error()).WithCause(err)
	}
	return &opsv1.HelmRollbackResponse{Release: s.helmReleaseInfo(ctx, rel)}, nil
}

// HelmUninstall deletes a release and its objects.
func (s *Service) HelmUninstall(ctx context.Context, req *opsv1.HelmUninstallRequest) (*opsv1.HelmUninstallResponse, error) {
	if s.helm == nil {
		return &opsv1.HelmUninstallResponse{Deleted: false}, nil
	}
	cur, ok, err := s.helm.Get(ctx, req.GetRelease())
	if err != nil {
		return nil, mapK8sErr(err)
	}
	if !ok || !writable(cur.Creator, req.GetOwner()) {
		return &opsv1.HelmUninstallResponse{Deleted: false}, nil
	}
	ok2, err := s.helm.Uninstall(ctx, req.GetRelease())
	if err != nil {
		return nil, mapK8sErr(err)
	}
	return &opsv1.HelmUninstallResponse{Deleted: ok2}, nil
}

// helmReleaseInfo maps a library release to its wire form.
// ownedHelmRelease verifies the release exists and is visible to `owner`.
func (s *Service) ownedHelmRelease(ctx context.Context, owner, release string) error {
	rel, ok, err := s.helm.Get(ctx, release)
	if err != nil {
		return mapK8sErr(err)
	}
	if !ok || !owned(rel.Creator, owner) {
		return connect.NewError(connect.CodeNotFound, "release not found")
	}
	return nil
}

func (s *Service) helmReleaseInfo(ctx context.Context, r helmmgr.Release) *opsv1.HelmReleaseInfo {
	info := &opsv1.HelmReleaseInfo{
		Name: r.Name, Namespace: r.Namespace, Owner: r.Creator, Session: r.Session,
		OrgNamespace: r.OrgNS,
		Ref:          r.Ref, ChartPath: r.ChartPath, Revision: int32(r.Revision),
		Status: r.Status, UpdatedAt: r.UpdatedAt,
		ChartVersion: r.ChartVersion, AppVersion: r.AppVersion,
		Objects: objectNames(currentObjects(r)),
	}
	return info
}

// currentObjects returns the applied objects of a release's current (newest)
// revision. Empty when the release has no history.
func currentObjects(r helmmgr.Release) []helmmgr.Object {
	if len(r.History) == 0 {
		return nil
	}
	return r.History[len(r.History)-1].Objects
}

// objectNames renders applied object identities as "Kind/name".
func objectNames(objs []helmmgr.Object) []string {
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.Kind+"/"+o.Name)
	}
	return out
}

// helmReleaseNameOK reports whether s is a DNS-1123 label (≤53 chars).
func helmReleaseNameOK(s string) bool {
	if s == "" || len(s) > 53 {
		return false
	}
	for i, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !ok {
			return false
		}
		if (i == 0 || i == len(s)-1) && r == '-' {
			return false
		}
	}
	return true
}

// chartDirIn resolves a repo-relative chart path within an extracted repo root,
// refusing any path that escapes it.
func chartDirIn(root, chartPath string) (string, error) {
	cp := chartPath
	if cp == "" {
		cp = "."
	}
	dir, err := secureJoin(root, cp)
	if err != nil {
		return "", fmt.Errorf("chart path escapes the repository")
	}
	return dir, nil
}
