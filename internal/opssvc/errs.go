package opssvc

import (
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
