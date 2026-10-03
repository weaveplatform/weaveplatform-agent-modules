package testkit

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/handshake"
)

// envFakeModule turns the test binary into a misbehaving module: Launch's
// failure paths need a child that exits early, hangs, or answers badly, and
// re-executing this binary avoids building one per behaviour.
const envFakeModule = "TESTKIT_FAKE_MODULE"

func TestMain(m *testing.M) {
	switch os.Getenv(envFakeModule) {
	case "":
		os.Exit(m.Run())
	case "exit":
		os.Exit(3)
	case "hang":
		time.Sleep(time.Minute)
	case "badline":
		fmt.Println("HELLO|not|a|handshake")
		time.Sleep(time.Minute)
	case "wrongproto":
		fmt.Println(handshake.Line{Protocol: 5, Network: "unix", Addr: "/nowhere"}.Format())
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func fakeModule(t *testing.T, mode string) string {
	t.Helper()
	t.Setenv(envFakeModule, mode)
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return bin
}
