package errs

import (
	"context"
	"log/slog"
	"strings"
)

// SlogHandler wraps next. Every record goes to next as before. Each
// record at error level or above also goes to the client, so one
// slog.Error call writes the log line and opens an error group.
//
// A record whose "err" attribute holds an error is sent as an
// exception, wrapped with the record message. Any other record is sent
// as a message. The record message becomes the "log" tag, and the other
// attributes become extras. The handler resolves each slog.LogValuer
// first, so an event shows what LogValue returns. The ReplaceAttr
// function of next does not apply to events. With no client started,
// the handler only calls next.
func SlogHandler(next slog.Handler) slog.Handler {
	return slogHandler{next}
}

type slogHandler struct{ slog.Handler }

func (h slogHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		captureRecord(r)
	}
	return h.Handler.Handle(ctx, r)
}

func (h slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return slogHandler{h.Handler.WithAttrs(attrs)}
}

func (h slogHandler) WithGroup(name string) slog.Handler {
	return slogHandler{h.Handler.WithGroup(name)}
}

func captureRecord(r slog.Record) {
	var cause error
	opts := make([]Option, 1, 1+r.NumAttrs())
	opts[0] = WithTag("log", r.Message)
	r.Attrs(func(a slog.Attr) bool {
		v := a.Value.Resolve()
		if e, ok := v.Any().(error); ok && a.Key == "err" {
			cause = e
			return true
		}
		opts = append(opts, WithExtra(a.Key, attrText(v)))
		return true
	})
	if cause != nil {
		CaptureException(Wrap(cause, r.Message), opts...)
		return
	}
	CaptureMessage(r.Message, opts...)
}

// attrText formats a slog value for an extra. It resolves each
// slog.LogValuer, in a group too. Value.Resolve stops a LogValue that
// panics or loops.
func attrText(v slog.Value) string {
	v = v.Resolve()
	if v.Kind() != slog.KindGroup {
		return v.String()
	}
	parts := make([]string, 0, len(v.Group()))
	for _, a := range v.Group() {
		parts = append(parts, a.Key+"="+attrText(a.Value))
	}
	return "[" + strings.Join(parts, " ") + "]"
}
