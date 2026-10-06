//go:build darwin

package main

import (
	"runtime"
	"sync"

	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/corefoundation"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/foundation"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/iokit"
	sc "github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/systemconfiguration"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/runtime/obj"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/runtime/purego"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/runtime/rt"
	"github.com/ebitengine/purego/objc"
)

// kCFStringEncodingUTF8, from CFString.h; the binding has no constant for it.
const cfStringEncodingUTF8 = 0x08000100

// consoleUsersKey is kIOConsoleUsersKey, a property of the registry root.
const consoleUsersKey = "IOConsoleUsers"

// bindingMu serializes every framework call this module makes. The generated
// bindings resolve each C function lazily, on its first call, by writing a
// package-level function variable with no synchronization; the session
// service's watch loop and a request handler both read the console, so two
// first calls race on that write. Holding one lock for the whole read keeps
// the lazy resolution, and the reads themselves (a few microseconds every two
// seconds), single-file.
var bindingMu sync.Mutex

// withPool runs fn inside an autorelease pool on one OS thread, with
// bindingMu held.
//
// The Foundation calls behind the conversions below (-description, -allKeys)
// hand back autoreleased objects. On a thread with no pool they would wait
// for the thread to exit, which a Go-managed thread never does, and the
// service reads the console every two seconds for the life of the module.
// The pool is made with objc directly: the binding's AutoreleasePool wrapper
// also releases the pool from a finalizer, after -drain has freed it.
func withPool(fn func()) {
	bindingMu.Lock()
	defer bindingMu.Unlock()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pool := objc.ID(objc.GetClass("NSAutoreleasePool")).Send(objc.RegisterName("new"))
	defer pool.Send(objc.RegisterName("drain"))
	fn()
}

// copyConsoleUser asks SystemConfiguration who owns the console, as agent-core
// does: a NULL store asks configd directly, with no session object for a
// one-shot read of State:/Users/ConsoleUser.
func copyConsoleUser() (name string, uid uint32, present bool) {
	withPool(func() {
		ref, u, _ := sc.SCDynamicStoreCopyConsoleUser(sc.SCDynamicStoreRef{})
		if ref.IsNil() {
			return
		}
		// CFString is toll-free bridged to NSString, whose -description is
		// the string itself.
		name, uid, present = ref.Description(), uint32(u), true //nolint:gosec // G115: a uid_t.
	})
	return name, uid, present
}

// readConsoleUsers reads IOConsoleUsers from the registry root. No property
// means no GUI session has ever started (a headless boot), which is an empty
// answer rather than a failure.
func readConsoleUsers() ([]consoleEntry, error) {
	var out []consoleEntry
	withPool(func() {
		// The registry's root entry, above the service plane's platform
		// expert ("IOService:/"): IOConsoleUsers lives there.
		root := iokit.IORegistryGetRootEntry(
			int(iokit.KIOMainPortDefault()),
		) //nolint:gosec // G115: a mach port name.
		if root == 0 {
			return
		}
		defer iokit.IOObjectRelease(root)

		var defaultAllocator corefoundation.CFAllocatorRef // NULL: kCFAllocatorDefault
		key := corefoundation.CFStringCreateWithCString(
			defaultAllocator,
			consoleUsersKey,
			cfStringEncodingUTF8,
		)
		defer key.Release()
		prop := iokit.IORegistryEntryCreateCFProperty(root, key, defaultAllocator, 0)
		if prop == nil {
			return
		}
		// The binding retains the result although a Create function already
		// returned it +1: CFRelease balances the create, prop.Release the
		// binding's own reference.
		defer prop.Release()
		defer corefoundation.CFRelease(prop)
		if !prop.IsKind("NSArray") {
			return
		}
		out = purego.NSArrayToSlice(obj.ID(prop), entryFromDict)
	})
	return out, nil
}

// entryFromDict converts one session dictionary, keeping the numbers and
// strings and dropping anything else (a UUID, a nested value).
func entryFromDict(dict objc.ID) consoleEntry {
	e := consoleEntry{}
	if !rt.IsKind(dict, "NSDictionary") {
		return e
	}
	for k, v := range rt.DictToMap(dict, rt.Description, func(id objc.ID) objc.ID { return id }) {
		switch {
		case rt.IsKind(v, "NSNumber"):
			e[k] = foundation.NumberFromID(v).LongLongValue()
		case rt.IsKind(v, "NSString"):
			e[k] = rt.Description(v)
		}
	}
	return e
}
