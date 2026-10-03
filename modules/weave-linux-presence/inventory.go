//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// Where the facts that have no syscall live. DMI is absent on ARM boards and
// in most containers, and product_serial is root-only on most distributions,
// so an empty answer from either is expected rather than a failure.
const (
	cpuinfoPath       = "/proc/cpuinfo"
	productNamePath   = "/sys/class/dmi/id/product_name"
	productSerialPath = "/sys/class/dmi/id/product_serial"
)

// inventory fills the Linux half of the inventory; weavepresence has already
// collected hostname, OS, arch, cores and the network interfaces.
//
// Linux has no house bindings, so x/sys/unix and the kernel's own filesystems
// are the sources. The functions are fields so tests can drive the paths a
// healthy runner never takes.
type inventory struct {
	sysinfo  func(*unix.Sysinfo_t) error
	uname    func(*unix.Utsname) error
	readFile func(string) ([]byte, error)
}

func newInventory() inventory {
	return inventory{sysinfo: unix.Sysinfo, uname: unix.Uname, readFile: os.ReadFile}
}

// Collect is best-effort throughout: a field that cannot be read stays empty
// rather than costing the rest of the inventory.
func (i inventory) Collect(_ context.Context, inv *weavewire.InventoryResponse) {
	var si unix.Sysinfo_t
	if err := i.sysinfo(&si); err == nil {
		inv.MemoryBytes = uint64(si.Totalram) * uint64(si.Unit)
		if si.Uptime > 0 {
			inv.UptimeSeconds = uint64(si.Uptime)
		}
	}

	var u unix.Utsname
	if err := i.uname(&u); err == nil {
		inv.OSVersion = unix.ByteSliceToString(u.Release[:])
		inv.OSBuild = unix.ByteSliceToString(u.Version[:])
	}

	inv.CPUModel = cpuModel(i.read(cpuinfoPath))
	inv.HardwareModel = strings.TrimSpace(string(i.read(productNamePath)))
	inv.SerialNumber = strings.TrimSpace(string(i.read(productSerialPath)))
}

// read returns a file's contents, or nothing when it cannot be read.
func (i inventory) read(path string) []byte {
	b, err := i.readFile(path)
	if err != nil {
		return nil
	}
	return b
}

// cpuModel finds the first CPU's model name in /proc/cpuinfo.
//
// The key differs by architecture: x86 writes "model name", some arm64
// kernels write "Processor", and many write neither. Both are tried, and an
// empty result is accepted rather than guessed at from "CPU part" numbers.
func cpuModel(cpuinfo []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(cpuinfo))
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "model name", "Processor":
			return strings.TrimSpace(value)
		}
	}
	return ""
}
