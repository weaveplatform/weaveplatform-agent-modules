//go:build !windows

package modulesdk

import (
	"path/filepath"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/handshake"
)

func TestServeFailsWhenSocketDirIsMissing(t *testing.T) {
	coreEnv(t, newFakeHost(t))
	t.Setenv(handshake.EnvSocketDir, filepath.Join(shortDir(t), "absent"))
	if err := serve(&fakeModule{}, discardLog()); err == nil {
		t.Fatal("serve listened in a missing directory")
	}
}

func TestServeFailsWithoutSocketDir(t *testing.T) {
	coreEnv(t, newFakeHost(t))
	t.Setenv(handshake.EnvSocketDir, "")
	if err := serve(&fakeModule{}, discardLog()); err == nil {
		t.Fatal("serve ran without a socket dir")
	}
}
