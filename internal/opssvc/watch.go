package opssvc

import (
	"context"
	"log"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	"github.com/easy-platform/easyops/gen/easyops/v1/opsv1connect"
	"github.com/easy-platform/easyops/k8s/k8swatch"
)

// WatchWorkspace streams the owner's sandboxes, long-lived services and PVCs
// live: an initial full snapshot, then a frame whenever any owned
// object of any kind changes.
//
// EVERY frame is SELF-CONTAINED: it carries the owner-filtered full list of all
// kinds, so a naive "replace the whole table" consumer never blanks a kind that
// merely did not change this frame. The *_changed bits flag which kind(s)
// actually changed since the previous frame (all configured kinds true on the
// snapshot frame); they are a hint, not a filter on the lists.
//
// A kind's watch source is only subscribed when its backend is configured;
// otherwise that list stays empty and its changed bit false.
func (s *Service) WatchWorkspace(ctx context.Context, req *opsv1.WatchWorkspaceRequest, st opsv1connect.OpsServiceWatchWorkspaceServerStream) error {
	owner := req.GetOwner()
	// A streaming interceptor cannot see the request message, so the owner check
	// (user-token path) is applied here.
	if err := checkOwner(ctx, owner); err != nil {
		return err
	}

	// One coalescing channel per kind, so a frame can flag exactly what changed.
	sandboxChanges := k8swatch.Changes(ctx, s.sbx.Watch)
	// nil channels never fire (used when the backend is absent).
	var serviceChanges, pvcChanges <-chan struct{}
	hasServices := s.services != nil
	if hasServices {
		serviceChanges = k8swatch.Changes(ctx, s.services.WatchDeployments)
		pvcChanges = k8swatch.Changes(ctx, s.services.WatchPVCs)
	}

	// The snapshot frame flags every CONFIGURED kind as changed (a kind whose
	// backend is absent has nothing to snapshot, so its bit stays false).
	frame := s.buildWatchFrame(ctx, owner, true, true, hasServices, hasServices)
	if err := st.Send(frame); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-sandboxChanges:
			if !ok {
				return nil
			}
			if err := st.Send(s.buildWatchFrame(ctx, owner, false, true, false, false)); err != nil {
				return err
			}
		case _, ok := <-serviceChanges:
			if !ok {
				return nil
			}
			if err := st.Send(s.buildWatchFrame(ctx, owner, false, false, true, false)); err != nil {
				return err
			}
		case _, ok := <-pvcChanges:
			if !ok {
				return nil
			}
			if err := st.Send(s.buildWatchFrame(ctx, owner, false, false, false, true)); err != nil {
				return err
			}
		}
	}
}

// buildWatchFrame assembles a SELF-CONTAINED frame: every owner-filtered list is
// always filled. The *_changed flags record which kind(s) changed since the
// previous frame.
//
// A per-kind list failure (e.g. the server's ServiceAccount lacks the
// worker-namespace RBAC) does NOT abort the stream: the kind's list is left
// empty and the error is logged, so the stream stays open and the client can
// still receive frames for the other kinds. A long-lived watch should not die on
// a transient list error.
func (s *Service) buildWatchFrame(ctx context.Context, owner string, snapshot, sbChanged, svcChanged, pvcChanged bool) *opsv1.WatchWorkspaceResponse {
	frame := &opsv1.WatchWorkspaceResponse{
		Snapshot:         snapshot,
		SandboxesChanged: sbChanged,
		ServicesChanged:  svcChanged,
		PvcsChanged:      pvcChanged,
	}
	if sbxs, err := s.listSandboxes(ctx, owner, ""); err != nil {
		log.Printf("warn: WatchWorkspace: list sandboxes: %v", err)
	} else {
		frame.Sandboxes = sbxs
	}
	if svcs, err := s.listServices(ctx, owner); err != nil {
		log.Printf("warn: WatchWorkspace: list services: %v", err)
	} else {
		frame.Services = svcs
	}
	if pvcs, err := s.listPVCs(ctx, owner, false); err != nil {
		log.Printf("warn: WatchWorkspace: list pvcs: %v", err)
	} else {
		frame.Pvcs = pvcs
	}
	return frame
}
