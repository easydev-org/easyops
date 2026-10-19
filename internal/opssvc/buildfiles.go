package opssvc

import (
	"context"

	"connectrpc.com/connect/v2"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	"github.com/easy-platform/easyops/internal/imagebuild"
)

// BuildFiles runs a build whose PRODUCT IS A FILE TREE (a compiled binary, a
// frontend dist/, a test report) instead of a container image.
//
// It is the same pipeline as BuildImage: the tarball is extracted, the
// Dockerfile is built with buildkitd, and the cache/registry knobs apply. Only
// the EXPORTER differs — `local` instead of `image` — and the result comes back
// as a tar through buildkit's session filesync, so nothing is written to the
// daemon's disk.
//
// The Dockerfile must name the stage to export (default "export"):
//
//	FROM golang:1.26 AS build
//	RUN go build -o /out/app ./cmd/app
//	FROM scratch AS export
//	COPY --from=build /out /
func (s *Service) BuildFiles(ctx context.Context, req *opsv1.BuildImageRequest) (*opsv1.BuildFilesResponse, error) {
	if s.builder == nil {
		return nil, errNoImageBackend()
	}
	m := req
	if len(m.GetArchive()) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "archive is required")
	}
	dir, cleanup, err := imagebuild.Extract(m.GetArchive(), 0)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	defer cleanup()

	// Repo/Tag are optional here: a file build does not push an image. They only
	// serve as the derived registry cache key, and imagebuild derives a stable
	// one when they are empty.
	res, err := s.builder.BuildFiles(ctx, imagebuild.Request{
		ContextDir:     dir,
		Dockerfile:     m.GetDockerfile(),
		Context:        m.GetContext(),
		Repo:           m.GetRepo(),
		Tag:            m.GetTag(),
		BuildArgs:      m.GetBuildArgs(),
		Cache:          cacheOptions(m.GetImageCache()),
		OutputStage:    m.GetOutputStage(),
		MaxOutputBytes: m.GetMaxOutputBytes(),
	})
	if err != nil {
		return nil, mapK8sErr(err)
	}
	return &opsv1.BuildFilesResponse{
		Files:     res.Files,
		FileCount: int32(res.FileCount),
		FileBytes: res.FileBytes,
		Truncated: res.FilesTruncated,
		Log:       res.Log,
	}, nil
}
