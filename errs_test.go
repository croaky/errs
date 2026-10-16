package errs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/croaky/is"
)

const testDSN = "https://abc123@o123.ingest.us.sentry.io/456"

func TestParseDSNExtractsComponents(t *testing.T) {
	is := is.New(t)
	d, err := parseDSN(testDSN)
	is.NoErr(err)
	is.True(d.scheme == "https" && d.host == "o123.ingest.us.sentry.io" &&
		d.publicKey == "abc123" && d.projectID == "456" && d.path == "")
}

func TestParseDSNEnvelopeEndpoint(t *testing.T) {
	is := is.New(t)
	d, err := parseDSN(testDSN)
	is.NoErr(err)
	got := d.envelopeEndpoint()
	want := "https://o123.ingest.us.sentry.io/api/456/envelope/"
	is.Eq(got, want)
}

func TestParseDSNAuthHeaderFormat(t *testing.T) {
	is := is.New(t)
	d, err := parseDSN(testDSN)
	is.NoErr(err)
	got := d.authHeader(1714766400, userAgent)
	for _, want := range []string{
		"Sentry sentry_version=7,",
		"sentry_timestamp=1714766400",
		"sentry_key=abc123",
		"sentry_client=" + userAgent,
	} {
		is.True(strings.Contains(got, want))
	}
}

func TestParseDSNRejectsBadInput(t *testing.T) {
	is := is.New(t)
	cases := []string{
		"",
		"not-a-url",
		"https://o123.sentry.io/456", // no public key
		"https://abc@o123.sentry.io", // no project id
	}
	for _, c := range cases {
		_, err := parseDSN(c)
		is.HasErr(err)
	}
}

type captureServer struct {
	*httptest.Server
	mu      sync.Mutex
	headers []http.Header
	events  []map[string]any
}

func newCaptureServer(t *testing.T, status int) *captureServer {
	t.Helper()
	is := is.New(t)
	cs := &captureServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
		is.Eq(len(lines), 3)
		var ev map[string]any
		if len(lines) >= 3 {
			err := json.Unmarshal([]byte(lines[2]), &ev)
			is.NoErr(err)
		}
		cs.mu.Lock()
		cs.headers = append(cs.headers, r.Header.Clone())
		cs.events = append(cs.events, ev)
		cs.mu.Unlock()
		w.WriteHeader(status)
	}))
	return cs
}

func (cs *captureServer) firstEvent(t *testing.T) map[string]any {
	t.Helper()
	is := is.New(t)
	cs.mu.Lock()
	defer cs.mu.Unlock()
	is.True(len(cs.events) > 0)
	return cs.events[0]
}

func rewriteTransport(target string) *http.Client {
	base, _ := http.DefaultTransport.(*http.Transport)
	tr := base.Clone()
	return &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			r2 := r.Clone(r.Context())
			r2.URL.Scheme = "http"
			r2.URL.Host = strings.TrimPrefix(target, "http://")
			return tr.RoundTrip(r2)
		}),
		Timeout: 5 * time.Second,
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type testCustomErr struct{}

func (testCustomErr) Error() string { return "custom" }

func resetActive(t *testing.T) {
	t.Helper()
	active.Store(nil)
}

func TestCaptureExceptionPostsEnvelopeWithFrames(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	cs := newCaptureServer(t, 200)
	defer cs.Close()

	err := Init(Options{
		DSN:          testDSN,
		Environment:  "test",
		Release:      "v1",
		ServerName:   "test-host",
		HTTPClient:   rewriteTransport(cs.URL),
		SyncDispatch: true,
	})
	is.NoErr(err)
	defer Flush(time.Second)

	err = Wrap(errors.New("kaboom"), "fetch positions")
	CaptureException(err, WithTag("job", "worker.RefreshItem"))

	ev := cs.firstEvent(t)
	is.Eq(ev["environment"], "test")
	is.Eq(ev["release"], "v1")
	is.Eq(ev["server_name"], "test-host")
	tags, _ := ev["tags"].(map[string]any)
	is.Eq(tags["job"], "worker.RefreshItem")
	exc := ev["exception"].(map[string]any)
	values := exc["values"].([]any)
	val := values[0].(map[string]any)
	is.True(strings.HasPrefix(val["value"].(string), "fetch positions: kaboom"))
	stack := val["stacktrace"].(map[string]any)
	frames := stack["frames"].([]any)
	is.True(len(frames) > 0)
	last := frames[len(frames)-1].(map[string]any)
	is.True(strings.Contains(last["function"].(string), "TestCaptureExceptionPostsEnvelopeWithFrames"))
}

func TestCaptureExceptionFallsBackToCallerStack(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	cs := newCaptureServer(t, 200)
	defer cs.Close()

	err := Init(Options{
		DSN:          testDSN,
		HTTPClient:   rewriteTransport(cs.URL),
		SyncDispatch: true,
	})
	is.NoErr(err)
	defer Flush(time.Second)

	CaptureException(fmt.Errorf("plain"))

	ev := cs.firstEvent(t)
	exc := ev["exception"].(map[string]any)
	val := exc["values"].([]any)[0].(map[string]any)
	stack := val["stacktrace"].(map[string]any)
	frames := stack["frames"].([]any)
	is.True(len(frames) > 0)
}

func TestCaptureMessageHeadersAndShape(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	cs := newCaptureServer(t, 200)
	defer cs.Close()

	err := Init(Options{
		DSN:          testDSN,
		Environment:  "test",
		HTTPClient:   rewriteTransport(cs.URL),
		SyncDispatch: true,
	})
	is.NoErr(err)
	defer Flush(time.Second)

	CaptureMessage("hello world", WithExtra("k", "v"), WithFingerprint("a", "b"), WithLevel("warning"))

	cs.mu.Lock()
	h := cs.headers[0]
	cs.mu.Unlock()
	got := h.Get("Content-Type")
	is.Eq(got, "application/x-sentry-envelope")
	is.True(strings.HasPrefix(h.Get("X-Sentry-Auth"), "Sentry "))

	ev := cs.firstEvent(t)
	is.Eq(ev["level"], "warning")
	msg := ev["message"].(map[string]any)
	is.Eq(msg["formatted"], "hello world")
	extra := ev["extra"].(map[string]any)
	is.Eq(extra["k"], "v")
	fp := ev["fingerprint"].([]any)
	is.True(len(fp) == 2 && fp[0] == "a" && fp[1] == "b")
}

func TestCaptureNoOpWhenDSNEmpty(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	err := Init(Options{})
	is.NoErr(err)
	got := CaptureException(errors.New("ignored"))
	is.Eq(got, "")
	got = CaptureMessage("ignored")
	is.Eq(got, "")
}

func TestCaptureExceptionReturnsEmptyForNilError(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	cs := newCaptureServer(t, 200)
	defer cs.Close()
	err := Init(Options{
		DSN:          testDSN,
		HTTPClient:   rewriteTransport(cs.URL),
		SyncDispatch: true,
	})
	is.NoErr(err)
	defer Flush(time.Second)
	got := CaptureException(nil)
	is.Eq(got, "")
	cs.mu.Lock()
	n := len(cs.events)
	cs.mu.Unlock()
	is.Eq(n, 0)
}

func TestSendRetriesTransientThenSucceeds(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) <= 2 {
			w.WriteHeader(502)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	err := Init(Options{
		DSN:          testDSN,
		HTTPClient:   rewriteTransport(srv.URL),
		SyncDispatch: true,
	})
	is.NoErr(err)
	defer Flush(time.Second)

	CaptureMessage("retry me")

	got := calls.Load()
	is.Eq(got, int32(3))
}

func TestSendRetriesOn429(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	err := Init(Options{
		DSN:          testDSN,
		HTTPClient:   rewriteTransport(srv.URL),
		SyncDispatch: true,
	})
	is.NoErr(err)
	defer Flush(time.Second)

	CaptureMessage("rate limited")

	got := calls.Load()
	is.Eq(got, int32(2))
}

func TestParseRetryAfter(t *testing.T) {
	is := is.New(t)
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"-1", 0},
		{"5", 5 * time.Second},
		{"Wed, 21 Oct 2026 07:28:00 GMT", 0},
		{"abc", 0},
	}
	for _, c := range cases {
		got := parseRetryAfter(c.in)
		is.Eq(got, c.want)
	}
}

func TestWrapPreservesUnwrap(t *testing.T) {
	is := is.New(t)
	base := errors.New("base")
	wrapped := Wrap(base, "context")
	is.True(errors.Is(wrapped, base))
	got := wrapped.Error()
	is.Eq(got, "context: base")
}

func TestWrapNilReturnsNil(t *testing.T) {
	is := is.New(t)
	got := Wrap(nil, "x")
	is.Nil(got)
}

func TestErrorfCarriesStack(t *testing.T) {
	is := is.New(t)
	err := Errorf("failed to %s: %w", "connect", errors.New("timeout"))
	is.Eq(err.Error(), "failed to connect: timeout")
	pcs := stackFromError(err)
	is.True(len(pcs) > 0)
}

func TestStackFromErrorWalksChain(t *testing.T) {
	is := is.New(t)
	inner := Wrap(errors.New("oops"), "layer 1")
	outer := fmt.Errorf("layer 2: %w", inner)
	pcs := stackFromError(outer)
	is.True(len(pcs) > 0)
}

func TestStackFromErrorWalksJoinedErrors(t *testing.T) {
	is := is.New(t)
	wrapped := Wrap(errors.New("underlying"), "context")
	joined := errors.Join(errors.New("plain"), wrapped)
	pcs := stackFromError(joined)
	is.True(len(pcs) > 0)
}

func panicHere() { panic("boom") }

func TestPanicErrorCarriesPanicSite(t *testing.T) {
	is := is.New(t)
	var err error
	func() {
		defer func() { err = PanicError(recover()) }()
		panicHere()
	}()

	is.Eq(err.Error(), "panic recovered: boom")
	frames := resolveFrames(stackFromError(err), "", "github.com/croaky/errs")
	is.Eq(frames[len(frames)-1].Function, "github.com/croaky/errs.panicHere")
}

func assertString(v any) { _ = v.(string) }

func TestPanicErrorWrapsErrorValue(t *testing.T) {
	is := is.New(t)
	var err error
	func() {
		defer func() { err = PanicError(recover()) }()
		assertString(nil)
	}()

	is.Eq(errorTypeName(err), "*runtime.TypeAssertionError")
	var target *runtime.TypeAssertionError
	is.True(errors.As(err, &target))
}

func TestErrorTypeNameSkipsErrsAndFmtWrappers(t *testing.T) {
	is := is.New(t)
	base := errors.New("kaboom 1")
	err := Wrap(base, "outer 2")
	got, want := errorTypeName(err), "*errors.errorString"
	is.Eq(got, want)
}

func TestErrorTypeNameTitlesAnUntypedErrorFromItsMessage(t *testing.T) {
	is := is.New(t)
	base := errors.New("at least one recipient is required")
	err := Wrap(base, "save note reminder")
	got, want := errorTypeName(err), "save note reminder"
	is.Eq(got, want)
}

func TestErrorTypeNameKeepsACustomType(t *testing.T) {
	is := is.New(t)
	err := Wrap(testCustomErr{}, "save note reminder")
	got, want := errorTypeName(err), "errs.testCustomErr"
	is.Eq(got, want)
}

func TestTitleFromMessage(t *testing.T) {
	is := is.NewRelaxed(t)
	cases := []struct {
		in   string
		want string
	}{
		{"save note reminder: at least one recipient is required", "save note reminder"},
		{"slow HTTP request: GET /search", "slow HTTP request"},
		{"company 123: not found", ""},
		{"no colon here at all", "no colon here at all"},
		{": leading colon", ""},
		{strings.Repeat("a", 41) + ": x", ""},
		{"two\nlines: x", ""},
	}
	for _, c := range cases {
		got := titleFromMessage(c.in)
		is.Eq(got, c.want)
	}
}

func TestErrorTypeNameSkipsJoinWrapper(t *testing.T) {
	is := is.New(t)
	err := errors.Join(fmt.Errorf("wrapped: %w", testCustomErr{}), errors.New("plain"))
	got, want := errorTypeName(err), "errs.testCustomErr"
	is.Eq(got, want)
}

func TestClockOptionSetsTimestamps(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	cs := newCaptureServer(t, 200)
	defer cs.Close()

	fixed := time.Date(2026, 10, 12, 14, 0, 0, 0, time.UTC)
	err := Init(Options{
		DSN:          testDSN,
		Clock:        func() time.Time { return fixed },
		HTTPClient:   rewriteTransport(cs.URL),
		SyncDispatch: true,
	})
	is.NoErr(err)
	defer Flush(time.Second)

	CaptureMessage("timed message")
	ev := cs.firstEvent(t)
	is.Eq(ev["timestamp"], "2026-10-12T14:00:00Z")
}

func TestInAppPrefixOption(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	cs := newCaptureServer(t, 200)
	defer cs.Close()

	err := Init(Options{
		DSN:          testDSN,
		InAppPrefix:  "github.com/croaky/errs",
		HTTPClient:   rewriteTransport(cs.URL),
		SyncDispatch: true,
	})
	is.NoErr(err)
	defer Flush(time.Second)

	CaptureException(errors.New("prefix test"))
	ev := cs.firstEvent(t)
	exc := ev["exception"].(map[string]any)
	val := exc["values"].([]any)[0].(map[string]any)
	stack := val["stacktrace"].(map[string]any)
	frames := stack["frames"].([]any)
	is.True(len(frames) > 0)
	last := frames[len(frames)-1].(map[string]any)
	is.Eq(last["in_app"], true)
}

func TestInitReadsReleaseFromBuildInfo(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	is.NoErr(Init(Options{DSN: testDSN, SyncDispatch: true}))
	defer Flush(time.Second)
	is.Eq(active.Load().release, buildRevision())

	resetActive(t)
	is.NoErr(Init(Options{DSN: testDSN, Release: "v9", SyncDispatch: true}))
	is.Eq(active.Load().release, "v9")
}
