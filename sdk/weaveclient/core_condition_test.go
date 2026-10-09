package weaveclient

import (
	"encoding/json"
	"testing"
)

func TestSnapshotRetainsAndIsolatesCoreCondition(t *testing.T) {
	var snapshot ModulesSnapshot
	if err := json.Unmarshal([]byte(`{"revision":1,"modules":[],"core":{"degraded":true,"reason":"store cannot be unsealed","unavailable":["store","manifest acceptance"]}}`), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Core == nil || !snapshot.Core.Degraded || len(snapshot.Core.Unavailable) != 2 {
		t.Fatalf("core condition lost: %+v", snapshot.Core)
	}
	copied := cloneSnapshot(snapshot)
	copied.Core.Reason = "changed"
	copied.Core.Unavailable[0] = "changed"
	if snapshot.Core.Reason != "store cannot be unsealed" || snapshot.Core.Unavailable[0] != "store" {
		t.Fatal("snapshot condition aliases caller memory")
	}
	if cloneSnapshot(ModulesSnapshot{}).Core != nil {
		t.Fatal("normal snapshot invented a core condition")
	}
}
