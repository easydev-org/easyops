package opssvc

import (
	"context"
	"time"

	"connectrpc.com/connect/v2"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	"github.com/easy-platform/easyops/gen/easyops/v1/opsv1connect"
	"github.com/easy-platform/easyops/internal/jobmgr"
)

// errNoJobBackend is returned by every Job RPC when the Jobs backend is not
// wired. Like the other backends it is OPTIONAL: a deployment may expose only
// a subset of the surface.
func errNoJobBackend() error {
	return connect.NewError(connect.CodeUnavailable, "jobs backend is not configured")
}

// jobTimeout bounds CreateJob's wait for the job to reach a terminal state.
// A job that is still running at the deadline is returned as `active`
// (non-fatal) — the caller then polls GetJob / JobLogs.
const jobTimeout = 5 * time.Minute

// CreateJob starts a run-to-completion Job (batch/v1). It waits briefly for a
// terminal state so the common case (a short command) returns its result
// inline; a longer job comes back `active`.
func (s *Service) CreateJob(ctx context.Context, req *opsv1.CreateJobRequest) (*opsv1.CreateJobResponse, error) {
	if s.jobs == nil {
		return nil, errNoJobBackend()
	}
	spec := jobmgr.Spec{
		Namespace:               req.GetNamespace(),
		Name:                    req.GetName(),
		Image:                   req.GetImage(),
		Command:                 req.GetCommand(),
		Args:                    req.GetArgs(),
		Env:                     req.GetEnv(),
		CPU:                     req.GetCpu(),
		Memory:                  req.GetMemory(),
		RestartPolicy:           req.GetRestartPolicy(),
		ActiveDeadlineSeconds:   req.GetActiveDeadlineSeconds(),
		TTLSecondsAfterFinished: req.GetTtlSecondsAfterFinished(),
		NodeSelector:            req.GetNodeSelector(),
		Owner:                   req.GetOwner(),
		Session:                 req.GetSession(),
	}
	if bl := req.GetBackoffLimit(); bl > 0 {
		v := bl
		spec.BackoffLimit = &v
	}
	if sa := req.GetServiceAccount(); len(sa) > 0 {
		spec.ServiceAccountName = sa[0]
	}
	for _, v := range req.GetVolumes() {
		spec.Volumes = append(spec.Volumes, jobmgr.VolumeMount{
			Name: v.GetName(), Claim: v.GetClaim(), MountPath: v.GetMountPath(),
			SubPath: v.GetSubPath(), ReadOnly: v.GetReadOnly(),
		})
	}
	j, err := s.jobs.Create(ctx, spec)
	if err != nil {
		if err == jobmgr.ErrExists {
			return nil, connect.NewError(connect.CodeAlreadyExists, "job already exists").WithCause(err)
		}
		return nil, mapK8sErr(err)
	}
	// Wait (bounded) for completion so a short job answers inline.
	if done, werr := s.jobs.Wait(ctx, j.Name, jobTimeout); werr == nil {
		j = done
	}
	return &opsv1.CreateJobResponse{Job: jobToProto(j)}, nil
}

// ListJobs lists the caller's managed jobs.
func (s *Service) ListJobs(ctx context.Context, req *opsv1.ListJobsRequest) (*opsv1.ListJobsResponse, error) {
	if s.jobs == nil {
		return nil, errNoJobBackend()
	}
	rows, err := s.jobs.List(ctx, req.GetOwner())
	if err != nil {
		return nil, mapK8sErr(err)
	}
	out := make([]*opsv1.JobInfo, 0, len(rows))
	for i := range rows {
		out = append(out, jobToProto(&rows[i]))
	}
	return &opsv1.ListJobsResponse{Jobs: out}, nil
}

// GetJob returns one job (owner-scoped like the other Get* RPCs).
func (s *Service) GetJob(ctx context.Context, req *opsv1.GetJobRequest) (*opsv1.GetJobResponse, error) {
	if s.jobs == nil {
		return nil, errNoJobBackend()
	}
	j, err := s.jobs.Get(ctx, req.GetName())
	if err != nil {
		return nil, mapK8sErr(err)
	}
	if !owned(j.Creator, req.GetOwner()) {
		return nil, connect.NewError(connect.CodeNotFound, "job not found")
	}
	return &opsv1.GetJobResponse{Job: jobToProto(j)}, nil
}

// JobLogs reads a job pod's log tail.
func (s *Service) JobLogs(ctx context.Context, req *opsv1.JobLogsRequest) (*opsv1.JobLogsResponse, error) {
	if s.jobs == nil {
		return nil, errNoJobBackend()
	}
	j, err := s.jobs.Get(ctx, req.GetName())
	if err != nil {
		return nil, mapK8sErr(err)
	}
	if !owned(j.Creator, req.GetOwner()) {
		return nil, connect.NewError(connect.CodeNotFound, "job not found")
	}
	tail := int64(req.GetTailLines())
	if tail <= 0 {
		tail = s.serviceLogTail
	}
	log, err := s.jobs.Logs(ctx, req.GetName(), req.GetContainer(), tail)
	if err != nil {
		return nil, mapK8sErr(err)
	}
	return &opsv1.JobLogsResponse{Log: log}, nil
}

// WatchJobLogs streams a running job's log until it finishes.
func (s *Service) WatchJobLogs(ctx context.Context, req *opsv1.WatchJobLogsRequest, stream opsv1connect.OpsServiceWatchJobLogsServerStream) error {
	if s.jobs == nil {
		return errNoJobBackend()
	}
	j, err := s.jobs.Get(ctx, req.GetName())
	if err != nil {
		return mapK8sErr(err)
	}
	if !owned(j.Creator, req.GetOwner()) {
		return connect.NewError(connect.CodeNotFound, "job not found")
	}
	tail := int64(req.GetTailLines())
	if tail <= 0 {
		tail = s.serviceLogTail
	}
	// Poll-and-diff: the job log endpoint is not a watch, so emit the newly
	// appended tail each tick until the job reaches a terminal state.
	var emitted int
	for {
		log, lerr := s.jobs.Logs(ctx, req.GetName(), req.GetContainer(), tail)
		if lerr == nil {
			if len(log) > emitted {
				if serr := stream.Send(&opsv1.WatchJobLogsResponse{Chunk: log[emitted:], State: "running"}); serr != nil {
					return serr
				}
				emitted = len(log)
			}
		}
		cur, gerr := s.jobs.Get(ctx, req.GetName())
		if gerr != nil {
			return mapK8sErr(gerr)
		}
		if cur.Done {
			state := cur.State
			if state == "" {
				state = "done"
			}
			// Flush anything the last read missed.
			if log, lerr := s.jobs.Logs(ctx, req.GetName(), req.GetContainer(), tail); lerr == nil && len(log) > emitted {
				_ = stream.Send(&opsv1.WatchJobLogsResponse{Chunk: log[emitted:], State: "running"})
			}
			return stream.Send(&opsv1.WatchJobLogsResponse{State: state})
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// DeleteJob removes a job and its pods (owner-scoped).
func (s *Service) DeleteJob(ctx context.Context, req *opsv1.DeleteJobRequest) (*opsv1.DeleteJobResponse, error) {
	if s.jobs == nil {
		return nil, errNoJobBackend()
	}
	j, err := s.jobs.Get(ctx, req.GetName())
	if err != nil {
		return nil, mapK8sErr(err)
	}
	if !writable(j.Creator, req.GetOwner()) {
		return nil, connect.NewError(connect.CodePermissionDenied, "job not writable by this tenant")
	}
	if err := s.jobs.Delete(ctx, req.GetName(), req.GetPropagation()); err != nil {
		return nil, mapK8sErr(err)
	}
	return &opsv1.DeleteJobResponse{Ok: true}, nil
}

// jobToProto maps the manager view onto the wire type.
func jobToProto(j *jobmgr.Job) *opsv1.JobInfo {
	if j == nil {
		return nil
	}
	return &opsv1.JobInfo{
		Name: j.Name, Namespace: j.Namespace, Image: j.Image,
		State: j.State, Done: j.Done,
		Succeeded: j.Succeeded, Failed: j.Failed, Active: j.Active,
		Reason: j.Reason, Message: j.Message,
		Creator: j.Creator, Session: j.Session,
		CreatedAt: j.CreatedAt, FinishedAt: j.FinishedAt, ExitCode: j.ExitCode,
	}
}
