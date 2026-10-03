//go:build darwin

package main

import (
	"context"
	"testing"

	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/runtime/purego"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/runtime/rt"
	"github.com/ebitengine/purego/objc"
)

// Against this Mac's real SystemConfiguration and IOKit. A CI macOS runner
// has a GUI session; a machine with nobody at the console still has to answer
// without an error.
func TestRealConsole(t *testing.T) {
	name, uid, present := copyConsoleUser()
	entries, err := readConsoleUsers()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("console user %q (%d, %v); %d IOConsoleUsers entries", name, uid, present, len(entries))

	s := newSessions()
	info, ok, err := s.Console(context.Background())
	if !present || !person(name, int64(uid)) {
		if ok || err != nil {
			t.Fatalf("console = %+v, %v, %v with nobody at it", info, ok, err)
		}
		t.Skip("nobody is logged in at this machine's console")
	}
	if err != nil || !ok || info.User != name || info.ID == "" {
		t.Fatalf("console = %+v, %v, %v; SystemConfiguration says %q", info, ok, err, name)
	}
	list, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, l := range list {
		found = found || (l.Console && l.ID == info.ID)
	}
	if !found {
		t.Errorf("list %+v does not mark the console session %s", list, info.ID)
	}
}

// The conversion keeps numbers and strings and skips what it does not read.
func TestEntryFromDict(t *testing.T) {
	withPool(func() {
		arr := purego.SliceToNSArray([]string{"a"}, purego.NSString)
		num := objc.ID(objc.GetClass("NSNumber")).
			Send(objc.RegisterName("numberWithLongLong:"), int64(42))
		yes := objc.ID(objc.GetClass("NSNumber")).Send(objc.RegisterName("numberWithBool:"), true)
		dict := rt.MapToDict(map[string]objc.ID{
			"n": num, "b": yes, "s": purego.NSString("text"), "a": arr,
		}, purego.NSString, func(id objc.ID) objc.ID { return id })

		e := entryFromDict(dict)
		if len(e) != 3 || e["n"] != int64(42) || e["b"] != int64(1) || e["s"] != "text" {
			t.Errorf("entry = %#v", e)
		}
		if e := entryFromDict(arr); len(e) != 0 {
			t.Errorf("a non-dictionary converted to %#v", e)
		}
	})
}
