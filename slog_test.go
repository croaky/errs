package errs

import (
	"bytes"
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
