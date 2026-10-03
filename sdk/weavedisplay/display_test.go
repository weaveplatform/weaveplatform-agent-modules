package weavedisplay_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavedisplay"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

type lister struct {
	displays []weavewire.DisplayInfo
	err      error
}

func (l *lister) List(context.Context) ([]weavewire.DisplayInfo, error) { return l.displays, l.err }

type setter struct {
	*lister
	got []weavewire.DisplaySetRequest
	err error
}

func (s *setter) Set(
	_ context.Context,
	req weavewire.DisplaySetRequest,
) (weavewire.DisplayInfo, error) {
	if s.err != nil {
		return weavewire.DisplayInfo{}, s.err
	}
	s.got = append(s.got, req)
	return weavewire.DisplayInfo{
		ID:      req.DisplayID,
		Current: weavewire.DisplayMode{Width: req.Width, Height: req.Height},
		Scale:   req.Scale,
	}, nil
}

func twoDisplays() *lister {
	return &lister{displays: []weavewire.DisplayInfo{
		{ID: "Virtual-1", Current: weavewire.DisplayMode{Width: 1024, Height: 768}},
		{
			ID:      "Virtual-2",
			Primary: true,
			Current: weavewire.DisplayMode{Width: 1920, Height: 1080, RefreshHz: 60},
			Modes:   []weavewire.DisplayMode{{Width: 1920, Height: 1080, RefreshHz: 60}},
			Scale:   1,
		},
	}}
}

func TestServesExactlyTheDisplayContract(t *testing.T) {
	for _, b := range []weavedisplay.Backend{&lister{}, &setter{lister: &lister{}}} {
		if err := weavemodule.CheckParity(weavedisplay.NewService(b)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestList(t *testing.T) {
	b := twoDisplays()
	h := weavemoduletest.Start(t, weavedisplay.NewService(b))
	var got weavewire.DisplayListResponse
	h.Decode(weavewire.KindDisplayList, nil, &got)
	if len(got.Displays) != 2 || !got.Displays[1].Primary ||
		got.Displays[1].Current.RefreshHz != 60 {
		t.Fatalf("list = %+v", got)
	}

	b.displays = nil
	if res := h.Call(
		weavewire.KindDisplayList,
		nil,
	); !strings.Contains(
		string(res.Payload),
		`"displays":[]`,
	) {
		t.Fatalf("res = %+v", res)
	}

	b.err = errors.New("no window server")
	if res := h.Call(
		weavewire.KindDisplayList,
		nil,
	); !strings.Contains(
		res.Err,
		"no window server",
	) {
		t.Fatalf("err = %q", res.Err)
	}
}

func TestSetIsUnsupportedWithoutTheBackend(t *testing.T) {
	h := weavemoduletest.Start(t, weavedisplay.NewService(twoDisplays()))
	res := h.Call(weavewire.KindDisplaySet, weavewire.DisplaySetRequest{Width: 800, Height: 600})
	if res.Code != weavewire.CodeUnsupported {
		t.Fatalf("res = %+v", res)
	}
}

func TestSetResolvesTheDisplay(t *testing.T) {
	b := &setter{lister: twoDisplays()}
	h := weavemoduletest.Start(t, weavedisplay.NewService(b))

	// No id: the primary. A size it never listed still goes to the OS.
	var got weavewire.DisplaySetResponse
	h.Decode(weavewire.KindDisplaySet, weavewire.DisplaySetRequest{Width: 1366, Height: 777}, &got)
	if got.Display.ID != "Virtual-2" || got.Display.Current.Width != 1366 {
		t.Fatalf("set = %+v", got)
	}
	h.Decode(
		weavewire.KindDisplaySet,
		weavewire.DisplaySetRequest{DisplayID: "Virtual-1", Scale: 2},
		&got,
	)
	if got.Display.ID != "Virtual-1" || got.Display.Scale != 2 {
		t.Fatalf("set = %+v", got)
	}

	// No primary marked: the first listed.
	b.displays[1].Primary = false
	h.Decode(weavewire.KindDisplaySet, weavewire.DisplaySetRequest{Scale: 1.5}, &got)
	if got.Display.ID != "Virtual-1" {
		t.Fatalf("set = %+v", got)
	}

	if res := h.Call(
		weavewire.KindDisplaySet,
		weavewire.DisplaySetRequest{DisplayID: "HDMI-9", Scale: 1},
	); !strings.Contains(
		res.Err,
		"no such display",
	) {
		t.Fatalf("err = %q", res.Err)
	}
	b.displays = nil
	if res := h.Call(
		weavewire.KindDisplaySet,
		weavewire.DisplaySetRequest{Scale: 1},
	); !strings.Contains(
		res.Err,
		"no displays",
	) {
		t.Fatalf("err = %q", res.Err)
	}
	b.lister.err = errors.New("no window server")
	if res := h.Call(
		weavewire.KindDisplaySet,
		weavewire.DisplaySetRequest{Scale: 1},
	); !strings.Contains(
		res.Err,
		"no window server",
	) {
		t.Fatalf("err = %q", res.Err)
	}
}

func TestSetRefusesNonsense(t *testing.T) {
	b := &setter{lister: twoDisplays()}
	h := weavemoduletest.Start(t, weavedisplay.NewService(b))
	for name, payload := range map[string]any{
		"garbage":        "nope",
		"nothing":        weavewire.DisplaySetRequest{DisplayID: "Virtual-1"},
		"half a size":    weavewire.DisplaySetRequest{Width: 800},
		"negative":       weavewire.DisplaySetRequest{Width: -800, Height: 600},
		"too big":        weavewire.DisplaySetRequest{Width: weavewire.MaxDisplayPixels + 1, Height: 600},
		"bad refresh":    weavewire.DisplaySetRequest{Width: 800, Height: 600, RefreshHz: -1},
		"negative scale": weavewire.DisplaySetRequest{Scale: -1},
		"huge scale":     weavewire.DisplaySetRequest{Scale: weavewire.MaxDisplayScale + 1},
	} {
		if res := h.Call(
			weavewire.KindDisplaySet,
			payload,
		); !strings.Contains(
			res.Err,
			"bad request",
		) {
			t.Errorf("%s: err = %q", name, res.Err)
		}
	}
	if len(b.got) != 0 {
		t.Fatalf("nonsense reached the OS: %+v", b.got)
	}
}

// A backend that cannot do part of a set says so with the unsupported code,
// which survives the service's wrapping.
func TestSetCarriesTheBackendsUnsupported(t *testing.T) {
	b := &setter{
		lister: twoDisplays(),
		err:    &weavewire.UnsupportedError{Kind: weavewire.KindDisplaySet, Reason: "no scaling"},
	}
	h := weavemoduletest.Start(t, weavedisplay.NewService(b))
	res := h.Call(weavewire.KindDisplaySet, weavewire.DisplaySetRequest{Scale: 2})
	if res.Code != weavewire.CodeUnsupported || !strings.Contains(res.Err, "no scaling") {
		t.Fatalf("res = %+v", res)
	}
}
