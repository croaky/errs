package errs

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/croaky/is"
)

type noJournalHTTP struct{ calls int }

func (t *noJournalHTTP) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	return nil, errors.New("unexpected HTTP")
}

func TestJournalSinkEvent(t *testing.T) {
	is := is.New(t)
	var out bytes.Buffer
	transport := &noJournalHTTP{}
	is.NoErr(Init(Options{Journal: &out, Release: "fixture-release", HTTPClient: &http.Client{Transport: transport}}))
	id := CaptureException(errors.New("fixture error\nsecond line"), WithFingerprint("fixture"), WithTag("source", "test"))
	RecordRequest(Request{Route: "/", Status: 200})
	Flush(time.Second)
	is.Eq(transport.calls, 0)
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	is.Eq(len(lines), 1)
	body, ok := strings.CutPrefix(lines[0], journalPrefix)
	is.Eq(ok, true)
	var e event
	is.NoErr(json.Unmarshal([]byte(body), &e))
	is.Eq(e.EventID, id)
	is.Eq(e.Release, "fixture-release")
	is.Eq(e.Fingerprint, []string{"fixture"})
	is.Eq(e.Exception.Values[0].Value, "fixture error\nsecond line")
	is.Eq(len(e.Exception.Values[0].Stacktrace.Frames) > 0, true)
	is.Eq(active.Load().perf.Load(), (*perfSender)(nil))
	is.NoErr(Init(Options{}))
}

func TestJournalSinkExclusive(t *testing.T) {
	is := is.New(t)
	for _, opts := range []Options{{Journal: io.Discard, DSN: testSockeyeDSN}, {Journal: io.Discard, SockeyeDSN: testSockeyeDSN}} {
		is.HasErr(Init(opts))
	}
	is.NoErr(Init(Options{}))
}

type journalShortWriter struct{ calls int }

func (w *journalShortWriter) Write(b []byte) (int, error) { w.calls++; return len(b) - 1, nil }

func TestJournalSinkShortWriteDoesNotRetry(t *testing.T) {
	is := is.New(t)
	w := &journalShortWriter{}
	is.NoErr(Init(Options{Journal: w}))
	CaptureMessage("fixture")
	Flush(time.Second)
	is.Eq(w.calls, 1)
	is.Eq(active.Load().dropped.Load(), int64(1))
	is.NoErr(Init(Options{}))
}

func TestJournalSinkRedaction(t *testing.T) {
	is := is.New(t)
	var out bytes.Buffer
	is.NoErr(Init(Options{Journal: &out}))
	logger := slog.New(SlogHandler(slog.NewTextHandler(io.Discard, nil)))
	logger.Error("fixture", "token", secret("fixture-secret"))
	Flush(time.Second)
	is.Eq(strings.Contains(out.String(), "fixture-secret"), false)
	is.Eq(strings.Contains(out.String(), "REDACTED"), true)
	is.NoErr(Init(Options{}))
}

func TestJournalSinkDoesNotSwallowPanic(t *testing.T) {
	is := is.New(t)
	var out bytes.Buffer
	is.NoErr(Init(Options{Journal: &out}))
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		func() {
			defer func() {
				if p := recover(); p != nil {
					CaptureException(PanicError(p))
					Flush(time.Second)
					panic(p)
				}
			}()
			panic("fixture panic")
		}()
	}()
	is.Eq(recovered, any("fixture panic"))
	is.Eq(strings.Contains(out.String(), "panic recovered"), true)
	is.NoErr(Init(Options{}))
}

func TestJournalSinkBodyBound(t *testing.T) {
	is := is.New(t)
	var out bytes.Buffer
	c := &client{journal: &out}
	is.Eq(c.writeJournal(make([]byte, journalBodyMax+1)), false)
	is.Eq(out.Len(), 0)
}
