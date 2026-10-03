//go:build windows

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

	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weaveclipboard"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/pkg/weavewire"
)

// The host's view: the real backend over a stand-in for the clipboard itself,
// so the runner's clipboard is never written, behind core's channel gate —
// reached by the host client over real framing, and only after
// authentication.
func TestClipboardThroughTheChannel(t *testing.T) {
	c, _ := backend(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	guestConn, hostConn := net.Pipe()
	core := weavemoduletest.NewCore(guestConn, pub)
	core.Serve(t, weaveclipboard.NewService(c))
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

	// Over the inline limit both ways: uploaded as chunks, then streamed back.
	big := bytes.Repeat([]byte("0123456789abcdef"), weavewire.ClipboardInlineBytes/8)
	set, err := client.ClipboardSet(ctx, []weavewire.ClipboardItem{
		{Format: weavewire.ClipboardText, Data: []byte("caption")},
		{Format: weavewire.ClipboardPNG, Data: big},
	})
	if err != nil || !slices.Equal(set.Written, []weavewire.ClipboardFormat{
		weavewire.ClipboardText, weavewire.ClipboardPNG,
	}) {
		t.Fatalf("set = %+v, %v", set, err)
	}
	st, err := client.ClipboardStat(ctx)
	if err != nil || st.ChangeToken != set.ChangeToken || len(st.Formats) != 2 {
		t.Errorf("stat after the host's set: %+v, %v; want token %d", st, err, set.ChangeToken)
	}
	back, err := client.ClipboardGet(ctx, weavewire.ClipboardGetRequest{})
	if err != nil || !back.Streamed || len(back.Items) != 2 ||
		!bytes.Equal(back.Items[0].Data, big) || string(back.Items[1].Data) != "caption" {
		t.Fatalf("get: streamed %v, %d items, %v", back.Streamed, len(back.Items), err)
	}
}
