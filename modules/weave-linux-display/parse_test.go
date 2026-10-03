//go:build linux

package main

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseXrandr(t *testing.T) {
	got := parseXrandr(fixture(t, "xrandr-query.txt"))
	want := []weavewire.DisplayInfo{
		{
			ID: "Virtual-1", Primary: true,
			Current: weavewire.DisplayMode{Width: 1920, Height: 1080, RefreshHz: 60},
			Modes: []weavewire.DisplayMode{
				{Width: 1920, Height: 1080, RefreshHz: 60},
				{Width: 1920, Height: 1080, RefreshHz: 59.96},
				{Width: 1920, Height: 1080, RefreshHz: 59.93},
				{Width: 1280, Height: 1024, RefreshHz: 75.02},
				{Width: 1280, Height: 1024, RefreshHz: 60.02},
				{Width: 1024, Height: 768, RefreshHz: 60},
			},
		},
		{
			ID:      "Virtual-3",
			Current: weavewire.DisplayMode{Width: 1280, Height: 1024, RefreshHz: 60.02},
			Modes: []weavewire.DisplayMode{
				{Width: 1280, Height: 1024, RefreshHz: 60.02},
				{Width: 800, Height: 600, RefreshHz: 60.32},
				{Width: 800, Height: 600, RefreshHz: 56.25},
			},
		},
		{
			ID:    "Virtual-4",
			Modes: []weavewire.DisplayMode{{Width: 1024, Height: 768, RefreshHz: 60}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsed\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseXrandrOddLines(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want []weavewire.DisplayInfo
	}{
		"empty":                   {"", nil},
		"modes before any output": {"   1024x768 60.00*\n", nil},
		"detached current marker": {
			"X connected\n   800x600 60.00 *\n   bogus 1\n   0x0 60\n   640x480 x60\n",
			[]weavewire.DisplayInfo{{
				ID:      "X",
				Current: weavewire.DisplayMode{Width: 800, Height: 600, RefreshHz: 60},
				Modes:   []weavewire.DisplayMode{{Width: 800, Height: 600, RefreshHz: 60}},
			}},
		},
		"modes of a disconnected output": {"Y disconnected\n   800x600 60.00\n", nil},
		"a header without a state":       {"Z\n", nil},
	} {
		if got := parseXrandr(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: parsed %+v, want %+v", name, got, tc.want)
		}
	}
}

func TestParseWlrRandr(t *testing.T) {
	got := parseWlrRandr(fixture(t, "wlr-randr.txt"))
	want := []weavewire.DisplayInfo{
		{
			ID: "Virtual-1", Name: "Red Hat, Inc. QEMU Monitor (Virtual-1)", Scale: 2,
			Current: weavewire.DisplayMode{Width: 2560, Height: 1600, RefreshHz: 59.987},
			Modes: []weavewire.DisplayMode{
				{Width: 1024, Height: 768, RefreshHz: 60},
				{Width: 2560, Height: 1600, RefreshHz: 59.987},
				{Width: 1920, Height: 1080, RefreshHz: 60},
			},
		},
		{
			ID: "HDMI-A-1", Name: "Dell Inc. DELL U2720Q 0x1234 (HDMI-A-1)", Scale: 1.5,
			Current: weavewire.DisplayMode{Width: 3840, Height: 2160, RefreshHz: 60},
			Modes: []weavewire.DisplayMode{
				{Width: 3840, Height: 2160, RefreshHz: 60},
				{Width: 3840, Height: 2160, RefreshHz: 30},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsed\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseWlrRandrOddLines(t *testing.T) {
	if got := parseWlrRandr("  Enabled: yes\n\n"); len(got) != 0 {
		t.Errorf("properties before any output: %+v", got)
	}
	got := parseWlrRandr(
		"A\n  Scale: big\n    wide px, 60 Hz\n    800x600 px, fast Hz\n    800x600\n",
	)
	want := []weavewire.DisplayInfo{
		{ID: "A", Modes: []weavewire.DisplayMode{{Width: 800, Height: 600}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsed %+v, want %+v", got, want)
	}
}

func TestParseModeline(t *testing.T) {
	name, timings, err := parseModeline(fixture(t, "cvt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"118.25",
		"1600",
		"1696",
		"1856",
		"2112",
		"900",
		"903",
		"908",
		"934",
		"-hsync",
		"+vsync",
	}
	if name != "1600x900_60.00" || !reflect.DeepEqual(timings, want) {
		t.Errorf("modeline %q %v", name, timings)
	}
	if _, _, err := parseModeline("# nothing\n"); !errors.Is(err, errNoModeline) {
		t.Errorf("err = %v", err)
	}
}
