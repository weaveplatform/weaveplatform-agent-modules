package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/modulesdk"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/protocol/manifest"
)

// EnvAgentDir names a directory holding a released weave-agent, weavectl and
// weavemanifest. TestUnderReleasedAgent runs only when it is set; the compat
// workflow sets it to the extracted release named in
// .github/agent-core-version.
const EnvAgentDir = "WEAVE_AGENT_DIR"

// TestUnderReleasedAgent runs the fixture under a real, released core and
// waits for core itself to report it running and healthy over its control
// socket: the module side of this sdk against the core side of the protocol,
// with nothing of either side stubbed. The fixture reports healthy only once
// core's RegistryService has listed it, so healthy also proves the registry
// host service and this sdk's client of it agree.
//
// A release core verifies every module before exec. It does so with the
// platform's code signature (codesign with a pinned Apple team, Authenticode
// with a pinned certificate, root-only ownership on Linux) unless it is given
// a signed channel manifest to verify against instead: --channel-dir with
// --manifest-root-pub, the offline-install path. That second path is the
// same on every OS and needs no platform signing identity, so the test mints
// a root key and a signing key with core's own weavemanifest, signs a channel
// manifest naming the fixture binary's SHA-256, and points core at it. Core
// still refuses anything the channel does not name, so the check is real.
func TestUnderReleasedAgent(t *testing.T) {
	dir := os.Getenv(EnvAgentDir)
	if dir == "" {
		t.Skipf("%s is unset: no released weave-agent to run under", EnvAgentDir)
	}
	tools := agentTools{dir: dir}

	// Short root: core puts each module's host socket four directories
	// down, and a unix socket path must fit sun_path (104 bytes on macOS),
	// which macOS's per-user $TMPDIR alone nearly fills.
	base := ""
	if runtime.GOOS != "windows" {
		base = "/tmp"
	}
	root, err := os.MkdirTemp(base, "wvc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	state := filepath.Join(root, "state")
	keys := filepath.Join(root, "keys")
	channel := filepath.Join(root, "channel")
	modDir := filepath.Join(state, "modules", ModuleID)
	for _, d := range []string{keys, channel, modDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	bin := filepath.Join(modDir, ModuleID+exeSuffix())
	buildFixture(t, bin)
	writeModuleManifest(t, filepath.Join(modDir, "module.manifest.json"))

	version := strings.TrimSpace(tools.run(t, "weave-agent", "-version"))
	t.Logf("weave-agent %s", version)
	signChannel(t, tools, keys, channel, version, bin)

	logPath := filepath.Join(root, "weave-agent.log")
	stop := tools.startAgent(t, state, channel, filepath.Join(keys, "root.pub"), logPath)
	defer stop()

	t.Log(tools.ctl(t, state, "status"))
	deadline := time.Now().Add(90 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = tools.ctlMaybe(state, "modules")
		state, health, ok := moduleRow(last, ModuleID)
		if ok && state == "running" && strings.HasPrefix(health, "STATUS_HEALTHY") {
			t.Logf("core reports %s running and healthy:\n%s", ModuleID, last)
			return
		}
		// States core does not leave on its own: waiting out the deadline
		// would only delay the same failure.
		if ok && (strings.HasPrefix(state, "start-limited") ||
			strings.HasPrefix(state, "unsupported-protocol") ||
			strings.HasPrefix(state, "requirements-unmet")) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	logs, _ := os.ReadFile(logPath)
	t.Fatalf(
		"%s never reached running and healthy; last weavectl modules:\n%s\nweave-agent log:\n%s",
		ModuleID,
		last,
		logs,
	)
}

// writeModuleManifest writes the fixture's manifest with this sdk's types,
// so core parsing it is part of what the test checks.
func writeModuleManifest(t *testing.T, path string) {
	t.Helper()
	m := manifest.Manifest{
		Schema:    1,
		ID:        ModuleID,
		Version:   "0.1.0",
		Protocol:  modulesdk.Protocol,
		Zone:      "A",
		Privilege: manifest.PrivilegeSystem,
		Session:   manifest.SessionSystem,
		Platforms: []manifest.Platform{{OS: runtime.GOOS, Arch: runtime.GOARCH}},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("fixture manifest: %v", err)
	}
	writeJSON(t, path, m)
}

// signChannel mints a root and a signing key with core's weavemanifest,
// endorses the signing key, and signs a channel manifest that names the
// fixture binary by digest. The file names are the ones weave-agent reads
// from --channel-dir.
func signChannel(t *testing.T, tools agentTools, keys, channel, coreVersion, bin string) {
	t.Helper()
	rootKey := filepath.Join(keys, "root")
	signingKey := filepath.Join(keys, "signing")
	tools.run(t, "weavemanifest", "keygen", "root", rootKey)
	tools.run(t, "weavemanifest", "keygen", "compat", signingKey)
	tools.run(t, "weavemanifest", "endorse", rootKey+".key", signingKey+".pub")
	copyFile(t, signingKey+".pub", filepath.Join(channel, "signing.pub"))
	copyFile(t, signingKey+".pub.sig", filepath.Join(channel, "signing.pub.sig"))

	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	doc := manifest.ChannelManifest{
		Schema:      1,
		Channel:     "compat",
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Sequence:    1,
		Protocol:    manifest.ProtocolWindow{Min: modulesdk.Protocol, Max: modulesdk.Protocol},
		Core:        manifest.ChannelCore{Version: strings.TrimPrefix(coreVersion, "v")},
		Modules: []manifest.ChannelModule{{
			ID:        ModuleID,
			Version:   "0.1.0",
			Protocol:  modulesdk.Protocol,
			Privilege: manifest.PrivilegeSystem,
			Session:   manifest.SessionSystem,
			Artifacts: []manifest.ChannelArtifact{{
				OS:     runtime.GOOS,
				Arch:   runtime.GOARCH,
				URL:    "file://" + filepath.ToSlash(bin),
				Digest: "sha256:" + hex.EncodeToString(sum[:]),
				Size:   int64(len(data)),
			}},
		}},
	}
	path := filepath.Join(channel, "channel.json")
	writeJSON(t, path, doc)
	if _, err := manifest.ParseChannel(mustRead(t, path)); err != nil {
		t.Fatalf("channel manifest: %v", err)
	}
	tools.run(t, "weavemanifest", "sign", signingKey+".key", path)
	// The chain exactly as core will check it, so a signing mistake fails
	// here with weavemanifest's message rather than as a module core refuses.
	tools.run(
		t,
		"weavemanifest",
		"verify",
		rootKey+".pub",
		filepath.Join(channel, "signing.pub"),
		path,
	)
}

// moduleRow finds id in `weavectl modules` output and returns its STATE and
// HEALTH columns, located by the header so a column core adds (v0.9.2 added
// ADDRESS) does not shift them. HEALTH is last and may contain spaces
// ("STATUS_DEGRADED (reason)").
func moduleRow(out, id string) (state, health string, ok bool) {
	stateCol, healthCol := -1, -1
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		if len(f) > 0 && f[0] == "MODULE" {
			stateCol = slices.Index(f, "STATE")
			healthCol = slices.Index(f, "HEALTH")
			continue
		}
		if stateCol < 0 || healthCol < 0 || len(f) <= healthCol || f[0] != id {
			continue
		}
		return f[stateCol], strings.Join(f[healthCol:], " "), true
	}
	return "", "", false
}

type agentTools struct{ dir string }

func (a agentTools) path(name string) string {
	return filepath.Join(a.dir, name+exeSuffix())
}

func (a agentTools) cmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, a.path(name), args...)
	cmd.Env = append(os.Environ(), "WEAVE_LOG_LEVEL=debug")
	return cmd
}

func (a agentTools) run(t *testing.T, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := a.cmd(ctx, name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// ctl runs weavectl against the core whose state directory is state; the
// layout (and so the control socket) comes from WEAVE_STATE_DIR, exactly as
// core resolves it.
func (a agentTools) ctl(t *testing.T, state string, args ...string) string {
	t.Helper()
	out, err := a.ctlRun(state, args...)
	if err != nil {
		t.Fatalf("weavectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// ctlMaybe is ctl for polling: core may not be listening yet.
func (a agentTools) ctlMaybe(state string, args ...string) string {
	out, _ := a.ctlRun(state, args...)
	return out
}

func (a agentTools) ctlRun(state string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := a.cmd(ctx, "weavectl", args...)
	cmd.Env = append(cmd.Env, "WEAVE_STATE_DIR="+state)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// startAgent runs weave-agent until the returned stop is called, and waits
// for its control socket to answer.
func (a agentTools) startAgent(
	t *testing.T,
	state, channel, rootPub, logPath string,
) (stop func()) {
	t.Helper()
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := a.cmd(ctx, "weave-agent",
		"--channel-dir", channel,
		"--manifest-root-pub", rootPub,
	)
	cmd.Env = append(cmd.Env, "WEAVE_STATE_DIR="+state)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("starting weave-agent: %v", err)
	}
	stop = func() {
		// Windows has no SIGTERM to send a console process; killing it is
		// what the context does everywhere, and the test is done with it.
		cancel()
		cmd.Wait() //nolint:errcheck // killed on purpose
		logFile.Close()
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := a.ctlRun(state, "status"); err == nil {
			return stop
		}
		if time.Now().After(deadline) {
			stop()
			logs, _ := os.ReadFile(logPath)
			t.Fatalf("weave-agent's control socket never answered; log:\n%s", logs)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	if err := os.WriteFile(to, mustRead(t, from), 0o600); err != nil {
		t.Fatal(err)
	}
}
