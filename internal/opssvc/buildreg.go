package opssvc

import (
	"context"
	"fmt"

	"connectrpc.com/connect/v2"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	"github.com/easy-platform/easyops/internal/buildreg"
	"github.com/easy-platform/easyops/internal/imagebuild"
)

// DurabilityMemory is the value reported in BuildRecord.durability. It exists so
// a client can never mistake a record for durable history.
const DurabilityMemory = "memory"

func errNoBuildRegistry() error {
	return connect.NewError(connect.CodeUnavailable, "build registry is not configured")
}

// CreateBuild starts a build in the BACKGROUND and returns immediately.
//
// The whole point is that the caller gets an id it can poll and cancel. The
// build itself runs under a context owned by the registry, NOT the request
// context: if it inherited the request context, the build would be cancelled
// the moment CreateBuild returned.
func (s *Service) CreateBuild(ctx context.Context, req *opsv1.CreateBuildRequest) (*opsv1.CreateBuildResponse, error) {
	if s.builds == nil {
		return nil, errNoBuildRegistry()
	}
	if s.builder == nil {
		return nil, errNoImageBackend()
	}
	m := req
	if len(m.GetArchive()) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "archive is required")
	}
	if m.GetRepo() == "" || m.GetTag() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "repo and tag are required")
	}
	// Extract NOW, synchronously: the archive is request-scoped memory and is
	// gone once this call returns, so the background goroutine cannot read it
	// later. A bad archive is therefore rejected inline, which is also the
	// behaviour a caller wants (no build id for input that cannot be built).
	dir, cleanup, err := imagebuild.Extract(m.GetArchive(), 0)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}

	cache := cacheOptions(m.GetImageCache())
	id, bctx := s.builds.Start(context.Background(), m.GetOwner(), m.GetRepo(), m.GetTag(), cache.Enabled)

	go func() {
		defer cleanup()
		ref, berr := s.builder.BuildStream(bctx, imagebuild.Request{
			ContextDir: dir,
			Dockerfile: m.GetDockerfile(),
			Context:    m.GetContext(),
			Repo:       m.GetRepo(),
			Tag:        m.GetTag(),
			BuildArgs:  m.GetBuildArgs(),
			Cache:      cache,
		}, func(chunk string) {
			s.builds.AppendLog(id, chunk)
		})
		s.builds.Finish(id, ref, berr)
	}()

	rec, ok := s.builds.Get(id)
	if !ok {
		return nil, connect.NewError(connect.CodeInternal, "build record vanished immediately")
	}
	return &opsv1.CreateBuildResponse{Build: toBuildRecord(rec)}, nil
}

// ListBuilds returns builds newest-first, owner-scoped.
func (s *Service) ListBuilds(ctx context.Context, req *opsv1.ListBuildsRequest) (*opsv1.ListBuildsResponse, error) {
	if s.builds == nil {
		return nil, errNoBuildRegistry()
	}
	recs := s.builds.List(req.GetOwner(), int(req.GetLimit()))
	out := make([]*opsv1.BuildRecord, 0, len(recs))
	for i := range recs {
		out = append(out, toBuildRecord(recs[i]))
	}
	return &opsv1.ListBuildsResponse{Builds: out}, nil
}

// GetBuild returns one build plus a WINDOW of its log by line.
//
// The window is by LINE, not by byte: a caller reads a log top-down, and line
// numbers are what a person (or a model) can act on. offset is 0-based; limit
// defaults to 200 and is capped at 1000, so a single response stays small while
// the whole log remains reachable by paging.
func (s *Service) GetBuild(ctx context.Context, req *opsv1.GetBuildRequest) (*opsv1.GetBuildResponse, error) {
	if s.builds == nil {
		return nil, errNoBuildRegistry()
	}
	rec, ok := s.builds.Get(req.GetId())
	if !ok {
		return &opsv1.GetBuildResponse{Found: false}, nil
	}
	if !owned(rec.Owner, req.GetOwner()) {
		return &opsv1.GetBuildResponse{Found: false}, nil
	}
	page, _ := s.builds.LogPage(req.GetId(), int(req.GetLogOffset()), int(req.GetLogLimit()))
	more := page.FirstLine-1+len(page.Lines) < page.TotalLines
	return &opsv1.GetBuildResponse{
		Build:      toBuildRecord(rec),
		Found:      true,
		LogLines:   page.Lines,
		FirstLine:  int32(page.FirstLine),
		TotalLines: int32(page.TotalLines),
		More:       more,
	}, nil
}

// CancelBuild cancels a running build.
func (s *Service) CancelBuild(ctx context.Context, req *opsv1.CancelBuildRequest) (*opsv1.CancelBuildResponse, error) {
	if s.builds == nil {
		return nil, errNoBuildRegistry()
	}
	rec, ok := s.builds.Get(req.GetId())
	if !ok || !writable(rec.Owner, req.GetOwner()) {
		return &opsv1.CancelBuildResponse{Found: false}, nil
	}
	canceled, ok := s.builds.Cancel(req.GetId())
	if !ok {
		return &opsv1.CancelBuildResponse{Found: false}, nil
	}
	return &opsv1.CancelBuildResponse{Build: toBuildRecord(canceled), Found: true}, nil
}

// toBuildRecord maps the library record onto the wire type.
func toBuildRecord(r buildreg.Record) *opsv1.BuildRecord {
	return &opsv1.BuildRecord{
		Id: r.ID, Owner: r.Owner, Repo: r.Repo, Tag: r.Tag,
		Image: r.Image, State: string(r.State), Error: r.Error,
		Log: r.Log, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
		CacheUsed: r.CacheUsed, Durability: DurabilityMemory,
		LogBytes: r.LogBytes, LogTruncated: r.LogTruncated, LogDropped: r.LogDropped,
	}
}

// buildSummary is a one-line rendering used by the watch frame.
func buildSummary(r buildreg.Record) string {
	return fmt.Sprintf("%s %s:%s %s", r.ID, r.Repo, r.Tag, r.State)
}
