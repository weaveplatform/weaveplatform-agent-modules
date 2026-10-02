package guestmodule_test

import (
	"strings"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-core/sdk/manifest"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestmodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/guestwire"
)

func execManifest(id, address string, platforms ...manifest.Platform) *manifest.Manifest {
	return &manifest.Manifest{
		Schema: 1, ID: id, Address: address, Version: "0.1.0", Protocol: 1,
		Zone: "A", Privilege: manifest.PrivilegeSystem, Session: manifest.SessionSystem,
		Platforms: platforms,
	}
}

func TestCheckManifest(t *testing.T) {
	svc := &echoService{capability: guestwire.Exec}
	linux := []manifest.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}}

	if err := guestmodule.CheckManifest(
		execManifest("guestweave-linux-exec", "guestweave.exec", linux...),
		svc,
	); err != nil {
		t.Fatalf("a correct manifest failed: %v", err)
	}
	if err := guestmodule.CheckManifest(execManifest("guestweave-macos-exec", "guestweave.exec",
		manifest.Platform{OS: "darwin", Arch: "arm64"}), svc); err != nil {
		t.Fatalf("macOS maps to darwin: %v", err)
	}

	for name, tc := range map[string]struct {
		m    *manifest.Manifest
		want string
	}{
		"no address":       {execManifest("guestweave-linux-exec", "", linux...), "answers to"},
		"wrong address":    {execManifest("guestweave-linux-exec", "guestweave.power", linux...), "answers to"},
		"wrong id":         {execManifest("guestweave-exec", "guestweave.exec", linux...), "is not"},
		"wrong capability": {execManifest("guestweave-linux-power", "guestweave.exec", linux...), "is not"},
		"wrong platform": {execManifest("guestweave-linux-exec", "guestweave.exec",
			manifest.Platform{OS: "windows", Arch: "amd64"}), "lists platform"},
		"invalid": {execManifest("guestweave-linux-exec", "guestweave.exec"), "platform required"},
	} {
		err := guestmodule.CheckManifest(tc.m, svc)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

// Every capability address is one core's manifest validation accepts, so no
// module is ever unable to declare its own.
func TestEveryCapabilityAddressIsAValidManifestAddress(t *testing.T) {
	for _, c := range guestwire.Capabilities() {
		m := execManifest(
			guestmodule.ModuleID("linux", c),
			c.Address(),
			manifest.Platform{OS: "linux", Arch: "amd64"},
		)
		if err := m.Validate(); err != nil {
			t.Errorf("%s: %v", c, err)
		}
		if m.ChannelAddress() != c.Address() {
			t.Errorf("%s: core would route it under %q", c, m.ChannelAddress())
		}
	}
}
