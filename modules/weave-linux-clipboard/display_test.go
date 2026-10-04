//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestMain gives the X11 tests a display of their own: when none is named in
// WEAVE_CLIPBOARD_TEST_DISPLAY and Xvfb is installed, it starts one for this
// test process and stops it after. Without either, the X11 tests skip, saying
// so. The tests never use the session's DISPLAY: they take its clipboard.
func TestMain(m *testing.M) {
	stop := startXvfb()
	code := m.Run()
	stop()
	os.Exit(code)
}

// startXvfb starts Xvfb on a free display, names it in the environment, and
// returns what stops it.
func startXvfb() func() {
	if os.Getenv(testDisplayEnv) != "" {
		return func() {}
	}
	bin, err := exec.LookPath("Xvfb")
	if err != nil {
		return func() {}
	}
	r, w, err := os.Pipe()
	if err != nil {
		return func() {}
	}
	defer func() { _ = r.Close() }()
	// -displayfd: Xvfb picks a free display and writes its number to fd 3
	// once it accepts connections. -noreset: an X server resets itself when
	// its last client leaves, and a test connecting during the reset is
	// refused; the tests connect and disconnect all the time.
	cmd := exec.Command(
		bin,
		"-displayfd",
		"3",
		"-noreset",
		"-nolisten",
		"tcp", //nolint:noctx // lives for the run
		"-screen",
		"0",
		"64x64x24",
	)
	cmd.ExtraFiles = []*os.File{w}
	if err := cmd.Start(); err != nil {
		_ = w.Close()
		return func() {}
	}
	_ = w.Close()
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(r).ReadString('\n')
		line <- strings.TrimSpace(s)
	}()
	select {
	case n := <-line:
		if n != "" {
			_ = os.Setenv(testDisplayEnv, ":"+n)
			fmt.Fprintf(os.Stderr, "X11 tests on Xvfb display :%s\n", n)
		}
	case <-time.After(10 * time.Second):
	}
	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}
