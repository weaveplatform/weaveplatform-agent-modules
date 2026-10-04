//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// The host's view: the module's real backend, speaking data control to a
// compositor of the test's own rather than the session's, behind core's
// channel gate — reached by the host client over real framing, and only
// after authentication.
func TestClipboardThroughTheChannel(t *testing.T) {
	f := newFakeCompositor(t, true, false)
	// The backend is chosen from the session's environment, as core sets it.
	t.Setenv("WAYLAND_DISPLAY", f.path)
	t.Setenv("DISPLAY", "")
	svc := newService()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	guestConn, hostConn := net.Pipe()
	core := weavemoduletest.NewCore(guestConn, pub)
	core.Serve(t, svc)
	go core.Run()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := weaveclient.New(
		ctx,
		hostConn,
		weaveclient.Options{Log: slog.New(slog.DiscardHandler)},
	)
	t.Cleanup(func() {
		_ = client.Close()
		_ = core.Close()
	})

	if _, err := client.ClipboardStat(ctx); !errors.Is(err, weaveclient.ErrNotAuthenticated) {
		t.Fatalf("pre-auth stat: err = %v, want ErrNotAuthenticated", err)
	}
	if err := client.Authenticate(ctx, priv); err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	f.setForeign(map[string][]byte{"text/plain;charset=utf-8": []byte("from the guest")},
		"text/plain;charset=utf-8")
	st, err := client.ClipboardStat(ctx)
	if err != nil || len(st.Formats) != 1 || st.Formats[0].Format != weavewire.ClipboardText {
		t.Fatalf("stat = %+v, %v", st, err)
	}
	got, err := client.ClipboardGet(ctx, weavewire.ClipboardGetRequest{})
	if err != nil || len(got.Items) != 1 || string(got.Items[0].Data) != "from the guest" ||
		got.ChangeToken != st.ChangeToken {
		t.Fatalf("get = %+v, %v", got, err)
	}

	// Over the inline limit both ways: uploaded as chunks, then streamed back.
	big := bytes.Repeat([]byte("0123456789abcdef"), weavewire.ClipboardInlineBytes/8)
	set, err := client.ClipboardSet(ctx, []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardText, Data: []byte("caption")},
		{Format: weavewire.ClipboardPNG, Data: big},
	})
	if err != nil || !slices.Equal(set.Written,
		[]weavewire.ClipboardFormat{weavewire.ClipboardPNG, weavewire.ClipboardText}) {
		t.Fatalf("set = %+v, %v", set, err)
	}
	after, err := client.ClipboardStat(ctx)
	if err != nil || after.ChangeToken != set.ChangeToken {
		t.Errorf("stat after the host's set: %+v, %v; want token %d", after, err, set.ChangeToken)
	}
	back, err := client.ClipboardGet(ctx, weavewire.ClipboardGetRequest{
		Formats: []weavewire.ClipboardFormat{weavewire.ClipboardPNG},
	})
	if err != nil || !back.Streamed || len(back.Items) != 1 ||
		!bytes.Equal(back.Items[0].Data, big) {
		t.Fatalf(
			"get of the large image: streamed %v, %d items, %v",
			back.Streamed,
			len(back.Items),
			err,
		)
	}
}
