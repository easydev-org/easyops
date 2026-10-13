package opssvc

import (
	"context"

	"connectrpc.com/connect/v2"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	"github.com/easy-platform/easyops/internal/helmmgr"
)

// HelmObjects lists the live status of every object a release's current revision
// deployed, so a UI can show per-object Ready/Pending/Failed (+ pod restarts and
// the first unhealthy reason) without extra calls.
func (s *Service) HelmObjects(ctx context.Context, req *opsv1.HelmObjectsRequest) (*opsv1.HelmObjectsResponse, error) {
	if s.helm == nil {
		return nil, errNoHelmBackend()
	}
	if err := s.ownedHelmRelease(ctx, req.GetOwner(), req.GetRelease()); err != nil {
		return nil, err
	}
	objs, err := s.helm.ObjectStatuses(ctx, req.GetRelease())
	if err != nil {
		return nil, mapK8sErr(err)
	}
	out := make([]*opsv1.HelmObjectInfo, 0, len(objs))
	for _, o := range objs {
		out = append(out, &opsv1.HelmObjectInfo{
			Kind: o.Kind, Name: o.Name, Namespace: o.Namespace,
			Status: o.Status, Ready: o.Ready, Message: o.Message,
			Phase: o.Phase, Restarts: o.Restarts,
			ReadyReplicas: o.ReadyReplicas, DesiredReplicas: o.DesiredReplicas,
		})
	}
	return &opsv1.HelmObjectsResponse{Objects: out}, nil
}

// HelmObjectLogs reads a bounded window of a release Pod object's container log.
// The (release, kind, name) triple is validated against the release's CURRENT
// objects, so a caller can only read logs of Pods the release deployed.
func (s *Service) HelmObjectLogs(ctx context.Context, req *opsv1.HelmObjectLogsRequest) (*opsv1.HelmObjectLogsResponse, error) {
	if s.helm == nil {
		return nil, errNoHelmBackend()
	}
	if err := s.ownedHelmRelease(ctx, req.GetOwner(), req.GetRelease()); err != nil {
		return nil, err
	}
	kind, name := req.GetKind(), req.GetName()
	if kind == "" || name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "kind and name are required")
	}
	ok, err := s.helm.HasObject(ctx, req.GetRelease(), kind, name)
	if err != nil {
		return nil, mapK8sErr(err)
	}
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound,
			"object is not part of this release's current revision")
	}
	tail := req.GetTailLines()
	if tail <= 0 {
		tail = s.serviceLogTail
	}
	lines, err := s.helm.ObjectLogs(ctx, kind, name, helmmgr.LogOptions{
		TailLines: tail, Previous: req.GetPrevious(), Container: req.GetContainer(),
	})
	if err != nil {
		return nil, mapK8sErr(err)
	}
	resp := &opsv1.HelmObjectLogsResponse{Lines: lines, Available: len(lines) > 0}
	if len(lines) == 0 {
		// No output: attach the object's diagnostics (why it produced nothing).
		if sts, serr := s.helm.ObjectStatuses(ctx, req.GetRelease()); serr == nil {
			for _, st := range sts {
				if st.Kind == kind && st.Name == name {
					resp.Message = st.Message
					break
				}
			}
		}
	}
	return resp, nil
}
