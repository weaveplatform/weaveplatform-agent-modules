package modulesdk

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"google.golang.org/grpc"

	agentv1 "github.com/weaveplatform/weaveplatform-agent-modules/sdk/gen/go/weave/agent/v1"
)

// streamHandler is an slog.Handler that ships records to core's LogService
// so module logs land in core's pipeline with level/time/attrs intact,
// instead of being scraped from stderr as opaque lines. If the stream is
// not up (pre-Init) or a send fails, it falls back to the local stderr
// handler — logging must never be lost because the wire hiccuped.
type streamHandler struct {
	fallback slog.Handler
	client   agentv1.LogServiceClient

	mu     sync.Mutex
	stream grpc.ClientStreamingClient[agentv1.LogRecord, agentv1.LogWriteResponse]
	broken bool // once the stream fails, stop trying it and use fallback

	attrs []slog.Attr
	group string
}

func newStreamHandler(fallback slog.Handler, client agentv1.LogServiceClient) *streamHandler {
	return &streamHandler{fallback: fallback, client: client}
}

func (h *streamHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.fallback.Enabled(ctx, l)
}

func (h *streamHandler) Handle(ctx context.Context, r slog.Record) error {
	rec := &agentv1.LogRecord{
		Level:   wireLevel(r.Level),
		Message: r.Message,
		TimeMs:  r.Time.UnixMilli(),
		Attrs:   map[string]string{},
	}
	for _, a := range h.attrs {
		rec.Attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		key := a.Key
		if h.group != "" {
			key = h.group + "." + key
		}
		rec.Attrs[key] = a.Value.String()
		return true
	})

	if h.send(ctx, rec) {
		return nil
	}
	if err := h.fallback.Handle(ctx, r); err != nil {
		return fmt.Errorf("modulesdk: stderr log fallback: %w", err)
	}
	return nil
}

// send tries the LogService stream, opening it lazily. Returns false (so
// the caller falls back to stderr) if the stream is unavailable or errors.
func (h *streamHandler) send(ctx context.Context, rec *agentv1.LogRecord) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.broken || h.client == nil {
		return false
	}
	if h.stream == nil {
		s, err := h.client.Write(context.WithoutCancel(ctx))
		if err != nil {
			h.broken = true
			return false
		}
		h.stream = s
	}
	if err := h.stream.Send(rec); err != nil {
		h.broken = true
		h.stream = nil
		return false
	}
	return true
}

func (h *streamHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.mu.Lock()
	defer h.mu.Unlock()
	nh := &streamHandler{
		fallback: h.fallback.WithAttrs(attrs),
		client:   h.client,
		stream:   h.stream,
		attrs:    append(append([]slog.Attr(nil), h.attrs...), attrs...),
		group:    h.group,
	}
	return nh
}

func (h *streamHandler) WithGroup(name string) slog.Handler {
	h.mu.Lock()
	defer h.mu.Unlock()
	g := name
	if h.group != "" {
		g = h.group + "." + name
	}
	return &streamHandler{
		fallback: h.fallback.WithGroup(name),
		client:   h.client,
		stream:   h.stream,
		attrs:    append([]slog.Attr(nil), h.attrs...),
		group:    g,
	}
}
