package opssvc

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// mapK8sErr maps a backend/kubernetes error to a Connect error with a
// meaningful code, so a caller can tell permission from failure. The gateway
// (and any other OpsService client) relies on the code to distinguish a
// misconfigured RBAC (403 PermissionDenied) from a genuine internal fault
// (500 Internal).
//
// Errors that are not recognised k8s API errors fall through to
// CodeInternal. A nil error maps to nil.
func mapK8sErr(err error) error {
	switch {
	case err == nil:
		return nil
	case apierrors.IsForbidden(err):
		return connect.NewError(connect.CodePermissionDenied, err.Error()).WithCause(err) // 403
	case apierrors.IsUnauthorized(err):
		return connect.NewError(connect.CodeUnauthenticated, err.Error()).WithCause(err) // 401
	case apierrors.IsNotFound(err):
		return connect.NewError(connect.CodeNotFound, err.Error()).WithCause(err) // 404
	case apierrors.IsAlreadyExists(err):
		return connect.NewError(connect.CodeAlreadyExists, err.Error()).WithCause(err) // 409
	case apierrors.IsConflict(err):
		return connect.NewError(connect.CodeAborted, err.Error()).WithCause(err) // 409 / aborted
	default:
		return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err) // 500
	}
}

// mapBuildErr maps an image-build failure to a Connect error.
//
// A build is a program the CALLER supplied (a Dockerfile whose RUN exited
// non-zero, an unpullable base image, a registry that refused the push). That is
// a failed precondition of the request, not a fault of this server, so it must
// NOT be CodeInternal: mapK8sErr would classify every non-k8s error as Internal
// and an operator would go looking for a server bug that does not exist.
//
// Only a real backend/daemon problem (buildkitd unreachable) stays Internal.
func mapBuildErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "buildkit dial"),
		strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "code = Unavailable"):
		return connect.NewError(connect.CodeUnavailable, msg).WithCause(err)
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, msg).WithCause(err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, msg).WithCause(err)
	}
	return connect.NewError(connect.CodeFailedPrecondition, msg).WithCause(err)
}
