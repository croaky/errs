package errs

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/croaky/is"
)

// TestCaptureCopiesExtraAtCapture: a host changes a map right after it
// passes the map to WithExtra. The event holds the value at capture,
// for a sent event and for a held event, and go test -race sees no
// race with the worker.
func TestCaptureCopiesExtraAtCapture(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	srv := newSockeyeServer(t)
	defer srv.Close()
	is.NoErr(Init(Options{DSN: testDSN, HTTPClient: rewriteTransport(srv.URL), Clock: newTestClock().Now}))

	m := map[string]any{"n": 0}
	fp := []string{"same"}
	for i := range fullPerWindow + 5 {
		CaptureMessage("busy", WithExtra("m", m), WithFingerprint(fp...))
		m["n"] = i + 1
		fp[0] = "changed"
		fp[0] = "same"
	}
	Flush(5 * time.Second)

	reqs := srv.requests()
	first := reqs[0].ev["extra"].(map[string]any)["m"].(map[string]any)
	is.Eq(first["n"], 0.0)
	total := 0
	for _, r := range reqs {
		n, _ := r.ev["count"].(float64)
		total += max(int(n), 1)
	}
	is.Eq(total, fullPerWindow+5)
}

type panicMarshaler struct{}

func (panicMarshaler) MarshalJSON() ([]byte, error) { panic("boom") }

// TestExtraMarshalPanicIsContained: an Extra value whose MarshalJSON
// panics becomes a note, and the event still sends.
func TestExtraMarshalPanicIsContained(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	srv := newSockeyeServer(t)
	defer srv.Close()
	is.NoErr(Init(Options{DSN: testDSN, HTTPClient: rewriteTransport(srv.URL)}))

	CaptureMessage("bad marshal", WithExtra("v", panicMarshaler{}), WithExtra("ok", 1))
	CaptureMessage("next")
	Flush(5 * time.Second)

	reqs := srv.requests()
	is.Eq(len(reqs), 2)
	extra := reqs[0].ev["extra"].(map[string]any)
	is.Eq(extra["ok"], 1.0)
	is.True(strings.Contains(extra["v"].(string), "panic"))
}

type failingJSONSecret struct {
	priv string
}

func (failingJSONSecret) MarshalJSON() ([]byte, error) { return nil, errors.New("marshal json failed") }

type failingTextSecret struct {
	priv string
}

func (failingTextSecret) MarshalText() ([]byte, error) { return nil, errors.New("marshal text failed") }

type panickingExtraSecret struct {
	priv string
}

func (panickingExtraSecret) MarshalJSON() ([]byte, error) { panic("marshal panic") }

// TestExtraEncodeFailureDoesNotLeakHiddenFields: when WithExtra receives
// a value that fails JSON encoding or panics, the event arrives with a
// fixed note on both the HTTP sink and the journal sink, without
// leaking json:"-" or unexported fields via fmt.Sprint.
func TestExtraEncodeFailureDoesNotLeakHiddenFields(t *testing.T) {
	type withChan struct {
		Ch     chan int
		Hidden string `json:"-"`
		priv   string
	}
	type withFunc struct {
		Fn     func()
		Hidden string `json:"-"`
		priv   string
	}
	type withNaN struct {
		Score  float64
		Hidden string `json:"-"`
		priv   string
	}

	sinks := []struct {
		name string
		send func(t *testing.T, fn func()) (string, map[string]any)
	}{
		{
			name: "http",
			send: func(t *testing.T, fn func()) (string, map[string]any) {
				t.Helper()
				is := is.New(t)
				resetActive(t)
				srv := newSockeyeServer(t)
				t.Cleanup(srv.Close)
				is.NoErr(Init(Options{DSN: testDSN, HTTPClient: rewriteTransport(srv.URL)}))
				t.Cleanup(func() { Flush(time.Second) })
				fn()
				Flush(time.Second)
				ev := srv.firstEvent(t)
				body, err := json.Marshal(ev)
				is.NoErr(err)
				return string(body), ev["extra"].(map[string]any)
			},
		},
		{
			name: "journal",
			send: func(t *testing.T, fn func()) (string, map[string]any) {
				t.Helper()
				is := is.New(t)
				resetActive(t)
				var out bytes.Buffer
				is.NoErr(Init(Options{Journal: &out}))
				t.Cleanup(func() { Flush(time.Second) })
				fn()
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
			body, extra := sink.send(t, func() {
				CaptureMessage("extra failure",
					WithExtra("ch", withChan{Ch: make(chan int), Hidden: "secret-ch-tag", priv: "secret-ch-priv"}),
					WithExtra("fn", withFunc{Fn: func() {}, Hidden: "secret-fn-tag", priv: "secret-fn-priv"}),
					WithExtra("nan_struct", withNaN{Score: math.NaN(), Hidden: "secret-nan-tag", priv: "secret-nan-priv"}),
					WithExtra("bad_json", failingJSONSecret{priv: "secret-json-err"}),
					WithExtra("bad_text", failingTextSecret{priv: "secret-text-err"}),
					WithExtra("panic_json", panickingExtraSecret{priv: "secret-panic-err"}),
					WithExtra("cause", errors.New("boom")),
					WithExtra("ratio", math.NaN()),
					WithExtra("inf", math.Inf(1)),
					WithExtra("ok", 1),
				)
			})
			for _, leaked := range []string{
				"secret-ch-tag", "secret-ch-priv",
				"secret-fn-tag", "secret-fn-priv",
				"secret-nan-tag", "secret-nan-priv",
				"secret-json-err", "secret-text-err", "secret-panic-err",
			} {
				is.True(!strings.Contains(body, leaked))
			}
			for _, key := range []string{"ch", "fn", "nan_struct", "bad_json", "bad_text"} {
				s, ok := extra[key].(string)
				is.True(ok)
				is.True(strings.Contains(s, "could not encode"))
			}
			p, ok := extra["panic_json"].(string)
			is.True(ok)
			is.True(strings.Contains(p, "panicked"))
			is.Eq(extra["cause"], "boom")
			is.Eq(extra["ratio"], "NaN")
			is.Eq(extra["inf"], "+Inf")
			is.Eq(extra["ok"], 1.0)
		})
	}
}

// TestEncodeExtraHonorsErrorMarshalers: WithExtra uses MarshalJSON,
// MarshalJSONTo, MarshalText, or AppendText before Error() on an error
// value, and replaces a failing or panicking marshal with a fixed note.
func TestEncodeExtraHonorsErrorMarshalers(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	srv := newSockeyeServer(t)
	defer srv.Close()
	is.NoErr(Init(Options{DSN: testDSN, HTTPClient: rewriteTransport(srv.URL)}))
	defer Flush(time.Second)

	CaptureMessage("extra error marshalers",
		WithExtra("je", jsonRedactedErr{raw: "secret-extra-je"}),
		WithExtra("jte", jsonToRedactedErr{raw: "secret-extra-jte"}),
		WithExtra("te", textRedactedErr{raw: "secret-extra-te"}),
		WithExtra("ae", appendTextRedactedErr{raw: "secret-extra-ae"}),
		WithExtra("fe", failingJSONErr{raw: "secret-extra-fe"}),
		WithExtra("pe", panickingJSONErr{raw: "secret-extra-pe"}),
		WithExtra("plain", errors.New("plain extra error")),
	)
	Flush(time.Second)

	ev := srv.firstEvent(t)
	body, err := json.Marshal(ev)
	is.NoErr(err)
	for _, leaked := range []string{
		"secret-extra-je", "secret-extra-jte", "secret-extra-te",
		"secret-extra-ae", "secret-extra-fe", "secret-extra-pe",
	} {
		is.True(!strings.Contains(string(body), leaked))
	}
	extra := ev["extra"].(map[string]any)
	is.Eq(extra["je"], "REDACTED_JSON_ERR")
	is.Eq(extra["jte"], "REDACTED_JSONTO_ERR")
	is.Eq(extra["te"], "REDACTED_TEXT_ERR")
	is.Eq(extra["ae"], "REDACTED_APPEND_ERR")
	is.Eq(extra["plain"], "plain extra error")
	fe, ok := extra["fe"].(string)
	is.True(ok)
	is.True(strings.Contains(fe, "could not encode"))
	pe, ok := extra["pe"].(string)
	is.True(ok)
	is.True(strings.Contains(pe, "panicked"))
}

// TestEncodeExtraHonorsPointerErrorMarshalers: WithExtra uses
// pointer-receiver MarshalJSON, MarshalJSONTo, MarshalText, and
// AppendText methods on error values passed by value or by pointer, on
// both the HTTP sink and the journal sink. Typed-nil pointers encode as
// null without panicking.
func TestEncodeExtraHonorsPointerErrorMarshalers(t *testing.T) {
	sinks := []struct {
		name string
		send func(t *testing.T, fn func()) (string, map[string]any)
	}{
		{
			name: "http",
			send: func(t *testing.T, fn func()) (string, map[string]any) {
				t.Helper()
				is := is.New(t)
				resetActive(t)
				srv := newSockeyeServer(t)
				t.Cleanup(srv.Close)
				is.NoErr(Init(Options{DSN: testDSN, HTTPClient: rewriteTransport(srv.URL)}))
				t.Cleanup(func() { Flush(time.Second) })
				fn()
				Flush(time.Second)
				ev := srv.firstEvent(t)
				body, err := json.Marshal(ev)
				is.NoErr(err)
				return string(body), ev["extra"].(map[string]any)
			},
		},
		{
			name: "journal",
			send: func(t *testing.T, fn func()) (string, map[string]any) {
				t.Helper()
				is := is.New(t)
				resetActive(t)
				var out bytes.Buffer
				is.NoErr(Init(Options{Journal: &out}))
				t.Cleanup(func() { Flush(time.Second) })
				fn()
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
			body, extra := sink.send(t, func() {
				CaptureMessage("extra pointer error marshalers",
					WithExtra("je_val", ptrJSONRedactedErr{raw: "secret-extra-pje-val"}),
					WithExtra("je_ptr", &ptrJSONRedactedErr{raw: "secret-extra-pje-ptr"}),
					WithExtra("je_nil", (*ptrJSONRedactedErr)(nil)),
					WithExtra("jte_val", ptrJSONToRedactedErr{raw: "secret-extra-pjte-val"}),
					WithExtra("jte_ptr", &ptrJSONToRedactedErr{raw: "secret-extra-pjte-ptr"}),
					WithExtra("jte_nil", (*ptrJSONToRedactedErr)(nil)),
					WithExtra("te_val", ptrTextRedactedErr{raw: "secret-extra-pte-val"}),
					WithExtra("te_ptr", &ptrTextRedactedErr{raw: "secret-extra-pte-ptr"}),
					WithExtra("te_nil", (*ptrTextRedactedErr)(nil)),
					WithExtra("ae_val", ptrAppendTextRedactedErr{raw: "secret-extra-pae-val"}),
					WithExtra("ae_ptr", &ptrAppendTextRedactedErr{raw: "secret-extra-pae-ptr"}),
					WithExtra("ae_nil", (*ptrAppendTextRedactedErr)(nil)),
					WithExtra("fe_val", ptrFailingJSONErr{raw: "secret-extra-pfe-val"}),
					WithExtra("fe_ptr", &ptrFailingJSONErr{raw: "secret-extra-pfe-ptr"}),
					WithExtra("pe_val", ptrPanickingJSONErr{raw: "secret-extra-ppe-val"}),
					WithExtra("pe_ptr", &ptrPanickingJSONErr{raw: "secret-extra-ppe-ptr"}),
					WithExtra("plain", errors.New("plain extra error")),
				)
			})
			for _, leaked := range []string{
				"secret-extra-pje-val", "secret-extra-pje-ptr",
				"secret-extra-pjte-val", "secret-extra-pjte-ptr",
				"secret-extra-pte-val", "secret-extra-pte-ptr",
				"secret-extra-pae-val", "secret-extra-pae-ptr",
				"secret-extra-pfe-val", "secret-extra-pfe-ptr",
				"secret-extra-ppe-val", "secret-extra-ppe-ptr",
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
			is.Eq(extra["plain"], "plain extra error")
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
		})
	}
}

// TestEventFieldsHaveByteLimits: tags, extras, fingerprint parts, and
// the level each have a limit, so an event built from huge caller text
// has a bounded body.
func TestEventFieldsHaveByteLimits(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	is.NoErr(Init(Options{DSN: testDSN}))
	defer Flush(time.Second)
	c := active.Load()

	big := strings.Repeat("x", 1<<20)
	var opts []Option
	for i := range maxTags + 10 {
		opts = append(opts, WithTag(fmt.Sprint("t", i), big))
		opts = append(opts, WithExtra(fmt.Sprint("e", i), big))
	}
	opts = append(opts, WithTag(big, "v"), WithExtra(big, "v"), WithLevel(big))
	parts := make([]string, maxFingerprintParts+10)
	for i := range parts {
		parts[i] = big
	}
	opts = append(opts, WithFingerprint(parts...))
	e := c.newEvent("info", opts)
	e.Message = truncate(big)

	is.Eq(len(e.Tags), maxTags)
	is.Eq(len(e.Extra), maxExtras)
	for k, v := range e.Tags {
		is.True(len(k) <= maxKeyBytes && len(v) <= maxTagBytes)
	}
	for k, v := range e.Extra {
		is.True(len(k) <= maxKeyBytes && len(v) <= maxExtraBytes)
	}
	is.Eq(len(e.Fingerprint), maxFingerprintParts)
	for _, p := range e.Fingerprint {
		is.True(len(p) <= maxFingerprintBytes)
	}
	is.True(len(e.Level) <= maxLevelBytes)
	body, err := json.Marshal(e, wireJSON)
	is.NoErr(err)
	is.True(len(body) < 256<<10)
}

// TestCutHoldsNoReferenceToTheLongString: a cut value does not share
// memory with the string it came from, so the long string can be freed.
func TestCutHoldsNoReferenceToTheLongString(t *testing.T) {
	is := is.New(t)
	long := strings.Repeat("é", maxMessageBytes)
	got := truncate(long)
	is.True(len(got) <= maxMessageBytes)
	is.True(unsafe.StringData(got) != unsafe.StringData(long))
	is.True(strings.HasPrefix(long, got))
	is.Eq(truncate("short"), "short")
}

// TestEventCopiesShortStrings: a tag, a fingerprint part, a level, or a
// message within its limit that is a substring of a long string does
// not keep that string in memory while the event waits.
func TestEventCopiesShortStrings(t *testing.T) {
	is := is.New(t)
	line := "GET /companies/show?q=" + strings.Repeat("a", 1<<20) + " HTTP/1.1"
	method, path := line[:3], line[4:19]
	c := &client{}
	e := c.newEvent("info", []Option{WithTag(method, path), WithFingerprint(method, path), WithLevel(method)})
	e.Message = truncate(path)
	is.Eq(e.Tags["GET"], "/companies/show")
	for k, v := range e.Tags {
		is.True(!shares(k, line) && !shares(v, line))
	}
	is.Eq(e.Fingerprint, []string{"GET", "/companies/show"})
	for _, p := range e.Fingerprint {
		is.True(!shares(p, line))
	}
	is.True(!shares(e.Level, line))
	is.True(!shares(e.Message, line))
}

// TestExceptionCopiesTypeTitle: the type title of a plain error is a
// prefix of the error message. The exception holds a copy of the title,
// so a queued event does not keep a long message in memory.
func TestExceptionCopiesTypeTitle(t *testing.T) {
	is := is.New(t)
	long := "bad input: " + strings.Repeat("a", 1<<20)
	for _, err := range []error{
		errors.New(long),
		fmt.Errorf("%w", errors.New(long)),
		Wrap(errors.New(strings.Repeat("a", 1<<20)), "bad input"),
	} {
		msg := err.Error()
		val := exceptionFromError(err, nil, "", "").Values[0]
		is.Eq(val.Type, "bad input")
		is.True(!shares(val.Type, msg))
		is.True(!shares(val.Value, msg))
	}
}

// TestBucketHoldsNoEventAfterItsCount: after send or flushCounts posts
// the count of a bucket, the bucket holds no event.
func TestBucketHoldsNoEventAfterItsCount(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	srv := newSockeyeServer(t)
	defer srv.Close()
	clock := newTestClock()
	is.NoErr(Init(Options{DSN: testDSN, HTTPClient: rewriteTransport(srv.URL), Clock: clock.Now}))
	defer Flush(time.Second)
	c := active.Load()

	for range fullPerWindow + 2 {
		sendNow(c, "held")
	}
	c.breakerMu.Lock()
	bk := c.buckets[fingerprintKey(c.seed, &event{Message: "held"})]
	is.True(bk.last != nil) // the 12th event waits for its count
	c.breakerMu.Unlock()

	clock.Advance(countEvery)
	c.flushCounts(false)
	c.breakerMu.Lock()
	defer c.breakerMu.Unlock()
	is.Nil(bk.last)
}

// BenchmarkCaptureException measures the work CaptureException does
// on the caller goroutine for a wrapped error with a handler-deep
// stack, a tag, a request id, and an extra.
func BenchmarkCaptureException(b *testing.B) {
	benchInit(b)
	err := deepErr(16)
	b.ReportAllocs()
	for b.Loop() {
		CaptureException(err, WithTag("job", "worker.RefreshItem"), WithRequestID("01J9ZQ4N8V2K7"), WithExtra("company_id", 42))
	}
}

// BenchmarkCaptureMessage measures CaptureMessage with a level and a
// map extra.
func BenchmarkCaptureMessage(b *testing.B) {
	benchInit(b)
	extra := map[string]any{"rows": 1200, "table": "positions", "took": 1500 * time.Millisecond}
	b.ReportAllocs()
	for b.Loop() {
		CaptureMessage("import finished with 3 skipped rows", WithLevel("warning"), WithExtra("import", extra))
	}
}

// BenchmarkExceptionFromError measures the stack frame resolution and
// the type name of one exception.
func BenchmarkExceptionFromError(b *testing.B) {
	err := deepErr(16)
	root := "/Users/dev/src/github.com/croaky/errs"
	b.ReportAllocs()
	for b.Loop() {
		exceptionFromError(err, nil, root, "github.com/croaky/errs")
	}
}

// BenchmarkEncodeExtra measures the encode of one extra on the caller
// goroutine, for small values and for a value past maxExtraBytes.
func BenchmarkEncodeExtra(b *testing.B) {
	cases := []struct {
		name string
		v    any
	}{
		{"string", "positions"},
		{"int", 42},
		{"map", map[string]any{"rows": 1200, "table": "positions", "took": 1500 * time.Millisecond}},
		{"error", errors.New("connection reset by peer")},
		{"large", strings.Repeat("x", 4<<10)},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				encodeExtra(c.v)
			}
		})
	}
}

// BenchmarkCut measures cut for a string under the limit and for one
// past it.
func BenchmarkCut(b *testing.B) {
	short := "worker.RefreshItem"
	long := strings.Repeat("é", maxMessageBytes)
	b.Run("short", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			cut(short, maxTagBytes)
		}
	})
	b.Run("long", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(maxMessageBytes)
		for b.Loop() {
			cut(long, maxMessageBytes)
		}
	})
}
