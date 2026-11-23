package errs

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/croaky/is"
)

func slogTestClient(t *testing.T) *sockeyeServer {
	t.Helper()
	resetActive(t)
	srv := newSockeyeServer(t)
	t.Cleanup(srv.Close)
	is.New(t).NoErr(Init(Options{
		DSN:        testDSN,
		HTTPClient: rewriteTransport(srv.URL),
	}))
	t.Cleanup(func() { Flush(time.Second) })
	return srv
}

func TestSlogHandlerSendsErrorRecordAsException(t *testing.T) {
	is := is.New(t)
	cs := slogTestClient(t)
	var buf bytes.Buffer
	log := slog.New(SlogHandler(slog.NewTextHandler(&buf, nil)))

	log.Error("fetch scores", "err", errors.New("kaboom"), "league", "nfl")
	Flush(time.Second)

	is.True(strings.Contains(buf.String(), "fetch scores")) // the log line still prints
	ev := cs.firstEvent(t)
	tags, _ := ev["tags"].(map[string]any)
	is.Eq(tags["log"], "fetch scores")
	extra, _ := ev["extra"].(map[string]any)
	is.Eq(extra["league"], "nfl")
	exc := ev["exception"].(map[string]any)
	val := exc["values"].([]any)[0].(map[string]any)
	is.True(strings.HasPrefix(val["value"].(string), "fetch scores: kaboom"))
}

func TestSlogHandlerSendsRecordWithoutErrorAsMessage(t *testing.T) {
	is := is.New(t)
	cs := slogTestClient(t)
	log := slog.New(SlogHandler(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	log.Error("queue full", "depth", 9)
	Flush(time.Second)

	ev := cs.firstEvent(t)
	is.Eq(ev["message"], "queue full")
}

func TestSlogHandlerIgnoresLowerLevels(t *testing.T) {
	is := is.New(t)
	srv := slogTestClient(t)
	log := slog.New(SlogHandler(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	log.Warn("slow feed")
	log.With("job", "ingest").Info("done")
	Flush(time.Second)

	is.Eq(len(srv.requests()), 0)
}

func TestSlogHandlerKeepsAttrsAndGroups(t *testing.T) {
	is := is.New(t)
	cs := slogTestClient(t)
	var buf bytes.Buffer
	log := slog.New(SlogHandler(slog.NewTextHandler(&buf, nil))).With("job", "ingest").WithGroup("g")

	log.Error("failed", "n", 1)
	Flush(time.Second)

	is.True(strings.Contains(buf.String(), "job=ingest"))
	is.True(strings.Contains(buf.String(), "g.n=1"))
	cs.firstEvent(t)
}

// TestSendFailureDoesNotReenterSlog: a host bridges the global log
// logger into SlogHandler at error level, and sockeye refuses each
// send. The failure line that errs writes does not become a new event,
// and a later log.Print returns.
func TestSendFailureDoesNotReenterSlog(t *testing.T) {
	is := is.New(t)
	srv := slogTestClient(t)
	srv.status.Store(400)

	oldSlog, oldOut, oldFlags := slog.Default(), log.Writer(), log.Flags()
	t.Cleanup(func() {
		slog.SetDefault(oldSlog)
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
		slog.SetLogLoggerLevel(slog.LevelInfo)
	})
	slog.SetDefault(slog.New(SlogHandler(slog.NewTextHandler(io.Discard, nil))))
	slog.SetLogLoggerLevel(slog.LevelError)

	CaptureMessage("first")
	done := make(chan struct{})
	go func() {
		log.Print("after")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("log.Print blocked after a failed send")
	}
	// The worker sends "first" and then "after". A failure line that
	// came back as an event would sit in the queue between them, and
	// Flush would send it too.
	for deadline := time.Now().Add(5 * time.Second); len(srv.requests()) < 2 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	Flush(5 * time.Second)

	var msgs []any
	for _, r := range srv.requests() {
		msgs = append(msgs, r.ev["message"])
	}
	is.Eq(msgs, []any{"first", "after"})
}

type secret string

func (secret) LogValue() slog.Value { return slog.StringValue("REDACTED") }

// TestSlogHandlerResolvesLogValuer: an attribute whose LogValue returns
// a placeholder shows only the placeholder in the event, at the top
// level, in a group, and under the err key.
func TestSlogHandlerResolvesLogValuer(t *testing.T) {
	is := is.New(t)
	cs := slogTestClient(t)
	log := slog.New(SlogHandler(slog.NewTextHandler(io.Discard, nil)))

	log.Error("login failed",
		"token", secret("hunter2"),
		slog.Group("user", "password", secret("hunter2")),
		"err", secret("hunter2"),
	)
	Flush(time.Second)

	reqs := cs.requests()
	is.Eq(len(reqs), 1)
	body, err := json.Marshal(reqs[0].ev)
	is.NoErr(err)
	is.True(!strings.Contains(string(body), "hunter2"))
	extra := reqs[0].ev["extra"].(map[string]any)
	is.Eq(extra["token"], "REDACTED")
	is.Eq(extra["user"], map[string]any{"password": "REDACTED"})
	is.Eq(extra["err"], "REDACTED")
}

type jsonRedacted struct {
	raw string
}

func (jsonRedacted) MarshalJSON() ([]byte, error) { return []byte(`"REDACTED_JSON"`), nil }

type textRedacted struct {
	raw string
}

func (textRedacted) MarshalText() ([]byte, error) { return []byte("REDACTED_TEXT"), nil }

type structRedacted struct {
	Public string `json:"public"`
	Hidden string `json:"-"`
	priv   string
}

type unencodableSecret struct {
	Ch     chan int
	Hidden string `json:"-"`
	priv   string
}

type panickingJSONSecret struct {
	priv string
}

func (panickingJSONSecret) MarshalJSON() ([]byte, error) { panic("boom") }

type panickingError struct{}

func (*panickingError) Error() string { panic("error boom") }

type jsonRedactedErr struct{ raw string }

func (e jsonRedactedErr) Error() string              { return "raw-json-err: " + e.raw }
func (jsonRedactedErr) MarshalJSON() ([]byte, error) { return []byte(`"REDACTED_JSON_ERR"`), nil }

type jsonToRedactedErr struct{ raw string }

func (e jsonToRedactedErr) Error() string { return "raw-jsonto-err: " + e.raw }
func (jsonToRedactedErr) MarshalJSONTo(enc *jsontext.Encoder) error {
	return enc.WriteToken(jsontext.String("REDACTED_JSONTO_ERR"))
}

type textRedactedErr struct{ raw string }

func (e textRedactedErr) Error() string              { return "raw-text-err: " + e.raw }
func (textRedactedErr) MarshalText() ([]byte, error) { return []byte("REDACTED_TEXT_ERR"), nil }

type appendTextRedactedErr struct{ raw string }

func (e appendTextRedactedErr) Error() string { return "raw-append-err: " + e.raw }
func (appendTextRedactedErr) AppendText(b []byte) ([]byte, error) {
	return append(b, "REDACTED_APPEND_ERR"...), nil
}

type failingJSONErr struct{ raw string }

func (e failingJSONErr) Error() string              { return "raw-fail-json-err: " + e.raw }
func (failingJSONErr) MarshalJSON() ([]byte, error) { return nil, errors.New("marshal failed") }

type panickingJSONErr struct{ raw string }

func (e panickingJSONErr) Error() string              { return "raw-panic-json-err: " + e.raw }
func (panickingJSONErr) MarshalJSON() ([]byte, error) { panic("marshal boom") }

type ptrJSONRedactedErr struct{ raw string }

func (e ptrJSONRedactedErr) Error() string { return "raw-ptr-json-err: " + e.raw }
func (*ptrJSONRedactedErr) MarshalJSON() ([]byte, error) {
	return []byte(`"REDACTED_PTR_JSON_ERR"`), nil
}

type ptrJSONToRedactedErr struct{ raw string }

func (e ptrJSONToRedactedErr) Error() string { return "raw-ptr-jsonto-err: " + e.raw }
func (*ptrJSONToRedactedErr) MarshalJSONTo(enc *jsontext.Encoder) error {
	return enc.WriteToken(jsontext.String("REDACTED_PTR_JSONTO_ERR"))
}

type ptrTextRedactedErr struct{ raw string }

func (e ptrTextRedactedErr) Error() string               { return "raw-ptr-text-err: " + e.raw }
func (*ptrTextRedactedErr) MarshalText() ([]byte, error) { return []byte("REDACTED_PTR_TEXT_ERR"), nil }

type ptrAppendTextRedactedErr struct{ raw string }

func (e ptrAppendTextRedactedErr) Error() string { return "raw-ptr-append-err: " + e.raw }
func (*ptrAppendTextRedactedErr) AppendText(b []byte) ([]byte, error) {
	return append(b, "REDACTED_PTR_APPEND_ERR"...), nil
}

type ptrFailingJSONErr struct{ raw string }

func (e ptrFailingJSONErr) Error() string               { return "raw-ptr-fail-json-err: " + e.raw }
func (*ptrFailingJSONErr) MarshalJSON() ([]byte, error) { return nil, errors.New("marshal failed") }

type ptrPanickingJSONErr struct{ raw string }

func (e ptrPanickingJSONErr) Error() string               { return "raw-ptr-panic-json-err: " + e.raw }
func (*ptrPanickingJSONErr) MarshalJSON() ([]byte, error) { panic("marshal boom") }

// TestSlogHandlerRedactsKindAnyValues: SlogHandler honors MarshalJSON,
// MarshalText, json:"-", and field visibility at the top level and in
// a group, on both the HTTP sink and the journal sink.
func TestSlogHandlerRedactsKindAnyValues(t *testing.T) {
	sinks := []struct {
		name string
		send func(t *testing.T, fn func(*slog.Logger)) (string, map[string]any)
	}{
		{
			name: "http",
			send: func(t *testing.T, fn func(*slog.Logger)) (string, map[string]any) {
				t.Helper()
				is := is.New(t)
				cs := slogTestClient(t)
				fn(slog.New(SlogHandler(slog.NewTextHandler(io.Discard, nil))))
				Flush(time.Second)
				ev := cs.firstEvent(t)
				body, err := json.Marshal(ev)
				is.NoErr(err)
				return string(body), ev["extra"].(map[string]any)
			},
		},
		{
			name: "journal",
			send: func(t *testing.T, fn func(*slog.Logger)) (string, map[string]any) {
				t.Helper()
				is := is.New(t)
				resetActive(t)
				var out bytes.Buffer
				is.NoErr(Init(Options{Journal: &out}))
				t.Cleanup(func() { Flush(time.Second) })
				fn(slog.New(SlogHandler(slog.NewTextHandler(io.Discard, nil))))
				Flush(time.Second)
				line := strings.TrimSuffix(out.String(), "\n")
				body, ok := strings.CutPrefix(line, journalPrefix)
				is.True(ok)
				var ev map[string]any
				is.NoErr(json.Unmarshal([]byte(body), &ev))
				return body, ev["extra"].(map[string]any)
			},
		},
	}

	for _, sink := range sinks {
		t.Run(sink.name, func(t *testing.T) {
			is := is.New(t)
			body, extra := sink.send(t, func(l *slog.Logger) {
				l.Error("auth failed",
					"j", jsonRedacted{raw: "secret-json-top"},
					"t", textRedacted{raw: "secret-text-top"},
					"s", structRedacted{Public: "ok", Hidden: "secret-tag-top", priv: "secret-priv-top"},
					slog.Group("nested",
						"j", jsonRedacted{raw: "secret-json-grp"},
						"t", textRedacted{raw: "secret-text-grp"},
						"s", structRedacted{Public: "ok-grp", Hidden: "secret-tag-grp", priv: "secret-priv-grp"},
					),
				)
			})
			for _, leaked := range []string{
				"secret-json-top", "secret-text-top", "secret-tag-top", "secret-priv-top",
				"secret-json-grp", "secret-text-grp", "secret-tag-grp", "secret-priv-grp",
			} {
				is.True(!strings.Contains(body, leaked))
			}
			is.Eq(extra["j"], "REDACTED_JSON")
			is.Eq(extra["t"], "REDACTED_TEXT")
			is.Eq(extra["s"], map[string]any{"public": "ok"})
			nested, ok := extra["nested"].(map[string]any)
			is.True(ok)
			is.Eq(nested["j"], "REDACTED_JSON")
			is.Eq(nested["t"], "REDACTED_TEXT")
			is.Eq(nested["s"], map[string]any{"public": "ok-grp"})
		})
	}
}

// TestSlogHandlerRedactsErrorMarshalers: a KindAny error attribute that
// implements MarshalJSON, MarshalJSONTo, MarshalText, or AppendText uses
// its marshal output instead of Error(), at top level and in groups, on
// both the HTTP sink and the journal sink. Plain errors still use
// Error(), and a failing or panicking marshal method becomes a fixed
// note.
func TestSlogHandlerRedactsErrorMarshalers(t *testing.T) {
	sinks := []struct {
		name string
		send func(t *testing.T, fn func(*slog.Logger)) (string, map[string]any)
	}{
		{
			name: "http",
			send: func(t *testing.T, fn func(*slog.Logger)) (string, map[string]any) {
				t.Helper()
				is := is.New(t)
				cs := slogTestClient(t)
				fn(slog.New(SlogHandler(slog.NewTextHandler(io.Discard, nil))))
				Flush(time.Second)
				ev := cs.firstEvent(t)
				body, err := json.Marshal(ev)
				is.NoErr(err)
				return string(body), ev["extra"].(map[string]any)
			},
		},
		{
			name: "journal",
			send: func(t *testing.T, fn func(*slog.Logger)) (string, map[string]any) {
				t.Helper()
				is := is.New(t)
				resetActive(t)
				var out bytes.Buffer
				is.NoErr(Init(Options{Journal: &out}))
				t.Cleanup(func() { Flush(time.Second) })
				fn(slog.New(SlogHandler(slog.NewTextHandler(io.Discard, nil))))
				Flush(time.Second)
				line := strings.TrimSuffix(out.String(), "\n")
				body, ok := strings.CutPrefix(line, journalPrefix)
				is.True(ok)
				var ev map[string]any
				is.NoErr(json.Unmarshal([]byte(body), &ev))
				return body, ev["extra"].(map[string]any)
			},
		},
	}

	for _, sink := range sinks {
		t.Run(sink.name, func(t *testing.T) {
			is := is.New(t)
			body, extra := sink.send(t, func(l *slog.Logger) {
				l.Error("api failed",
					"je", jsonRedactedErr{raw: "secret-je-top"},
					"jte", jsonToRedactedErr{raw: "secret-jte-top"},
					"te", textRedactedErr{raw: "secret-te-top"},
					"ae", appendTextRedactedErr{raw: "secret-ae-top"},
					"fe", failingJSONErr{raw: "secret-fe-top"},
					"pe", panickingJSONErr{raw: "secret-pe-top"},
					"plain", errors.New("plain error"),
					slog.Group("nested",
						"je", jsonRedactedErr{raw: "secret-je-grp"},
						"jte", jsonToRedactedErr{raw: "secret-jte-grp"},
						"te", textRedactedErr{raw: "secret-te-grp"},
						"ae", appendTextRedactedErr{raw: "secret-ae-grp"},
					),
				)
			})
			for _, leaked := range []string{
				"secret-je-top", "secret-jte-top", "secret-te-top", "secret-ae-top",
				"secret-fe-top", "secret-pe-top",
				"secret-je-grp", "secret-jte-grp", "secret-te-grp", "secret-ae-grp",
			} {
				is.True(!strings.Contains(body, leaked))
			}
			is.Eq(extra["je"], "REDACTED_JSON_ERR")
			is.Eq(extra["jte"], "REDACTED_JSONTO_ERR")
			is.Eq(extra["te"], "REDACTED_TEXT_ERR")
			is.Eq(extra["ae"], "REDACTED_APPEND_ERR")
			is.Eq(extra["plain"], "plain error")
			fe, ok := extra["fe"].(string)
			is.True(ok)
			is.True(strings.Contains(fe, "could not encode"))
			pe, ok := extra["pe"].(string)
			is.True(ok)
			is.True(strings.Contains(pe, "panicked"))
			nested, ok := extra["nested"].(map[string]any)
			is.True(ok)
			is.Eq(nested["je"], "REDACTED_JSON_ERR")
			is.Eq(nested["jte"], "REDACTED_JSONTO_ERR")
			is.Eq(nested["te"], "REDACTED_TEXT_ERR")
			is.Eq(nested["ae"], "REDACTED_APPEND_ERR")
		})
	}
}

// TestSlogHandlerRedactsPointerErrorMarshalers: a KindAny error value
// with a value-receiver Error() and a pointer-receiver marshal method
// uses its marshal output when passed by value or by pointer, at top
// level and in groups, on both the HTTP sink and the journal sink.
// Typed-nil pointers encode as null without panicking.
func TestSlogHandlerRedactsPointerErrorMarshalers(t *testing.T) {
	sinks := []struct {
		name string
		send func(t *testing.T, fn func(*slog.Logger)) (string, map[string]any)
	}{
		{
			name: "http",
			send: func(t *testing.T, fn func(*slog.Logger)) (string, map[string]any) {
				t.Helper()
				is := is.New(t)
				cs := slogTestClient(t)
				fn(slog.New(SlogHandler(slog.NewTextHandler(io.Discard, nil))))
				Flush(time.Second)
				ev := cs.firstEvent(t)
				body, err := json.Marshal(ev)
				is.NoErr(err)
				return string(body), ev["extra"].(map[string]any)
			},
		},
		{
			name: "journal",
			send: func(t *testing.T, fn func(*slog.Logger)) (string, map[string]any) {
				t.Helper()
				is := is.New(t)
				resetActive(t)
				var out bytes.Buffer
				is.NoErr(Init(Options{Journal: &out}))
				t.Cleanup(func() { Flush(time.Second) })
				fn(slog.New(SlogHandler(slog.NewTextHandler(io.Discard, nil))))
				Flush(time.Second)
				line := strings.TrimSuffix(out.String(), "\n")
				body, ok := strings.CutPrefix(line, journalPrefix)
				is.True(ok)
				var ev map[string]any
				is.NoErr(json.Unmarshal([]byte(body), &ev))
				return body, ev["extra"].(map[string]any)
			},
		},
	}

	for _, sink := range sinks {
		t.Run(sink.name, func(t *testing.T) {
			is := is.New(t)
			body, extra := sink.send(t, func(l *slog.Logger) {
				l.Error("api failed",
					"je_val", ptrJSONRedactedErr{raw: "secret-pje-val"},
					"je_ptr", &ptrJSONRedactedErr{raw: "secret-pje-ptr"},
					"je_nil", (*ptrJSONRedactedErr)(nil),
					"jte_val", ptrJSONToRedactedErr{raw: "secret-pjte-val"},
					"jte_ptr", &ptrJSONToRedactedErr{raw: "secret-pjte-ptr"},
					"jte_nil", (*ptrJSONToRedactedErr)(nil),
					"te_val", ptrTextRedactedErr{raw: "secret-pte-val"},
					"te_ptr", &ptrTextRedactedErr{raw: "secret-pte-ptr"},
					"te_nil", (*ptrTextRedactedErr)(nil),
					"ae_val", ptrAppendTextRedactedErr{raw: "secret-pae-val"},
					"ae_ptr", &ptrAppendTextRedactedErr{raw: "secret-pae-ptr"},
					"ae_nil", (*ptrAppendTextRedactedErr)(nil),
					"fe_val", ptrFailingJSONErr{raw: "secret-pfe-val"},
					"fe_ptr", &ptrFailingJSONErr{raw: "secret-pfe-ptr"},
					"pe_val", ptrPanickingJSONErr{raw: "secret-ppe-val"},
					"pe_ptr", &ptrPanickingJSONErr{raw: "secret-ppe-ptr"},
					slog.Group("nested",
						"je_val", ptrJSONRedactedErr{raw: "secret-pje-grp-val"},
						"je_ptr", &ptrJSONRedactedErr{raw: "secret-pje-grp-ptr"},
						"je_nil", (*ptrJSONRedactedErr)(nil),
						"jte_val", ptrJSONToRedactedErr{raw: "secret-pjte-grp-val"},
						"jte_ptr", &ptrJSONToRedactedErr{raw: "secret-pjte-grp-ptr"},
						"te_val", ptrTextRedactedErr{raw: "secret-pte-grp-val"},
						"te_ptr", &ptrTextRedactedErr{raw: "secret-pte-grp-ptr"},
						"ae_val", ptrAppendTextRedactedErr{raw: "secret-pae-grp-val"},
						"ae_ptr", &ptrAppendTextRedactedErr{raw: "secret-pae-grp-ptr"},
					),
				)
			})
			for _, leaked := range []string{
				"secret-pje-val", "secret-pje-ptr",
				"secret-pjte-val", "secret-pjte-ptr",
				"secret-pte-val", "secret-pte-ptr",
				"secret-pae-val", "secret-pae-ptr",
				"secret-pfe-val", "secret-pfe-ptr",
				"secret-ppe-val", "secret-ppe-ptr",
				"secret-pje-grp-val", "secret-pje-grp-ptr",
				"secret-pjte-grp-val", "secret-pjte-grp-ptr",
				"secret-pte-grp-val", "secret-pte-grp-ptr",
				"secret-pae-grp-val", "secret-pae-grp-ptr",
				"raw-ptr-",
			} {
				is.True(!strings.Contains(body, leaked))
			}
			is.Eq(extra["je_val"], "REDACTED_PTR_JSON_ERR")
			is.Eq(extra["je_ptr"], "REDACTED_PTR_JSON_ERR")
			is.Nil(extra["je_nil"])
			is.Eq(extra["jte_val"], "REDACTED_PTR_JSONTO_ERR")
			is.Eq(extra["jte_ptr"], "REDACTED_PTR_JSONTO_ERR")
			is.Nil(extra["jte_nil"])
			is.Eq(extra["te_val"], "REDACTED_PTR_TEXT_ERR")
			is.Eq(extra["te_ptr"], "REDACTED_PTR_TEXT_ERR")
			is.Nil(extra["te_nil"])
			is.Eq(extra["ae_val"], "REDACTED_PTR_APPEND_ERR")
			is.Eq(extra["ae_ptr"], "REDACTED_PTR_APPEND_ERR")
			is.Nil(extra["ae_nil"])
			for _, key := range []string{"fe_val", "fe_ptr"} {
				s, ok := extra[key].(string)
				is.True(ok)
				is.True(strings.Contains(s, "could not encode"))
			}
			for _, key := range []string{"pe_val", "pe_ptr"} {
				s, ok := extra[key].(string)
				is.True(ok)
				is.True(strings.Contains(s, "panicked"))
			}
			nested, ok := extra["nested"].(map[string]any)
			is.True(ok)
			is.Eq(nested["je_val"], "REDACTED_PTR_JSON_ERR")
			is.Eq(nested["je_ptr"], "REDACTED_PTR_JSON_ERR")
			is.Nil(nested["je_nil"])
			is.Eq(nested["jte_val"], "REDACTED_PTR_JSONTO_ERR")
			is.Eq(nested["jte_ptr"], "REDACTED_PTR_JSONTO_ERR")
			is.Eq(nested["te_val"], "REDACTED_PTR_TEXT_ERR")
			is.Eq(nested["te_ptr"], "REDACTED_PTR_TEXT_ERR")
			is.Eq(nested["ae_val"], "REDACTED_PTR_APPEND_ERR")
			is.Eq(nested["ae_ptr"], "REDACTED_PTR_APPEND_ERR")
		})
	}
}

// TestSlogHandlerEncodesScalarsErrorsAndGroups: scalar attributes stay
// strings, non-err error attributes use Error(), and groups encode as
// JSON objects.
func TestSlogHandlerEncodesScalarsErrorsAndGroups(t *testing.T) {
	is := is.New(t)
	cs := slogTestClient(t)
	log := slog.New(SlogHandler(slog.NewTextHandler(io.Discard, nil)))

	log.Error("request failed",
		"host", "db1",
		"attempt", 3,
		"cause", errors.New("connection refused"),
		slog.Group("req",
			"method", "POST",
			"status", 502,
			slog.Group("inner", "ok", true),
		),
	)
	Flush(time.Second)

	ev := cs.firstEvent(t)
	extra := ev["extra"].(map[string]any)
	is.Eq(extra["host"], "db1")
	is.Eq(extra["attempt"], "3")
	is.Eq(extra["cause"], "connection refused")
	req, ok := extra["req"].(map[string]any)
	is.True(ok)
	is.Eq(req["method"], "POST")
	is.Eq(req["status"], "502")
	inner, ok := req["inner"].(map[string]any)
	is.True(ok)
	is.Eq(inner["ok"], "true")
}

// TestSlogHandlerReplacesEncodeFailureAndPanicWithNote: a KindAny value
// whose encode fails or panics becomes a fixed note and never falls back
// to fmt text.
func TestSlogHandlerReplacesEncodeFailureAndPanicWithNote(t *testing.T) {
	is := is.New(t)
	cs := slogTestClient(t)
	log := slog.New(SlogHandler(slog.NewTextHandler(io.Discard, nil)))

	log.Error("encode problem",
		"bad", unencodableSecret{Ch: make(chan int), Hidden: "secret-unenc-tag", priv: "secret-unenc-priv"},
		"panic", panickingJSONSecret{priv: "secret-panic-priv"},
		"panic_err", (*panickingError)(nil),
	)
	Flush(time.Second)

	ev := cs.firstEvent(t)
	body, err := json.Marshal(ev)
	is.NoErr(err)
	for _, leaked := range []string{"secret-unenc-tag", "secret-unenc-priv", "secret-panic-priv"} {
		is.True(!strings.Contains(string(body), leaked))
	}
	extra := ev["extra"].(map[string]any)
	bad, ok := extra["bad"].(string)
	is.True(ok)
	is.True(strings.Contains(bad, "could not encode"))
	p, ok := extra["panic"].(string)
	is.True(ok)
	is.True(strings.Contains(p, "panicked"))
	pe, ok := extra["panic_err"].(string)
	is.True(ok)
	is.True(strings.Contains(pe, "panicked"))
}

// TestSlogHandlerExtraSizeLimitAndCallerMutation: an extra past 2 KiB
// becomes the size note, and caller mutation of a KindAny map after
// log.Error returns does not change the captured event.
func TestSlogHandlerExtraSizeLimitAndCallerMutation(t *testing.T) {
	is := is.New(t)
	cs := slogTestClient(t)
	log := slog.New(SlogHandler(slog.NewTextHandler(io.Discard, nil)))

	m := map[string]string{"state": "before"}
	big := struct{ Data string }{Data: strings.Repeat("x", maxExtraBytes)}
	log.Error("bounds check", "payload", m, "big", big)
	m["state"] = "after"
	Flush(time.Second)

	ev := cs.firstEvent(t)
	extra := ev["extra"].(map[string]any)
	payload, ok := extra["payload"].(map[string]any)
	is.True(ok)
	is.Eq(payload["state"], "before")
	bigVal, ok := extra["big"].(string)
	is.True(ok)
	is.True(strings.Contains(bigVal, "the limit is"))
}

// TestSlogHandlerQueuedEventsHoldNoLongMessage: SlogHandler wraps an
// untyped err attribute with the record message. Eight queued events
// with 1 MiB messages keep much less than 8 MiB of heap in memory.
func TestSlogHandlerQueuedEventsHoldNoLongMessage(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	t.Cleanup(func() { active.Store(nil) })
	c := &client{queue: make(chan *event, 8)}
	active.Store(c) // no worker, so the events stay in the queue
	log := slog.New(SlogHandler(slog.NewTextHandler(io.Discard, nil)))

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range 8 {
		log.Error("bad input", "err", errors.New(strings.Repeat("a", 1<<20)))
	}
	runtime.GC()
	runtime.ReadMemStats(&after)

	is.Eq(len(c.queue), 8)
	ev := <-c.queue
	is.Eq(ev.Exception.Values[0].Type, "bad input")
	is.True(after.HeapAlloc < before.HeapAlloc+2<<20)
	runtime.KeepAlive(c)
}

func TestSlogHandlerWithoutClientOnlyLogs(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	var buf bytes.Buffer
	log := slog.New(SlogHandler(slog.NewTextHandler(&buf, nil)))

	log.Error("no client", "err", errors.New("x"))

	is.True(strings.Contains(buf.String(), "no client"))
}

// BenchmarkSlogHandlerError measures one slog.Error call with an error
// and attributes through SlogHandler, with a JSON log line to
// io.Discard.
func BenchmarkSlogHandlerError(b *testing.B) {
	benchInit(b)
	log := slog.New(SlogHandler(slog.NewJSONHandler(io.Discard, nil)))
	err := deepErr(16)
	b.ReportAllocs()
	for b.Loop() {
		log.Error("refresh item", "err", err, "company_id", 42, "attempt", 3, slog.Group("req", "method", "GET", "path", "/companies/42"))
	}
}
