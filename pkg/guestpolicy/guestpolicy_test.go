package guestpolicy

import (
	"path/filepath"
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

// No policy permits everything. A guest mid-setup has never spoken to a policy
// server, and that is exactly when exec matters most — failing closed here would
// brick every disconnected guest.
func TestAbsentPolicyPermits(t *testing.T) {
	for _, doc := range [][]byte{nil, {}, []byte("  \n ")} {
		p, err := Parse(doc)
		if err != nil {
			t.Fatalf("Parse(%q): %v", doc, err)
		}
		if err := p.CheckExec(
			ExecRequest{Argv: []string{"/bin/sh", "-c", "anything"}},
		); err != nil {
			t.Fatalf("an absent policy refused an exec: %v", err)
		}
	}
}

// A policy that cannot be parsed must refuse, not fall back to permitting. This
// is the one failure mode a policy must not have: a typo becoming an open door.
func TestMalformedPolicyIsRefusedNotIgnored(t *testing.T) {
	if _, err := Parse([]byte("{not json")); err == nil {
		t.Fatal("a malformed policy parsed as permissive")
	}
}

func TestAllowListPermitsOnlyListedPrograms(t *testing.T) {
	p := Policy{AllowArgv0: []string{"systemctl", "apt-get"}}

	if err := p.CheckExec(ExecRequest{Argv: []string{"systemctl", "status"}}); err != nil {
		t.Fatalf("a listed program was refused: %v", err)
	}
	// A path form of a listed program is permitted: the host cannot be
	// expected to know whether a guest resolves it via PATH.
	if err := p.CheckExec(ExecRequest{Argv: []string{"/usr/bin/systemctl", "status"}}); err != nil {
		t.Fatalf("a listed program was refused by path: %v", err)
	}
	err := p.CheckExec(ExecRequest{Argv: []string{"curl", "http://example.invalid"}})
	if err == nil {
		t.Fatal("an unlisted program was permitted")
	}
	if !strings.Contains(err.Error(), "curl") {
		t.Fatalf("the refusal does not name the program: %v", err)
	}
}

// Deny beats allow, so a permit-list broadened over time cannot quietly
// re-admit something explicitly excluded.
func TestDenyBeatsAllow(t *testing.T) {
	p := Policy{AllowArgv0: []string{"*"}, DenyArgv0: []string{"rm"}}
	if err := p.CheckExec(ExecRequest{Argv: []string{"ls"}}); err != nil {
		t.Fatalf("a permitted program was refused: %v", err)
	}
	if err := p.CheckExec(ExecRequest{Argv: []string{"/bin/rm", "-rf", "/"}}); err == nil {
		t.Fatal("a denied program was permitted because the allow-list was broad")
	}
}

// A pattern containing a separator pins one specific binary rather than
// matching any program with that name.
func TestPathPatternPinsTheBinary(t *testing.T) {
	p := Policy{AllowArgv0: []string{"/usr/bin/*"}}
	if err := p.CheckExec(ExecRequest{Argv: []string{"/usr/bin/uptime"}}); err != nil {
		t.Fatalf("a matching path was refused: %v", err)
	}
	if err := p.CheckExec(ExecRequest{Argv: []string{"/tmp/uptime"}}); err == nil {
		t.Fatal("a same-named binary elsewhere was permitted by a path pattern")
	}
}

// The directory check exists to stop a caller escaping a permitted root, so a
// traversal must not slip through.
func TestWorkingDirectoryCannotEscapeAPermittedRoot(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "srv", "weave")
	p := Policy{AllowDirs: []string{root}}

	for _, dir := range []string{root, filepath.Join(root, "sub", "deeper")} {
		if err := p.CheckExec(ExecRequest{Argv: []string{"ls"}, Dir: dir}); err != nil {
			t.Fatalf("a permitted directory %q was refused: %v", dir, err)
		}
	}
	for _, dir := range []string{
		filepath.Join(root, "..", "elsewhere"),
		filepath.Join(string(filepath.Separator), "etc"),
		filepath.Join(root, "..", "..", "etc"),
	} {
		if err := p.CheckExec(ExecRequest{Argv: []string{"ls"}, Dir: dir}); err == nil {
			t.Fatalf("%q escaped the permitted root", dir)
		}
	}
}

// A root whose name merely prefixes another must not be treated as containing
// it: /srv/weave does not permit /srv/weave-secrets.
func TestSiblingWithASharedPrefixIsNotInsideTheRoot(t *testing.T) {
	p := Policy{AllowDirs: []string{filepath.Join(string(filepath.Separator), "srv", "weave")}}
	sibling := filepath.Join(string(filepath.Separator), "srv", "weave-secrets")
	if err := p.CheckExec(ExecRequest{Argv: []string{"ls"}, Dir: sibling}); err == nil {
		t.Fatalf("%q was treated as inside the permitted root", sibling)
	}
}

func TestExecCanBeDisabledEntirely(t *testing.T) {
	p := Policy{AllowExec: ptr(false)}
	if err := p.CheckExec(ExecRequest{Argv: []string{"ls"}}); err == nil {
		t.Fatal("exec ran with allow_exec false")
	}
}

// Disabling interactive execs is how a shell is kept out of an otherwise
// automated guest — while ordinary commands keep working.
func TestTTYCanBeDisabledWithoutDisablingExec(t *testing.T) {
	p := Policy{AllowTTY: ptr(false)}
	if err := p.CheckExec(ExecRequest{Argv: []string{"ls"}}); err != nil {
		t.Fatalf("a non-interactive exec was refused: %v", err)
	}
	if err := p.CheckExec(ExecRequest{Argv: []string{"bash"}, TTY: true}); err == nil {
		t.Fatal("an interactive exec ran with allow_tty false")
	}
}

func TestEmptyArgvIsRefused(t *testing.T) {
	if err := (Policy{}).CheckExec(ExecRequest{}); err == nil {
		t.Fatal("an exec with no program was permitted")
	}
	if err := (Policy{}).CheckExec(ExecRequest{Argv: []string{""}}); err == nil {
		t.Fatal("an exec with an empty program was permitted")
	}
}

// An unconstrained field must place no constraint: a document that sets only
// max_output_bytes must not accidentally deny every program.
func TestOmittedFieldsConstrainNothing(t *testing.T) {
	p, err := Parse([]byte(`{"max_output_bytes": 1024}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.CheckExec(ExecRequest{Argv: []string{"anything"}, TTY: true}); err != nil {
		t.Fatalf("a partial policy refused an exec: %v", err)
	}
	if p.MaxOutputBytes != 1024 {
		t.Fatalf("MaxOutputBytes = %d", p.MaxOutputBytes)
	}
}
