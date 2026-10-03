package modulesdk

import (
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/werror"
)

// sentinelErr translates a gRPC status returned by a host RPC back into a
// werror sentinel, so a module can branch with errors.Is(err,
// werror.ErrNotFound) instead of unpacking status codes. It mirrors the
// hostserv-side grpcErr mapping. The original status stays wrapped so the
// server-supplied message survives. A nil error stays nil.
func sentinelErr(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	var sentinel error
	switch st.Code() {
	case codes.NotFound:
		sentinel = werror.ErrNotFound
	case codes.Unavailable:
		sentinel = werror.ErrUnavailable
	case codes.PermissionDenied:
		sentinel = werror.ErrDenied
	case codes.FailedPrecondition:
		sentinel = werror.ErrProtocol
	default:
		return err
	}
	return fmt.Errorf("%s: %w", st.Message(), sentinel)
}
