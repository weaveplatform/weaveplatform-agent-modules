package weaveclipboardtest

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclient"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weaveclipboard"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavemodule/weavemoduletest"
	"github.com/weaveplatform/weaveplatform-agent-modules/sdk/weavewire"
)

// LargeFileEnv sets, in MiB, the size of the file LargeFilesStreamWithNoCeiling
// sends both ways. The default is just over the older transfer's 64 MiB
// ceiling, which proves there is none while keeping a run with the race
// detector short; set it higher (200, say) to soak a backend.
const LargeFileEnv = "WEAVE_CLIPBOARD_CONTRACT_LARGE_MIB"

// defaultLargeMiB is over weavewire.MaxClipboardBytes.
const defaultLargeMiB = 72

func largeFileBytes(t testing.TB) int64 {
	mib := int64(defaultLargeMiB)
	if v := os.Getenv(LargeFileEnv); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			t.Fatalf("%s=%q is not a size in MiB", LargeFileEnv, v)
		}
		mib = n
	}
	return mib << 20
}

// writeLarge writes size bytes to path — a pattern that differs in every
// block, so a block lost or repeated changes the digest — and returns their
// SHA-256. It is generated, never stored: only the test's temporary
// directory holds it.
func writeLarge(t testing.TB, path string, size int64) string {
	t.Helper()
	f, err := os.Create(path) //nolint:gosec // G304: the test's own directory
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // closed below on the path that matters
	h := sha256.New()
	w := bufio.NewWriterSize(io.MultiWriter(f, h), 1<<20)
	block := make([]byte, 64<<10)
	for off := int64(0); off < size; off += int64(len(block)) {
		for i := 0; i < len(block); i += 8 {
			binary.LittleEndian.PutUint64(
				block[i:],
				uint64(off)+uint64(i),
			) //nolint:gosec // G115: an offset
		}
		n := min(int64(len(block)), size-off)
		if _, err := w.Write(block[:n]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// A file over the older transfer's 64 MiB ceiling crosses both ways, as a
// host sends it: through weaveclient and the channel's real framing, streamed
// from disk to the guest's staging and back to the host's disk, verified at
// each end by its SHA-256. Nothing caps it.
//
// A backend that takes files in memory rather than by path (not a
// weaveclipboard.FileWriter) cannot stream them from disk, and is skipped.
func checkLargeFiles(t testing.TB, b weaveclipboard.Backend, size int64) {
	if _, ok := b.(weaveclipboard.FileWriter); !ok {
		t.Skip("the backend takes files in memory, not by path: there is no disk to stream")
	}
	if size <= 0 {
		size = largeFileBytes(t)
	}
	svc := weaveclipboard.NewService(b, weaveclipboard.WithStagingDir(t.TempDir()))
	guestConn, hostConn := net.Pipe()
	core := weavemoduletest.NewCore(guestConn, nil)
	core.Serve(t, svc)
	go core.Run()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	client := weaveclient.New(ctx, hostConn, weaveclient.Options{})
	t.Cleanup(func() {
		_ = client.Close()
		cancel()
		_ = core.Close()
	})

	st, err := client.ClipboardStat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Streaming {
		t.Fatal("the module does not stream")
	}
	if !slices.ContainsFunc(st.Support, func(s weavewire.ClipboardFormatSupport) bool {
		return s.Format == weavewire.ClipboardFiles && s.Held
	}) {
		t.Skip("this guest does not hold files")
	}

	host := t.TempDir()
	src := filepath.Join(host, "large.bin")
	want := writeLarge(t, src, size)

	res, err := client.ClipboardSend(ctx, []weaveclient.ClipboardSource{
		{Format: weavewire.ClipboardFiles, Name: "large.bin", Path: src, Size: size},
	}, weaveclient.TransferOptions{})
	if err != nil || !res.Set || len(res.Dropped) != 0 ||
		!slices.Contains(res.Written, weavewire.ClipboardFiles) {
		t.Fatalf("sending %d MiB: %+v, %v", size>>20, res, err)
	}
	if err := os.Remove(src); err != nil { // the disk holds one copy at a time
		t.Fatal(err)
	}

	got, err := client.ClipboardGetWith(ctx, weavewire.ClipboardGetRequest{
		Formats: []weavewire.ClipboardFormat{weavewire.ClipboardFiles}, Stream: true,
		TransferID: "large",
	}, weaveclient.TransferOptions{})
	if err != nil || len(got.Items) != 1 || !got.Items[0].Deferred || got.Items[0].Size != size ||
		got.Items[0].Name != "large.bin" {
		t.Fatalf(
			"reading back: %v; items %v, want large.bin deferred at %d bytes",
			err,
			describe(got.Items),
			size,
		)
	}
	f, err := client.ClipboardFetch(
		ctx,
		"large",
		0,
		size,
		host,
		"back.bin",
		weaveclient.TransferOptions{},
	)
	if err != nil {
		t.Fatalf("fetching %d MiB: %v", size>>20, err)
	}
	if f.SHA256 != want || f.Size != size {
		t.Fatalf("fetched %+v, want SHA-256 %s", f, want)
	}
}

// describe names items by format, name, size and whether they are deferred,
// never by their data, which may be large.
func describe(items []weavewire.ClipboardItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(
			out,
			fmt.Sprintf("%s %q %d bytes deferred=%v", it.Format, it.Name, it.Size, it.Deferred),
		)
	}
	return out
}
