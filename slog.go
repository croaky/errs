package errs

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"log/slog"
)

// SlogHandler wraps next. Every record goes to next as before. Each
// record at error level or above also goes to the client, so one
// slog.Error call writes the log line and opens an error group.
//
// A record whose "err" attribute holds an error is sent as an
// exception, wrapped with the record message. Any other record is sent
// as a message. The record message becomes the "log" tag, and the other
// attributes become extras. The handler resolves each slog.LogValuer
// first and encodes non-scalar values as JSON, so events honor
// LogValuer, MarshalJSON, MarshalText, json tags, and field visibility.
// The ReplaceAttr function of next does not apply to events. With no
// client started, the handler only calls next.
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
		opts = append(opts, WithExtra(a.Key, attrValue(v)))
		return true
	})
	if cause != nil {
		CaptureException(Wrap(cause, r.Message), opts...)
		return
	}
	CaptureMessage(r.Message, opts...)
}

// attrValue encodes a slog value as JSON on the caller goroutine. It
// resolves each slog.LogValuer, in a group too. Value.Resolve stops a
// LogValue that panics or loops. Scalar kinds stay text, as before. A
// KindAny error uses Error(). Any other KindAny value encodes with
// wireJSON, so MarshalJSON, MarshalText, json tags, and field
// visibility apply. A group encodes as a JSON object with the same
// rules for each member.
func attrValue(v slog.Value) jsontext.Value {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindGroup:
		attrs := v.Group()
		m := make(map[string]jsontext.Value, len(attrs))
		for _, a := range attrs {
			m[eventKey(a.Key)] = attrValue(a.Value)
		}
		b, err := json.Marshal(m, wireJSON)
		if err != nil {
			return noteValue("extra value could not encode")
		}
		return b
	case slog.KindAny:
		return encodeSlogAny(v.Any())
	default:
		b, err := json.Marshal(v.String(), wireJSON)
		if err != nil {
			return noteValue("extra value could not encode")
		}
		return b
	}
}

// encodeSlogAny encodes a KindAny slog value with wireJSON, or with
// Error() for an error. A failure or panic becomes a fixed note so
// hidden fields never fall back to fmt text.
func encodeSlogAny(v any) (out jsontext.Value) {
	defer func() {
		if r := recover(); r != nil {
			out = noteValue("extra value panicked while it encoded")
		}
	}()
	if e, ok := v.(error); ok {
		b, err := json.Marshal(e.Error(), wireJSON)
		if err != nil {
			return noteValue("extra value could not encode")
		}
		return b
	}
	b, err := json.Marshal(v, wireJSON)
	if err != nil {
		return noteValue("extra value could not encode")
	}
	return b
}
