package opssvc

import (
	"context"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/easy-platform/easyops/internal/platform/obs"
)

// NewObserveInterceptor records one observation per RPC (a request counter and a
// latency histogram, labelled by procedure + Connect code) on the obs default
// registry. It covers unary and server-streaming RPCs (the observation is the
// RPC's total duration).
func NewObserveInterceptor() connect.ServerInterceptor { return (&observeInterceptor{}).wrap }

type observeInterceptor struct{}

func (o *observeInterceptor) wrap(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		start := time.Now()
		err := next(ctx, spec, stream)
		obs.ObserveRPC(spec.Procedure, codeOf(err), time.Since(start))
		return err
	}
}

// codeOf maps an error to the short code label used on the metrics.
func codeOf(err error) string {
	if err == nil {
		return "ok"
	}
	return connect.CodeOf(err).String()
}
