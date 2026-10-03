package weavewire_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// The console payloads' JSON spelled out: a renamed field is a value the other
// end silently reads as zero.
func TestConsolePayloadSpelling(t *testing.T) {
	since := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	for want, v := range map[string]any{
		`{"change_token":7,"formats":[{"format":"files","size":12,"count":2}]}`: weavewire.ClipboardStatResponse{
			ChangeToken: 7,
			Formats:     []weavewire.ClipboardFormatInfo{{Format: weavewire.ClipboardFiles, Size: 12, Count: 2}},
		},
		`{"formats":["text/plain"],"max_bytes":10,"transfer_id":"t"}`: weavewire.ClipboardGetRequest{
			Formats: []weavewire.ClipboardFormat{weavewire.ClipboardText}, MaxBytes: 10, TransferID: "t",
		},
		`{"change_token":1,"items":[{"format":"files","name":"a","size":2,"data":"aGk="}],"omitted":[{"format":"image/png","size":9}],"streamed":true}`: weavewire.ClipboardGetResponse{
			ChangeToken: 1,
			Items:       []weavewire.ClipboardItem{{Format: weavewire.ClipboardFiles, Name: "a", Size: 2, Data: []byte("hi")}},
			Omitted:     []weavewire.ClipboardItem{{Format: weavewire.ClipboardPNG, Size: 9}},
			Streamed:    true,
		},
		`{"items":[{"format":"text/html","size":3}],"transfer_id":"u"}`: weavewire.ClipboardSetRequest{
			Items: []weavewire.ClipboardItem{{Format: weavewire.ClipboardHTML, Size: 3}}, TransferID: "u",
		},
		`{"change_token":2,"written":["text/rtf"]}`: weavewire.ClipboardSetResponse{
			ChangeToken: 2, Written: []weavewire.ClipboardFormat{weavewire.ClipboardRTF},
		},
		`{"session":{"id":"2","user":"alice","uid":"501","state":"locked","console":true,"since":"2026-10-03T09:00:00Z"}}`: weavewire.SessionCurrentResponse{
			Session: &weavewire.SessionInfo{
				ID: "2", User: "alice", UID: "501", State: weavewire.SessionLocked, Console: true, Since: since,
			},
		},
		`{}`: weavewire.SessionCurrentResponse{},
		`{"sessions":[{"id":"9","user":"bob","state":"inactive","remote":true}]}`: weavewire.SessionListResponse{
			Sessions: []weavewire.SessionInfo{{ID: "9", User: "bob", State: weavewire.SessionInactive, Remote: true}},
		},
		`{"session_id":"2"}`: weavewire.SessionLockRequest{SessionID: "2"},
		`{"current":{"id":"1","user":"a","state":"active"},"changed_at":"2026-10-03T09:00:00Z"}`: weavewire.SessionChangedEvent{
			Current: &weavewire.SessionInfo{ID: "1", User: "a", State: weavewire.SessionActive}, ChangedAt: since,
		},
		`{"displays":[{"id":"Virtual-1","name":"Built-in","primary":true,"current":{"width":1920,"height":1080,"refresh_hz":60},"modes":[{"width":800,"height":600}],"scale":2}]}`: weavewire.DisplayListResponse{
			Displays: []weavewire.DisplayInfo{{
				ID: "Virtual-1", Name: "Built-in", Primary: true,
				Current: weavewire.DisplayMode{Width: 1920, Height: 1080, RefreshHz: 60},
				Modes:   []weavewire.DisplayMode{{Width: 800, Height: 600}},
				Scale:   2,
			}},
		},
		`{"display_id":"Virtual-1","width":1440,"height":900,"refresh_hz":60,"scale":1.5}`: weavewire.DisplaySetRequest{
			DisplayID: "Virtual-1", Width: 1440, Height: 900, RefreshHz: 60, Scale: 1.5,
		},
	} {
		got, err := json.Marshal(v)
		if err != nil || string(got) != want {
			t.Errorf("%T:\n got %s\nwant %s (%v)", v, got, want, err)
		}
	}
}

func TestClipboardFormatsAreRichestFirstAndComplete(t *testing.T) {
	got := weavewire.ClipboardFormats()
	if got[0] != weavewire.ClipboardFiles || got[len(got)-1] != weavewire.ClipboardText {
		t.Fatalf("formats = %v", got)
	}
	for _, f := range []weavewire.ClipboardFormat{
		weavewire.ClipboardText, weavewire.ClipboardHTML, weavewire.ClipboardRTF, weavewire.ClipboardPNG,
		weavewire.ClipboardTIFF, weavewire.ClipboardPDF, weavewire.ClipboardFiles,
	} {
		if !slices.Contains(got, f) {
			t.Errorf("%s missing", f)
		}
	}
	// The inline limit must leave room under gRPC's 4 MiB message cap once
	// base64 has inflated it.
	if weavewire.ClipboardInlineBytes*4/3 > (4<<20)/4 {
		t.Fatal("the inline limit is too close to the transport's")
	}
}
