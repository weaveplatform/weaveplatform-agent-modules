//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// logind reads systemd-logind's state files under /run/systemd: the seat file
// names the active session, each session file says whose it is, what kind and
// in what state.
//
// The files, not org.freedesktop.login1 over D-Bus, for the reasons agent-core
// reads them too: they hold what logind serves (it rewrites them on every
// change), reading them needs no D-Bus client, and a fake is a directory of
// text files. Reading them the same way also means this module and core agree
// on who is at the console — the session core starts the console modules in.
// systemd marks them "private, do not parse"; the keys used here have been
// stable since logind shipped, and a format change would surface as "nobody
// logged in", not as a wrong answer.
//
// What the files lack is the lock state (session_save does not write
// LockedHint), and locking is a request to logind rather than a read, so both
// go through loginctl.
type logind struct {
	// root is the filesystem root the state paths are resolved under: "/" in
	// production, a fixture directory in tests.
	root string
	// seat is the seat whose active session is the console; seat0 is the
	// only one with a physical console.
	seat string
	// loginctl runs loginctl with args and returns its standard output.
	loginctl func(ctx context.Context, args ...string) ([]byte, error)
}

func newLogind() logind { return logind{root: "/", seat: "seat0", loginctl: runLoginctl} }

func runLoginctl(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "loginctl", args...).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return nil, fmt.Errorf("loginctl %s: %w: %s",
				strings.Join(args, " "), err, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("loginctl %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// Console implements weavesession.Backend: the active session on the seat,
// when it is a local user's.
func (l logind) Console(ctx context.Context) (weavewire.SessionInfo, bool, error) {
	id, err := l.activeOnSeat()
	if err != nil || id == "" {
		return weavewire.SessionInfo{}, false, err
	}
	sv, err := readEnvFile(l.path("run/systemd/sessions", id))
	if errors.Is(err, fs.ErrNotExist) {
		return weavewire.SessionInfo{}, false, nil // ended between the two reads
	}
	if err != nil {
		return weavewire.SessionInfo{}, false, err
	}
	// A greeter (gdm, sddm) is a session on seat0 too, owned by a display-
	// manager account; it is the login screen, not a user. A remote session
	// cannot be the seat's active one in practice, but if logind ever said
	// so it would still not be the physical console.
	if sv["CLASS"] != "user" || sv["STATE"] != "active" || sv["REMOTE"] == "1" {
		return weavewire.SessionInfo{}, false, nil
	}
	info, err := l.info(ctx, id, sv)
	if err != nil {
		return weavewire.SessionInfo{}, false, err
	}
	return info, true, nil
}

// activeOnSeat is the seat file's ACTIVE session, or "" when there is no seat
// (a server or container: nobody can be at a console that does not exist).
func (l logind) activeOnSeat() (string, error) {
	seatVars, err := readEnvFile(l.path("run/systemd/seats", l.seat))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	id := seatVars["ACTIVE"]
	if !validID(id) {
		return "", nil
	}
	return id, nil
}

// List implements weavesession.Lister: every user session logind tracks,
// local or remote, that is not on its way out.
func (l logind) List(ctx context.Context) ([]weavewire.SessionInfo, error) {
	dir := l.path("run/systemd/sessions")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil // no logind: no sessions it could report
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	console, err := l.activeOnSeat()
	if err != nil {
		return nil, err
	}
	var out []weavewire.SessionInfo
	for _, e := range entries {
		// <id>.ref files are the FIFOs logind holds to notice a session's
		// leader going away; only regular files are session records.
		if !e.Type().IsRegular() || strings.HasSuffix(e.Name(), ".ref") || !validID(e.Name()) {
			continue
		}
		sv, err := readEnvFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // ended while the directory was being read
		}
		if sv["CLASS"] != "user" || sv["STATE"] == "closing" {
			continue
		}
		info, err := l.info(ctx, e.Name(), sv)
		if err != nil {
			continue // a malformed record costs that session, not the list
		}
		info.Console = e.Name() == console && sv["STATE"] == "active" && !info.Remote
		out = append(out, info)
	}
	slices.SortFunc(
		out,
		func(a, b weavewire.SessionInfo) int { return strings.Compare(a.ID, b.ID) },
	)
	return out, nil
}

// errBadRecord reports a session file that does not say whose session it is.
var errBadRecord = errors.New("malformed logind session record")

// info turns a session file into a SessionInfo.
func (l logind) info(
	ctx context.Context,
	id string,
	sv map[string]string,
) (weavewire.SessionInfo, error) {
	uid, err := strconv.ParseUint(sv["UID"], 10, 32)
	if err != nil {
		return weavewire.SessionInfo{}, fmt.Errorf(
			"%w: session %s has UID %q",
			errBadRecord,
			id,
			sv["UID"],
		)
	}
	if sv["USER"] == "" {
		return weavewire.SessionInfo{}, fmt.Errorf("%w: session %s has no USER", errBadRecord, id)
	}
	info := weavewire.SessionInfo{
		ID:     id,
		User:   sv["USER"],
		UID:    strconv.FormatUint(uid, 10),
		Remote: sv["REMOTE"] == "1",
		State:  weavewire.SessionInactive,
	}
	if us, err := strconv.ParseInt(sv["REALTIME"], 10, 64); err == nil && us > 0 {
		info.Since = time.UnixMicro(us).UTC()
	}
	// logind's "online" is a session logged in but not in the foreground of
	// its seat (switched away from, or a seatless one such as SSH, which is
	// never "active"); only "active" is in front of someone.
	if sv["STATE"] == "active" {
		info.State = weavewire.SessionActive
		if l.locked(ctx, id) {
			info.State = weavewire.SessionLocked
		}
	}
	return info, nil
}

// locked asks logind for the session's LockedHint, which the desktop's screen
// locker sets. Best effort: without loginctl, or from a desktop that never
// sets the hint, an active session reads as unlocked, which is what logind
// itself would report.
func (l logind) locked(ctx context.Context, id string) bool {
	out, err := l.loginctl(ctx, "show-session", id, "--property=LockedHint", "--value")
	return err == nil && strings.TrimSpace(string(out)) == "yes"
}

// errBadID reports a session id that is not one logind would mint.
var errBadID = errors.New("not a logind session id")

// Lock implements weavesession.Locker: logind asks the session's screen
// locker to lock, as `loginctl lock-session` does from a terminal.
func (l logind) Lock(ctx context.Context, sessionID string) error {
	// The id reaches loginctl's argument list; one starting with "-" would
	// be read as an option, and anything else non-alphanumeric is not an id
	// logind issues.
	if !validID(sessionID) {
		return fmt.Errorf("%w: %q", errBadID, sessionID)
	}
	if _, err := l.loginctl(ctx, "lock-session", sessionID); err != nil {
		return err
	}
	return nil
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]*$`)

func validID(id string) bool { return idRe.MatchString(id) }

func (l logind) path(elem ...string) string {
	return filepath.Join(append([]string{l.root}, elem...)...)
}

// readEnvFile parses systemd's KEY=VALUE state-file format.
func readEnvFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: paths under the logind state directory
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return parseEnv(string(b)), nil
}

func parseEnv(s string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			// A value with characters a shell would split on is written
			// double-quoted; none of the keys read here needs more than
			// the quotes removed.
			if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
				v = v[1 : len(v)-1]
			}
			out[k] = v
		}
	}
	return out
}
