package opssvc

import (
	"context"

	"connectrpc.com/connect/v2"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	opsv1connect "github.com/easy-platform/easyops/gen/easyops/v1/opsv1connect"
	"github.com/easy-platform/easyops/internal/imagebuild"
)

// errNoImageBackend is returned by image RPCs when the builder is absent.
func errNoImageBackend() error {
	return connect.NewError(connect.CodeUnavailable, "image builder not configured")
}

// ---- image build / import ----

// BuildImage builds an image from a repo Dockerfile via buildkit and pushes it
// to the deployment registry. The repo tarball is supplied by the caller.
func (s *Service) BuildImage(ctx context.Context, req *opsv1.BuildImageRequest) (*opsv1.BuildImageResponse, error) {
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
	dir, cleanup, err := imagebuild.Extract(m.GetArchive(), 0)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	defer cleanup()
	res, err := s.builder.Build(ctx, imagebuild.Request{
		ContextDir: dir,
		Dockerfile: m.GetDockerfile(),
		Context:    m.GetContext(),
		Repo:       m.GetRepo(),
		Tag:        m.GetTag(),
		BuildArgs:  m.GetBuildArgs(),
	})
	if err != nil {
		return nil, mapK8sErr(err)
	}
	return &opsv1.BuildImageResponse{ImageRef: res.ImageRef, Log: res.Log}, nil
}

// BuildImageStream builds like BuildImage but STREAMS the build output live, so
// a client (the gateway, then the webui) can show the log as it is produced.
func (s *Service) BuildImageStream(ctx context.Context, req *opsv1.BuildImageRequest, stream opsv1connect.OpsServiceBuildImageStreamServerStream) error {
	if s.builder == nil {
		return errNoImageBackend()
	}
	m := req
	if len(m.GetArchive()) == 0 {
		return connect.NewError(connect.CodeInvalidArgument, "archive is required")
	}
	if m.GetRepo() == "" || m.GetTag() == "" {
		return connect.NewError(connect.CodeInvalidArgument, "repo and tag are required")
	}
	dir, cleanup, err := imagebuild.Extract(m.GetArchive(), 0)
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	defer cleanup()
	// Stream each chunk as it arrives. A send failure (client gone) stops the
	// build via the returned error, which also cancels the build context.
	var sendErr error
	ref, berr := s.builder.BuildStream(ctx, imagebuild.Request{
		ContextDir: dir,
		Dockerfile: m.GetDockerfile(),
		Context:    m.GetContext(),
		Repo:       m.GetRepo(),
		Tag:        m.GetTag(),
		BuildArgs:  m.GetBuildArgs(),
	}, func(chunk string) {
		if sendErr != nil {
			return
		}
		sendErr = stream.Send(&opsv1.BuildImageStreamEvent{Chunk: chunk, State: "running"})
	})
	if sendErr != nil {
		return sendErr
	}
	final := &opsv1.BuildImageStreamEvent{State: "done", ImageRef: ref}
	if berr != nil {
		final = &opsv1.BuildImageStreamEvent{State: "failed", Error: berr.Error()}
	}
	return stream.Send(final)
}

// ImportImage mirrors an upstream image into the deployment registry with a
// no-op `FROM <source>` rebuild.
func (s *Service) ImportImage(ctx context.Context, req *opsv1.ImportImageRequest) (*opsv1.ImportImageResponse, error) {
	if s.builder == nil {
		return nil, errNoImageBackend()
	}
	m := req
	if m.GetSource() == "" || m.GetRepo() == "" || m.GetTag() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "source, repo and tag are required")
	}
	res, err := s.builder.Import(ctx, imagebuild.ImportRequest{
		Source: m.GetSource(), Repo: m.GetRepo(), Tag: m.GetTag(),
		AuthUser: m.GetAuthUser(), AuthToken: m.GetAuthToken(),
	})
	if err != nil {
		return nil, mapK8sErr(err)
	}
	return &opsv1.ImportImageResponse{ImageRef: res.ImageRef, Log: res.Log}, nil
}
