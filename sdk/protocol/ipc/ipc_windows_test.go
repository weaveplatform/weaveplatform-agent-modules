package ipc

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/deploymenttheory/go-bindings-win32/bindings/win32/foundation"
	winsec "github.com/deploymenttheory/go-bindings-win32/bindings/win32/security"
	"golang.org/x/sys/windows"
)

// badAddr returns a pipe name already held by another listener: creating the
// first instance of a name that exists fails, which is a more dependable
// error than guessing which malformed names the kernel rejects.
func badAddr(t *testing.T) string {
	t.Helper()
	addr := testAddr(t)
	l, err := Listen(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return addr
}

// Named pipes carry no uid: access is the SDDL's job.
func checkPeerCred(t *testing.T, pc PeerCred) {
	t.Helper()
	if pc != (PeerCred{}) {
		t.Fatalf("peer cred = %+v, want zero on Windows", pc)
	}
}

func TestListenPipeSDDL(t *testing.T) {
	addr := testAddr(t)
	l, err := ListenPipeSDDL(addr, "D:(A;;GA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		if c, err := l.Accept(); err == nil {
			c.Close()
		}
	}()
	c, err := Dial(dialCtx(t), NetworkPipe, addr)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestListenRejectsTakenPipeName(t *testing.T) {
	if _, err := Listen(badAddr(t)); err == nil {
		t.Fatal("Listen took over a pipe name already in use")
	}
}

func TestPipeListenerCloseIsIdempotentAndFinal(t *testing.T) {
	l, err := Listen(testAddr(t))
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		accepted <- err
	}()
	// Close while an Accept waits: the case go-winio can wedge on.
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > pipeCloseTimeout+time.Second {
		t.Fatalf("Close took %v", d)
	}
	select {
	case err := <-accepted:
		if err == nil {
			t.Fatal("pending Accept succeeded after Close")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pending Accept never returned")
	}
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept after Close = %v, want net.ErrClosed", err)
	}
}

func TestOwnPipeSDDLGrantsProcessUser(t *testing.T) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sddl, err := ownPipeSDDL()
	if err != nil {
		t.Fatal(err)
	}
	want := pipeSDDL + "(A;;GA;;;" + u.User.Sid.String() + ")"
	if u.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		want = pipeSDDL
	}
	if sddl != want {
		t.Fatalf("ownPipeSDDL = %q, want %q", sddl, want)
	}
}

// Running as SYSTEM, the user is already in the descriptor.
func TestOwnPipeSDDLAsSystem(t *testing.T) {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	orig := tokenUser
	t.Cleanup(func() { tokenUser = orig })
	tokenUser = func() (*windows.SID, error) { return system, nil }
	sddl, err := ownPipeSDDL()
	if err != nil || sddl != pipeSDDL {
		t.Fatalf("ownPipeSDDL as SYSTEM = %q, %v; want %q", sddl, err, pipeSDDL)
	}
}

func TestListenWithoutTokenUserFails(t *testing.T) {
	orig := tokenUser
	t.Cleanup(func() { tokenUser = orig })
	boom := errors.New("boom")
	tokenUser = func() (*windows.SID, error) { return nil, boom }
	if _, err := Listen(testAddr(t)); !errors.Is(err, boom) {
		t.Fatalf("Listen = %v, want %v", err, boom)
	}
}

// nonAdminHelperEnv selects what TestListenAsNonAdminUser does when it is
// the child it spawned.
const nonAdminHelperEnv = "WEAVE_IPC_NONADMIN_HELPER"

// A module that core launches as the console user runs without SYSTEM and,
// under UAC, with Administrators deny-only. Such a process must be able to
// serve its own pipe: go-winio opens a further instance on every Accept,
// and that open is checked against the pipe's descriptor. The test runs a
// child of itself with Administrators disabled in its token, so the case
// holds on a runner that is an administrator. The child also shows that the
// SYSTEM-and-Administrators descriptor alone fails there, so the token
// really is the one that broke per-user modules.
func TestListenAsNonAdminUser(t *testing.T) {
	switch os.Getenv(nonAdminHelperEnv) {
	case "own":
		nonAdminServesOwnPipe(t)
		return
	case "admins-only":
		nonAdminDeniedAdminsOnlyPipe(t)
		return
	}
	for _, mode := range []string{"own", "admins-only"} {
		t.Run(mode, func(t *testing.T) {
			tok := nonAdminToken(t)
			//nolint:gosec // the test binary re-running one of its own tests
			cmd := exec.Command(os.Args[0], "-test.run=^TestListenAsNonAdminUser$", "-test.v")
			cmd.Env = append(os.Environ(), nonAdminHelperEnv+"="+mode)
			cmd.SysProcAttr = &syscall.SysProcAttr{Token: syscall.Token(tok)}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("child as non-admin (%s): %v\n%s", mode, err, out)
			}
			if !strings.Contains(string(out), "PASS") {
				t.Fatalf("child as non-admin (%s) did not pass:\n%s", mode, out)
			}
		})
	}
}

// nonAdminToken is this process's token with Builtin Administrators
// deny-only: an administrator's token as UAC filters it.
func nonAdminToken(t *testing.T) windows.Token {
	t.Helper()
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	var self windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_DUPLICATE|windows.TOKEN_ASSIGN_PRIMARY|windows.TOKEN_QUERY,
		&self); err != nil {
		t.Fatal(err)
	}
	defer self.Close()
	var restricted foundation.HANDLE
	disable := []winsec.SID_AND_ATTRIBUTES{{Sid: winsec.PSID(unsafe.Pointer(admins))}}
	if err := winsec.CreateRestrictedToken(
		foundation.HANDLE(self), 0, disable, nil, nil, &restricted,
	); err != nil {
		t.Fatalf("CreateRestrictedToken: %v", err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(windows.Handle(restricted)) })
	return windows.Token(restricted)
}

// nonAdminServesOwnPipe: Listen's descriptor lets the process accept and
// connect on its own pipe.
func nonAdminServesOwnPipe(t *testing.T) {
	addr := testAddr(t)
	l, err := Listen(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	accepted := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err == nil {
			c.Close()
		}
		accepted <- err
	}()
	c, err := Dial(dialCtx(t), NetworkPipe, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	c.Close()
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Accept never returned")
	}
}

// nonAdminDeniedAdminsOnlyPipe: SYSTEM and Administrators alone leave the
// process unable to accept on the pipe it created.
func nonAdminDeniedAdminsOnlyPipe(t *testing.T) {
	l, err := ListenPipeSDDL(testAddr(t), pipeSDDL)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	accepted := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err == nil {
			c.Close()
		}
		accepted <- err
	}()
	select {
	case err := <-accepted:
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Fatalf("Accept = %v, want access denied", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Accept waited for a client: the instance was created")
	}
}
