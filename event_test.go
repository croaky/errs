package errs

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/croaky/is"
)

// TestCaptureCopiesFingerprintAtCapture: a host changes a slice right
// after it passes the slice to WithFingerprint. The event holds the
// value at capture, for a sent event and for a held event, and
// go test -race sees no race with the worker.
func TestCaptureCopiesFingerprintAtCapture(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	srv := newSockeyeServer(t)
	defer srv.Close()
	is.NoErr(Init(Options{DSN: testDSN, HTTPClient: rewriteTransport(srv.URL), Clock: newTestClock().Now}))

	fp := []string{"same"}
	for range fullPerWindow + 5 {
		CaptureMessage("busy", WithExtra("k", "v"), WithFingerprint(fp...))
		fp[0] = "changed"
		fp[0] = "same"
	}
	Flush(5 * time.Second)

	reqs := srv.requests()
	first := reqs[0].ev["extra"].(map[string]any)
	is.Eq(first["k"], "v")
	total := 0
	for _, r := range reqs {
		n, _ := r.ev["count"].(float64)
		total += max(int(n), 1)
	}
	is.Eq(total, fullPerWindow+5)
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

// TestEventCopiesShortStrings: a tag, an extra, a fingerprint part, a
// level, or a message within its limit that is a substring of a long
// string does not keep that string in memory while the event waits.
func TestEventCopiesShortStrings(t *testing.T) {
	is := is.New(t)
	line := "GET /companies/show?q=" + strings.Repeat("a", 1<<20) + " HTTP/1.1"
	method, path := line[:3], line[4:19]
	c := &client{}
	e := c.newEvent("info", []Option{WithTag(method, path), WithExtra(method, path), WithFingerprint(method, path), WithLevel(method)})
	e.Message = truncate(path)
	is.Eq(e.Tags["GET"], "/companies/show")
	for k, v := range e.Tags {
		is.True(!shares(k, line) && !shares(v, line))
	}
	is.Eq(e.Extra["GET"], "/companies/show")
	for k, v := range e.Extra {
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
		CaptureException(err, WithTag("job", "worker.RefreshItem"), WithRequestID("01J9ZQ4N8V2K7"), WithExtra("company_id", "42"))
	}
}

// BenchmarkCaptureMessage measures CaptureMessage with a level and an
// extra.
func BenchmarkCaptureMessage(b *testing.B) {
	benchInit(b)
	b.ReportAllocs()
	for b.Loop() {
		CaptureMessage("import finished with 3 skipped rows", WithLevel("warning"), WithExtra("table", "positions"))
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

type typedNilErr struct{ msg string }

func (e *typedNilErr) Error() string { return e.msg }

type panickingError struct{}

func (*panickingError) Error() string { panic("error boom") }

type customWrapErr struct{ err error }

func (e customWrapErr) Error() string { return "wrap: " + e.err.Error() }
func (e customWrapErr) Unwrap() error { return e.err }

type panicUnwrapErr struct{}

func (panicUnwrapErr) Error() string { return "unwrap-ok" }
func (panicUnwrapErr) Unwrap() error { panic("unwrap boom") }

type panicUnwrapMultiErr struct{}

func (panicUnwrapMultiErr) Error() string   { return "unwrap-multi-ok" }
func (panicUnwrapMultiErr) Unwrap() []error { panic("unwrap multi boom") }

type panicStackErr struct{}

func (panicStackErr) Error() string         { return "stack-ok" }
func (panicStackErr) StackTrace() []uintptr { panic("stack boom") }

// TestCaptureExceptionRecoversHostErrorPanics verifies that a panicking
// Error, Unwrap, or StackTrace method on a reported error (top-level,
// below fmt.Errorf("%w"), or inside errors.Join) never escapes
// CaptureException and still sends an event with a fixed placeholder.
func TestCaptureExceptionRecoversHostErrorPanics(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantValue string
	}{
		{"typed-nil", (*typedNilErr)(nil), panickedErrorText},
		{"panic-error", (*panickingError)(nil), panickedErrorText},
		{"panic-unwrap", panicUnwrapErr{}, "unwrap-ok"},
		{"panic-unwrap-multi", panicUnwrapMultiErr{}, "unwrap-multi-ok"},
		{"panic-stack", panicStackErr{}, "stack-ok"},
		{"wrapped-typed-nil", fmt.Errorf("wrap: %w", (*typedNilErr)(nil)), "wrap: <nil>"},
		{"custom-wrapped-typed-nil", customWrapErr{err: (*typedNilErr)(nil)}, panickedErrorText},
		{"wrapped-panic-unwrap", fmt.Errorf("wrap: %w", panicUnwrapErr{}), "wrap: unwrap-ok"},
		{"wrapped-panic-stack", fmt.Errorf("wrap: %w", panicStackErr{}), "wrap: stack-ok"},
		{"joined-typed-nil", errors.Join(errors.New("ok"), (*typedNilErr)(nil)), panickedErrorText},
		{"joined-panic-unwrap", errors.Join(panicUnwrapErr{}, errors.New("ok")), "unwrap-ok\nok"},
		{"joined-panic-stack", errors.Join(panicStackErr{}, errors.New("ok")), "stack-ok\nok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			is := is.New(t)
			resetActive(t)
			srv := newSockeyeServer(t)
			defer srv.Close()
			is.NoErr(Init(Options{DSN: testDSN, HTTPClient: rewriteTransport(srv.URL)}))
			defer Flush(time.Second)

			id := CaptureException(tc.err)
			is.True(id != "")
			Flush(time.Second)

			ev := srv.firstEvent(t)
			exc := ev["exception"].(map[string]any)
			val := exc["values"].([]any)[0].(map[string]any)
			is.Eq(val["value"], tc.wantValue)
		})
	}
}
