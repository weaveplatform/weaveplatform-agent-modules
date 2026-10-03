package modulesdk

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/werror"
)

func TestSentinelErr(t *testing.T) {
	if sentinelErr(nil) != nil {
		t.Fatal("nil did not stay nil")
	}
	plain := errors.New("plain")
	if sentinelErr(plain) != plain {
		t.Fatal("non-status error was rewritten")
	}
	for code, want := range map[codes.Code]error{
		codes.NotFound:           werror.ErrNotFound,
		codes.Unavailable:        werror.ErrUnavailable,
		codes.PermissionDenied:   werror.ErrDenied,
		codes.FailedPrecondition: werror.ErrProtocol,
	} {
		err := sentinelErr(status.Error(code, "server says"))
		if !errors.Is(err, want) || !strings.Contains(err.Error(), "server says") {
			t.Errorf("%v: got %v, want wrapping %v", code, err, want)
		}
	}
	internal := status.Error(codes.Internal, "boom")
	if sentinelErr(internal) != internal {
		t.Fatal("unmapped code was rewritten")
	}
}
