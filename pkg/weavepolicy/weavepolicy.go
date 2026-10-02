// Package weavepolicy is the machine-side control on what a controller may run
// on a machine, plus the audit record of what it actually ran.
//
// # Posture
//
// A request reaches exec only over core's host channel, from the host directly
// outside the machine — a hypervisor for a VM, a container runtime for a
// container — and only after that host has proved the machine's channel key.
// The asker already controls the machine outright (it can power it off, or
// discard it), so exec is not an open surface and exec policy is defence in
// depth, not the primary control. That shapes the default:
//
//   - No policy document means ALLOW, audited. A machine with no policy server
//     must still be driveable by whoever controls it; failing closed here would
//     brick every disconnected machine, which is most machines during setup —
//     the exact moment exec matters most.
//   - A policy document that IS present is authoritative and enforced fail-
//     closed: anything it does not permit is refused, and a document that
//     cannot be parsed refuses everything rather than silently reverting to
//     allow-all. A policy that fails open is not a policy.
//
// Every invocation is audited regardless, before the process starts, so a
// permissive policy still leaves a record.
package weavepolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Policy is the module-scoped exec policy, as delivered by core in a
// modulesdk.PolicyDocument. Every field is optional; an omitted field places no
// constraint, so a document can tighten one thing without restating the rest.
type Policy struct {
	// AllowExec disables exec entirely when explicitly false.
	AllowExec *bool `json:"allow_exec,omitempty"`
	// AllowArgv0, when non-empty, is a permit-list of glob patterns matched
	// against the program being run. Anything unmatched is refused.
	AllowArgv0 []string `json:"allow_argv0,omitempty"`
	// DenyArgv0 patterns are refused even if AllowArgv0 permits them. Deny
	// beats allow, so a broad permit can be narrowed without rewriting it.
	DenyArgv0 []string `json:"deny_argv0,omitempty"`
	// AllowDirs, when non-empty, restricts the working directory to these
	// paths or their descendants.
	AllowDirs []string `json:"allow_dirs,omitempty"`
	// AllowTTY disables pseudo-terminal execs when explicitly false, which is
	// how interactive shells are kept out of an otherwise automated guest.
	AllowTTY *bool `json:"allow_tty,omitempty"`
	// MaxOutputBytes caps the combined stdout+stderr a single exec may stream.
	// Zero means no cap. It bounds a runaway process that would otherwise fill
	// the channel indefinitely.
	MaxOutputBytes int64 `json:"max_output_bytes,omitempty"`
}

// Parse reads a policy document.
//
// An EMPTY document is the no-policy case and permits everything; a MALFORMED
// one is a policy that could not be understood, which is refused wholesale by
// the returned error. Collapsing those two would turn a typo into an open door.
func Parse(doc []byte) (Policy, error) {
	if len(strings.TrimSpace(string(doc))) == 0 {
		return Policy{}, nil
	}
	var p Policy
	if err := json.Unmarshal(doc, &p); err != nil {
		return Policy{}, fmt.Errorf("weavepolicy: unreadable exec policy: %w", err)
	}
	return p, nil
}

// ErrRefused is wrapped by every refusal CheckExec returns, so a caller can
// tell "policy said no" from a policy that could not be read.
var ErrRefused = errors.New("weavepolicy: refused")

// ExecRequest is what CheckExec judges.
type ExecRequest struct {
	Argv []string
	Dir  string
	TTY  bool
}

// CheckExec reports whether the request is permitted, returning the reason it
// was not. The reason is sent back to the host verbatim: an operator whose
// command was refused needs to know which rule refused it.
func (p Policy) CheckExec(req ExecRequest) error {
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		return fmt.Errorf("%w: no program to run", ErrRefused)
	}
	if p.AllowExec != nil && !*p.AllowExec {
		return fmt.Errorf("%w: exec is disabled by policy", ErrRefused)
	}
	if req.TTY && p.AllowTTY != nil && !*p.AllowTTY {
		return fmt.Errorf("%w: interactive (tty) exec is disabled by policy", ErrRefused)
	}

	program := req.Argv[0]
	// Deny is checked first and wins: a permit-list broadened over time should
	// not quietly re-admit something explicitly excluded.
	for _, pattern := range p.DenyArgv0 {
		if matchProgram(pattern, program) {
			return fmt.Errorf(
				"%w: %q is denied by policy (matched %q)",
				ErrRefused,
				program,
				pattern,
			)
		}
	}
	if len(p.AllowArgv0) > 0 {
		var permitted bool
		for _, pattern := range p.AllowArgv0 {
			if matchProgram(pattern, program) {
				permitted = true
				break
			}
		}
		if !permitted {
			return fmt.Errorf("%w: %q is not in the policy's allowed programs", ErrRefused, program)
		}
	}

	if len(p.AllowDirs) > 0 && req.Dir != "" {
		if !dirPermitted(p.AllowDirs, req.Dir) {
			return fmt.Errorf(
				"%w: working directory %q is not permitted by policy",
				ErrRefused,
				req.Dir,
			)
		}
	}
	return nil
}

// matchProgram tests a pattern against both the full program string and its
// base name, so a policy can say "curl" without knowing whether the host will
// send /usr/bin/curl. A pattern containing a separator is matched only against
// the full path — that is how a policy pins one specific binary.
func matchProgram(pattern, program string) bool {
	if strings.ContainsAny(pattern, `/\`) {
		ok, err := filepath.Match(pattern, filepath.ToSlash(program))
		return err == nil && ok
	}
	ok, err := filepath.Match(pattern, filepath.Base(program))
	return err == nil && ok
}

// dirPermitted reports whether dir is one of the allowed roots or inside one.
//
// Comparison is on cleaned paths so ".." cannot walk out of a permitted root,
// which is the whole point of the check.
func dirPermitted(allowed []string, dir string) bool {
	clean := filepath.Clean(dir)
	for _, root := range allowed {
		root = filepath.Clean(root)
		if clean == root {
			return true
		}
		rel, err := filepath.Rel(root, clean)
		if err != nil {
			continue
		}
		// Inside the root iff the relative path does not climb out of it.
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// AuditTopic is the event topic exec audit records are published on. Core
// prefixes it with the module id.
const AuditTopic = "exec.audit"

// ExecAudit is the record written for every invocation, permitted or refused,
// BEFORE the process starts.
//
// Writing it first is deliberate: a record written on completion is missing
// exactly the executions that mattered — the one that hung, the one that took
// the guest down, the one still running when the channel dropped.
type ExecAudit struct {
	At       time.Time `json:"at"`
	ExecID   string    `json:"exec_id"`
	Argv     []string  `json:"argv"`
	Dir      string    `json:"dir,omitempty"`
	TTY      bool      `json:"tty,omitempty"`
	Refused  string    `json:"refused,omitempty"`
	Policy   bool      `json:"policy_present"`
	ExitCode *int      `json:"exit_code,omitempty"`
}
