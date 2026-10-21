package opssvc

import (
	"context"

	"connectrpc.com/connect/v2"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	"github.com/easy-platform/easyops/k8s/servicesmgr"
)

// ---- PVCs ----

// CreatePVC creates a named claim owned by `owner`.
func (s *Service) CreatePVC(ctx context.Context, req *opsv1.CreatePVCRequest) (*opsv1.CreatePVCResponse, error) {
	if s.services == nil {
		return nil, errNoServiceBackend()
	}
	m := req
	if m.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "name is required")
	}
	if sc := m.GetStorageClass(); sc != "" && s.pvcStorageClass != "" && sc != s.pvcStorageClass {
		return nil, connect.NewError(connect.CodeInvalidArgument, "storage_class is not supported by this deployment")
	}
	size := m.GetSize()
	if size == "" {
		size = s.pvcDefaultSize
	}
	pvc, err := s.services.CreatePVC(ctx, m.GetName(), size, s.pvcStorageClass, m.GetOwner(), m.GetNamespace())
	if err != nil {
		return nil, mapK8sErr(err)
	}
	return &opsv1.CreatePVCResponse{Pvc: toPVCInfo(pvc)}, nil
}

// ListPVCs lists the claims. PVCs are a READ-ONLY, shared view: the list is NOT
// owner-filtered (any caller sees every managed claim), so a client can show a
// claim a service depends on even when it is owned by another tenant. Get/Create/
// Delete remain owner-scoped.
func (s *Service) ListPVCs(ctx context.Context, req *opsv1.ListPVCsRequest) (*opsv1.ListPVCsResponse, error) {
	out, err := s.listPVCs(ctx, "", req.GetIncludeAll())
	if err != nil {
		return nil, err
	}
	return &opsv1.ListPVCsResponse{Pvcs: out}, nil
}

// listPVCs returns the claims visible to `owner` (empty owner = no filter). It
// returns an empty list (not an error) when the services backend is not
// configured.
//
// includeAll widens the MANAGER-level filter from "claims easyops created" to
// "every claim in the namespace". The owner filter below is then skipped for
// claims that have no creator (they were made outside easyops, so nothing can
// be matched): showing them as "unowned" is the whole point of includeAll.
func (s *Service) listPVCs(ctx context.Context, owner string, includeAll bool) ([]*opsv1.PVCInfo, error) {
	if s.services == nil {
		return nil, nil
	}
	pvcs, err := s.services.ListPVCs(ctx, includeAll)
	if err != nil {
		return nil, mapK8sErr(err)
	}
	out := make([]*opsv1.PVCInfo, 0, len(pvcs))
	for _, p := range pvcs {
		// A claim with no creator (created outside easyops) cannot be attributed,
		// so it bypasses the owner filter — but ONLY when the caller explicitly
		// asked for everything.
		if p.Creator == "" && includeAll {
			out = append(out, toPVCInfo(p))
			continue
		}
		if !owned(p.Creator, owner) {
			continue
		}
		out = append(out, toPVCInfo(p))
	}
	return out, nil
}

// GetPVC returns one claim if visible to `owner`.
func (s *Service) GetPVC(ctx context.Context, req *opsv1.GetPVCRequest) (*opsv1.GetPVCResponse, error) {
	if s.services == nil {
		return &opsv1.GetPVCResponse{Found: false}, nil
	}
	pvc, err := s.services.GetPVC(ctx, req.GetName())
	if err != nil || !owned(pvc.Creator, req.GetOwner()) {
		return &opsv1.GetPVCResponse{Found: false}, nil
	}
	return &opsv1.GetPVCResponse{Pvc: toPVCInfo(pvc), Found: true}, nil
}

// DeletePVC removes a claim visible to `owner`. Refused while a service mounts
// it (FailedPrecondition).
func (s *Service) DeletePVC(ctx context.Context, req *opsv1.DeletePVCRequest) (*opsv1.DeletePVCResponse, error) {
	if s.services == nil {
		return &opsv1.DeletePVCResponse{Deleted: false}, nil
	}
	cur, err := s.services.GetPVC(ctx, req.GetName())
	if err != nil || !writable(cur.Creator, req.GetOwner()) {
		return &opsv1.DeletePVCResponse{Deleted: false}, nil
	}
	ok, err := s.services.DeletePVC(ctx, req.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error()).WithCause(err)
	}
	return &opsv1.DeletePVCResponse{Deleted: ok}, nil
}

// toPVCInfo maps a library PVC to its wire form.
func toPVCInfo(p servicesmgr.PVC) *opsv1.PVCInfo {
	return &opsv1.PVCInfo{
		Name: p.Name, Size: p.Size, StorageClass: p.StorageClass, Phase: p.Phase,
		Owner: p.Creator, CreatedAt: p.CreatedAt, MountedBy: p.MountedBy,
		Namespace: p.Namespace, Message: p.Message, BoundVolume: p.BoundVolume,
		AgeSeconds: p.AgeSeconds, Capacity: p.Capacity,
	}
}
