package modulesdk

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc"

	agentv1 "github.com/weaveplatform/weaveplatform-agent-modules/sdk/gen/go/weave/agent/v1"
)

// fakeLogClient stands in for LogServiceClient so the handler's failure
// paths are reachable without racing a real stream's teardown.
type fakeLogClient struct {
	openErr error
	sendErr error
	opened  int
	sent    []*agentv1.LogRecord
}

func (c *fakeLogClient) Write(
	context.Context,
	...grpc.CallOption,
) (grpc.ClientStreamingClient[agentv1.LogRecord, agentv1.LogWriteResponse], error) {
	c.opened++
	if c.openErr != nil {
		return nil, c.openErr
	}
	return &fakeLogStream{c: c}, nil
}

type fakeLogStream struct {
	grpc.ClientStream
	c *fakeLogClient
}

func (s *fakeLogStream) Send(r *agentv1.LogRecord) error {
	if s.c.sendErr != nil {
		return s.c.sendErr
	}
	s.c.sent = append(s.c.sent, r)
	return nil
}

func (s *fakeLogStream) CloseAndRecv() (*agentv1.LogWriteResponse, error) { return nil, nil }

func TestStreamHandlerShipsRecordsWithAttrsAndGroups(t *testing.T) {
	var local bytes.Buffer
	client := &fakeLogClient{}
	h := newStreamHandler(
		slog.NewTextHandler(&local, &slog.HandlerOptions{Level: slog.LevelInfo}),
		client,
	)
	log := slog.New(h).With("module", "toy").WithGroup("req").WithGroup("inner").With("id", 7)

	if log.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug enabled though the fallback is at info")
	}
	log.Info("hello", "k", "v")
	log.Warn("again")
	if local.Len() != 0 {
		t.Fatalf("streamed record also hit the fallback: %s", local.String())
	}
	if client.opened != 1 || len(client.sent) != 2 {
		t.Fatalf(
			"opened %d streams, sent %d records; want 1 and 2",
			client.opened,
			len(client.sent),
		)
	}
	rec := client.sent[0]
	if rec.GetMessage() != "hello" || rec.GetLevel() != int32(slog.LevelInfo) {
		t.Fatalf("record = %v", rec)
	}
	for k, want := range map[string]string{"module": "toy", "id": "7", "req.inner.k": "v"} {
		if got := rec.GetAttrs()[k]; got != want {
			t.Errorf("attr %q = %q, want %q (all: %v)", k, got, want, rec.GetAttrs())
		}
	}
}

func TestStreamHandlerFallsBack(t *testing.T) {
	for name, client := range map[string]*fakeLogClient{
		"open fails": {openErr: errors.New("no stream")},
		"send fails": {sendErr: errors.New("broken pipe")},
	} {
		t.Run(name, func(t *testing.T) {
			var local bytes.Buffer
			log := slog.New(newStreamHandler(slog.NewTextHandler(&local, nil), client))
			log.Info("first")
			log.Info("second")
			if got := local.String(); !strings.Contains(got, "first") ||
				!strings.Contains(got, "second") {
				t.Fatalf("fallback output = %q", got)
			}
			// Once broken, the stream is not retried for every record.
			if client.opened != 1 {
				t.Fatalf("stream opened %d times, want 1", client.opened)
			}
		})
	}
	t.Run("no client", func(t *testing.T) {
		var local bytes.Buffer
		slog.New(newStreamHandler(slog.NewTextHandler(&local, nil), nil)).Info("pre-init")
		if !strings.Contains(local.String(), "pre-init") {
			t.Fatalf("fallback output = %q", local.String())
		}
	})
}
