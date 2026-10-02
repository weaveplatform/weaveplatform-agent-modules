//go:build darwin

package main

import (
	"strings"

	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/corefoundation"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/iokit"
)

// kCFStringEncodingUTF8, from CFString.h; the binding has no constant for it.
const cfStringEncodingUTF8 = 0x08000100

// platformSerial reads IOPlatformSerialNumber from the root of the IOKit
// service plane (the IOPlatformExpertDevice), which needs no privilege. A
// guest with no serial — some virtual machines report none — reports "".
func platformSerial() string {
	return registryString("IOService:/", "IOPlatformSerialNumber")
}

// registryString reads one string property of the registry entry at path.
func registryString(path, key string) string {
	entry := iokit.IORegistryEntryFromPath(int(iokit.KIOMainPortDefault()), path)
	if entry == 0 {
		return ""
	}
	defer iokit.IOObjectRelease(entry)

	var defaultAllocator corefoundation.CFAllocatorRef // NULL: kCFAllocatorDefault
	cfKey := corefoundation.CFStringCreateWithCString(defaultAllocator, key, cfStringEncodingUTF8)
	defer cfKey.Release()

	prop := iokit.IORegistryEntryCreateCFProperty(entry, cfKey, defaultAllocator, 0)
	if prop == nil {
		return ""
	}
	// The binding wraps the result with a retain of its own, but a Create
	// function already returned it +1: without this CFRelease each inventory
	// would leak the string. prop.Release drops the binding's reference.
	defer prop.Release()
	defer corefoundation.CFRelease(prop)

	// A CFString is an NSString, whose description is its own text. Anything
	// else (CFData, as some properties are) is not a string to report.
	if !prop.IsKind("NSString") {
		return ""
	}
	return strings.TrimSpace(prop.Description())
}
