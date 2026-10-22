package obsdb

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
)

// Handler 返回一个 slog.Handler：记录先写库，再交给下一个 handler（stdout/文件）。
// 也就是说，日志的"标准输出"行为不变，只是多了一份可查询的副本。
func (s *Store) Handler(next slog.Handler) slog.Handler {
	return &handler{store: s, next: next}
}

type handler struct {
	store *Store
	next  slog.Handler
	attrs []slog.Attr
}

func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	rec := LogRecord{
		TimeNS: r.Time.UnixNano(),
		Level:  r.Level.String(),
		Msg:    r.Message,
		Attrs:  make(map[string]any, r.NumAttrs()+len(h.attrs)),
	}
	if r.PC != 0 {
		if frame, _ := runtime.CallersFrames([]uintptr{r.PC}).Next(); frame.File != "" {
			rec.Caller = fmt.Sprintf("%s:%d", frame.File, frame.Line)
		}
	}
	for _, a := range h.attrs {
		rec.Attrs[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attrs[a.Key] = a.Value.Any()
		return true
	})
	rec.TraceID, _ = rec.Attrs["trace_id"].(string)
	rec.SpanID, _ = rec.Attrs["span_id"].(string)
	rec.ParentSpanID, _ = rec.Attrs["parent_span_id"].(string)

	h.store.push(entry{log: &rec})
	return h.next.Handle(ctx, r)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(append(merged, h.attrs...), attrs...)
	return &handler{store: h.store, next: h.next.WithAttrs(attrs), attrs: merged}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{store: h.store, next: h.next.WithGroup(name), attrs: h.attrs}
}
