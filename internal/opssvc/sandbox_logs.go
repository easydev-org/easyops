package opssvc

import (
	"context"

	"connectrpc.com/connect/v2"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	opsv1connect "github.com/easy-platform/easyops/gen/easyops/v1/opsv1connect"
	"github.com/easy-platform/easyops/internal/sandboxmgr"
)

// SandboxLogs reads a bounded window of a sandbox's container log. It is the
// sandbox analogue of ServiceLogs: the sandbox is owned by easyops, so only
// easyops' ServiceAccount can read its pod — this RPC is the way a non-admin
// caller (scoped by `owner`) can diagnose a Failed sandbox.
func (s *Service) SandboxLogs(ctx context.Context, req *opsv1.SandboxLogsRequest) (*opsv1.SandboxLogsResponse, error) {
	if _, ok, err := s.ownedSandbox(ctx, req.GetOwner(), req.GetName()); err != nil {
		return nil, err
	} else if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "sandbox not found")
	}
	tail := req.GetTailLines()
	if tail <= 0 {
		tail = s.serviceLogTail
	}
	lines, err := s.sbx.Logs(ctx, req.GetName(), sandboxmgr.LogOptions{
		TailLines: tail, Previous: req.GetPrevious(),
	})
	if err != nil {
		return nil, mapK8sErr(err)
	}
	resp := &opsv1.SandboxLogsResponse{Lines: lines, Available: len(lines) > 0}
	// A container that failed BEFORE producing output has no stdout/stderr, so
	// the ONLY explanation lives in the pod status + events. Always attach them.
	d := s.sandboxDiagnostics(ctx, req.GetName())
	resp.PodPhase = d.phase
	resp.Restarts = d.restarts
	resp.Message = d.message
	resp.ExitCode = d.exitCode
	resp.TerminatedReason = d.terminatedReason
	resp.LastStateReason = d.lastStateReason
	resp.Events = d.events
	return resp, nil
}

// WatchSandboxLogs streams a sandbox's container log until the stream ends or
// the client disconnects.
func (s *Service) WatchSandboxLogs(ctx context.Context, req *opsv1.WatchSandboxLogsRequest, st opsv1connect.OpsServiceWatchSandboxLogsServerStream) error {
	// A streaming interceptor cannot see the request message; apply the owner
	// check (user-token path) here.
	if err := checkOwner(ctx, req.GetOwner()); err != nil {
		return err
	}
	if _, ok, err := s.ownedSandbox(ctx, req.GetOwner(), req.GetName()); err != nil {
		return err
	} else if !ok {
		return connect.NewError(connect.CodeNotFound, "sandbox not found")
	}
	lines, errc := s.sbx.Follow(ctx, req.GetName(), sandboxmgr.LogOptions{
		Previous: req.GetPrevious(),
	})
	sent := false
	for l := range lines {
		sent = true
		if err := st.Send(&opsv1.WatchSandboxLogsResponse{Output: l + "\n"}); err != nil {
			return err
		}
	}
	// Terminal frame: carry diagnostics when the container never produced output,
	// so a failed-to-start container is explained rather than blank.
	done := &opsv1.WatchSandboxLogsResponse{Done: true}
	if !sent {
		d := s.sandboxDiagnostics(ctx, req.GetName())
		done.PodPhase = d.phase
		done.Restarts = d.restarts
		done.Message = d.message
		done.ExitCode = d.exitCode
		done.TerminatedReason = d.terminatedReason
		done.LastStateReason = d.lastStateReason
		done.Events = d.events
	}
	if err := <-errc; err != nil {
		done.Error = err.Error()
	}
	return st.Send(done)
}

// sandboxDiag is the diagnostic snapshot of a sandbox's pod.
type sandboxDiag struct {
	phase            string
	restarts         int32
	message          string
	exitCode         int32
	terminatedReason string
	lastStateReason  string
	events           []string
}

// sandboxDiagnostics reads a sandbox's pod diagnostics + recent events.
// Best-effort: on error it returns the zero value (callers then leave the
// response fields empty).
func (s *Service) sandboxDiagnostics(ctx context.Context, name string) sandboxDiag {
	sb, ok, err := s.sbx.Get(ctx, name)
	if err != nil || !ok {
		return sandboxDiag{}
	}
	return sandboxDiag{
		phase:            sb.Phase,
		restarts:         sb.Restarts,
		message:          sb.Message,
		exitCode:         sb.ExitCode,
		terminatedReason: sb.TerminatedReason,
		lastStateReason:  sb.LastStateReason,
		events:           s.sbx.Events(ctx, name, 20),
	}
}
