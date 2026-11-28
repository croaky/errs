package errs

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"io"
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
	is.Eq(c.writeJournal(make([]byte, journalBodyMax+1)), journalTooLarge)
	is.Eq(out.Len(), 0)
}

// TestJournalSizeRefusalDoesNotTripBreaker verifies that consecutive
// oversize journal events increment dropped without opening the shared
// circuit breaker, while real writer failures still open it.
func TestJournalSizeRefusalDoesNotTripBreaker(t *testing.T) {
	is := is.New(t)
	var out bytes.Buffer
	clock := newTestClock()
	c := &client{journal: &out, clock: clock.Now}

	oversize := &event{
		EventID: "oversize",
		Message: strings.Repeat("a", journalBodyMax+1),
	}
	for range breakerFailures + 2 {
		c.post(oversize, 0)
	}
	is.Eq(c.failures, 0)
	is.True(c.allow(clock.Now()))
	is.Eq(c.dropped.Load(), int64(breakerFailures+2))

	c.post(&event{EventID: "ok", Message: "normal"}, 0)
	is.True(strings.Contains(out.String(), `"message":"normal"`))

	// Real writer failures still open the breaker after breakerFailures.
	bad := &client{journal: &journalShortWriter{}, clock: clock.Now}
	for range breakerFailures {
		bad.post(&event{EventID: "fail", Message: "x"}, 0)
	}
	is.Eq(bad.failures, breakerFailures)
	is.True(!bad.allow(clock.Now()))
}

// TestMaxedEscapedEventFitsJournalCap verifies that an event at every
// documented limit filled with control bytes and invalid UTF-8 still
// encodes under journalBodyMax and writes one line to the journal.
func TestMaxedEscapedEventFitsJournalCap(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	var out bytes.Buffer
	is.NoErr(Init(Options{Journal: &out}))
	defer Flush(time.Second)

	ctrl := strings.Repeat("\x00", maxMessageBytes)
	badUTF8 := strings.Repeat("\xff", maxMessageBytes)
	var opts []Option
	for i := range maxTags {
		key := strings.Repeat("\x00", 20) + string(rune('a'+i%26)) + string(rune('A'+i/26))
		if i%2 == 0 {
			opts = append(opts, WithTag(key, ctrl))
		} else {
			opts = append(opts, WithTag(key, badUTF8))
		}
	}
	for i := range maxExtras {
		key := strings.Repeat("\xff", 20) + string(rune('a'+i%26)) + string(rune('A'+i/26))
		opts = append(opts, WithExtra(key, strings.Repeat("x", maxExtraBytes-10)))
	}
	fp := make([]string, maxFingerprintParts)
	for i := range fp {
		fp[i] = ctrl
	}
	opts = append(opts, WithFingerprint(fp...), WithLevel(ctrl))

	CaptureException(deepErr(64), opts...)
	Flush(time.Second)

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	is.Eq(len(lines), 1)
	body, ok := strings.CutPrefix(lines[0], journalPrefix)
	is.True(ok)
	is.True(len(body) <= journalBodyMax)
	is.Eq(active.Load().dropped.Load(), int64(0))
}
