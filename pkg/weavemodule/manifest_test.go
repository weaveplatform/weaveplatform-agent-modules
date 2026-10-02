package weavemodule_test

import (
	"strings"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-core/sdk/manifest"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

func execManifest(id, address string, platforms ...manifest.Platform) *manifest.Manifest {
	return &manifest.Manifest{
		Schema: 1, ID: id, Address: address, Version: "0.1.0", Protocol: 1,
		Zone: "A", Privilege: manifest.PrivilegeSystem, Session: manifest.SessionSystem,
		Platforms: platforms,
	}
}

func TestCheckManifest(t *testing.T) {
	svc := &echoService{capability: weavewire.Exec}
	linux := []manifest.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}}

	if err := weavemodule.CheckManifest(
		execManifest("weave-linux-exec", "weave.exec", linux...),
		svc,
	); err != nil {
		t.Fatalf("a correct manifest failed: %v", err)
	}
	if err := weavemodule.CheckManifest(execManifest("weave-macos-exec", "weave.exec",
		manifest.Platform{OS: "darwin", Arch: "arm64"}), svc); err != nil {
		t.Fatalf("macOS maps to darwin: %v", err)
	}

	for name, tc := range map[string]struct {
		m    *manifest.Manifest
		want string
	}{
		"no address":       {execManifest("weave-linux-exec", "", linux...), "answers to"},
		"wrong address":    {execManifest("weave-linux-exec", "weave.power", linux...), "answers to"},
		"wrong id":         {execManifest("weave-exec", "weave.exec", linux...), "is not"},
		"wrong capability": {execManifest("weave-linux-power", "weave.exec", linux...), "is not"},
		"wrong platform": {execManifest("weave-linux-exec", "weave.exec",
			manifest.Platform{OS: "windows", Arch: "amd64"}), "lists platform"},
		"invalid": {execManifest("weave-linux-exec", "weave.exec"), "platform required"},
	} {
		err := weavemodule.CheckManifest(tc.m, svc)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

// Every capability address is one core's manifest validation accepts, so no
// module is ever unable to declare its own.
func TestEveryCapabilityAddressIsAValidManifestAddress(t *testing.T) {
	for _, c := range weavewire.Capabilities() {
		m := execManifest(
			weavemodule.ModuleID("linux", c),
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
