package errs

import (
	"encoding/json/v2"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/croaky/is"
)

const testSockeyeDSN = "https://sek_test@sockeye.example/errors/app/web"

type sockeyeRequest struct {
	path string
	auth string
	ct   string
	ev   map[string]any
}

// sockeyeServer records each request and answers with status, which a
// test may change while it runs.
type sockeyeServer struct {
	*httptest.Server
	mu     sync.Mutex
	reqs   []sockeyeRequest
	status atomic.Int32
}

func newSockeyeServer(t *testing.T) *sockeyeServer {
	t.Helper()
	s := &sockeyeServer{}
	s.status.Store(202)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var ev map[string]any
		if strings.HasPrefix(r.URL.Path, "/errors/") {
			_ = json.Unmarshal(body, &ev)
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, sockeyeRequest{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), ev})
		s.mu.Unlock()
		w.WriteHeader(int(s.status.Load()))
	}))
	return s
}

func (s *sockeyeServer) requests() []sockeyeRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sockeyeRequest(nil), s.reqs...)
}

func (s *sockeyeServer) firstEvent(t *testing.T) map[string]any {
	t.Helper()
	reqs := s.requests()
	if len(reqs) == 0 {
		t.Fatal("no requests received")
	}
	return reqs[0].ev
}

// testClock is a clock a test moves by hand.
type testClock struct{ ns atomic.Int64 }

func newTestClock() *testClock {
	c := &testClock{}
	c.ns.Store(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *testClock) Now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *testClock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

func TestParseSockeyeDSN(t *testing.T) {
	is := is.New(t)
	d, err := parseDSN(testSockeyeDSN)
	is.NoErr(err)
	is.Eq(d.endpoint, "https://sockeye.example/errors/app/web")
	is.Eq(d.key, "sek_test")

	d, err = parseDSN("http://k@localhost:8080/errors/a/b/")
	is.NoErr(err)
	is.Eq(d.endpoint, "http://localhost:8080/errors/a/b")
}

func TestParseSockeyeDSNRejectsBadInput(t *testing.T) {
	for _, s := range []string{
		"sockeye.example/errors/app/web",
		"ftp://k@sockeye.example/errors/app/web",
		"https://sockeye.example/errors/app/web",
		"https://k@sockeye.example/errors/app",
		"https://k@sockeye.example/api/app/web",
		"https://k@sockeye.example/errors/app/web/more",
	} {
		_, err := parseDSN(s)
		if err == nil || !strings.Contains(err.Error(), "parse dsn") {
			t.Fatalf("%q: want a parse error, got %v", s, err)
		}
	}
}

func TestInitRejectsBadSockeyeDSN(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	is.True(Init(Options{SockeyeDSN: "nope"}) != nil)
}

func TestSockeyeDSNAloneEnablesReporting(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	srv := newSockeyeServer(t)
	defer srv.Close()

	is.NoErr(Init(Options{
		SockeyeDSN:  testSockeyeDSN,
		Environment: "test",
		Release:     "abc123def",
		ServerName:  "host-1",
		HTTPClient:  rewriteTransport(srv.URL),
	}))
	defer Flush(time.Second)

	err := Wrap(testCustomErr{}, "load page")
	id := CaptureException(err, WithTag("job", "x"), WithRequestID("req-9"))
	Flush(time.Second)
	is.True(id != "")

	reqs := srv.requests()
	is.Eq(len(reqs), 1)
	r := reqs[0]
	is.Eq(r.path, "/errors/app/web")
	is.Eq(r.auth, "Bearer sek_test")
	is.Eq(r.ct, "application/json")
	is.Eq(r.ev["event_id"], id)
	is.Eq(r.ev["release"], "abc123def")
	is.Eq(r.ev["server_name"], "host-1")
	tags := r.ev["tags"].(map[string]any)
	is.Eq(tags["job"], "x")
	is.Eq(tags["request_id"], "req-9")
	vals := r.ev["exception"].(map[string]any)["values"].([]any)
	frames := vals[0].(map[string]any)["stacktrace"].(map[string]any)["frames"].([]any)
	is.True(len(frames) > 0)
	_, hasCount := r.ev["count"]
	is.True(!hasCount)
}

func TestSockeyeMessageIsAString(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	srv := newSockeyeServer(t)
	defer srv.Close()
	is.NoErr(Init(Options{SockeyeDSN: testSockeyeDSN, HTTPClient: rewriteTransport(srv.URL)}))
	defer Flush(time.Second)

	CaptureMessage("deploy finished", WithLevel("info"))
	Flush(time.Second)

	reqs := srv.requests()
	is.Eq(len(reqs), 1)
	is.Eq(reqs[0].ev["message"], "deploy finished")
	is.Eq(reqs[0].ev["level"], "info")
}

func TestSockeyeDSNAlias(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	srv := newSockeyeServer(t)
	defer srv.Close()
	is.NoErr(Init(Options{
		SockeyeDSN: testSockeyeDSN,
		HTTPClient: rewriteTransport(srv.URL),
	}))
	defer Flush(time.Second)

	CaptureMessage("via alias")
	Flush(time.Second)

	reqs := srv.requests()
	is.Eq(len(reqs), 1)
	is.Eq(reqs[0].ev["message"], "via alias")
}

func TestSockeyeSamplingKeepsTheTotalExact(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	srv := newSockeyeServer(t)
	defer srv.Close()
	clock := newTestClock()
	is.NoErr(Init(Options{
		SockeyeDSN: testSockeyeDSN,
		HTTPClient: rewriteTransport(srv.URL),
		Clock:      clock.Now,
	}))

	for range 30 {
		CaptureMessage("same error 42")
	}
	Flush(time.Second)

	reqs := srv.requests()
	total := 0
	for _, r := range reqs {
		n, _ := r.ev["count"].(float64)
		total += max(int(n), 1)
	}
	is.Eq(total, 30)
	is.True(len(reqs) < 30)
	for _, r := range reqs[:fullPerWindow] {
		_, has := r.ev["count"]
		is.True(!has)
	}
}

func TestSockeyeSamplingKeepsErrorsApart(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	srv := newSockeyeServer(t)
	defer srv.Close()
	is.NoErr(Init(Options{SockeyeDSN: testSockeyeDSN, HTTPClient: rewriteTransport(srv.URL), Clock: newTestClock().Now}))
	defer Flush(time.Second)

	for range fullPerWindow {
		CaptureMessage("first")
	}
	CaptureMessage("second")
	Flush(time.Second)

	is.Eq(len(srv.requests()), fullPerWindow+1)
}

func TestSockeyeSamplingStartsOverAfterTheWindow(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	srv := newSockeyeServer(t)
	defer srv.Close()
	clock := newTestClock()
	is.NoErr(Init(Options{SockeyeDSN: testSockeyeDSN, HTTPClient: rewriteTransport(srv.URL), Clock: clock.Now}))
	defer Flush(time.Second)
	c := active.Load()

	for range fullPerWindow {
		sendNow(c, "again")
	}
	clock.Advance(sampleWindow)
	sendNow(c, "again")

	reqs := srv.requests()
	is.Eq(len(reqs), fullPerWindow+1)
	_, has := reqs[len(reqs)-1].ev["count"]
	is.True(!has)
}

func TestFlushCountsSendsOnlyDueCounts(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	srv := newSockeyeServer(t)
	defer srv.Close()
	clock := newTestClock()
	is.NoErr(Init(Options{SockeyeDSN: testSockeyeDSN, HTTPClient: rewriteTransport(srv.URL), Clock: clock.Now}))
	c := active.Load()

	for range fullPerWindow + 3 {
		sendNow(c, "burst")
	}
	// Events 11 sent a count of 1; events 12 and 13 are held.
	before := len(srv.requests())
	c.flushCounts(false)
	is.Eq(len(srv.requests()), before)

	clock.Advance(countEvery)
	c.flushCounts(false)
	reqs := srv.requests()
	is.Eq(len(reqs), before+1)
	is.Eq(reqs[len(reqs)-1].ev["count"], float64(2))
}

func TestSockeyeBreakerOpensAndReportsDropped(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	srv := newSockeyeServer(t)
	defer srv.Close()
	srv.status.Store(500)
	clock := newTestClock()
	is.NoErr(Init(Options{SockeyeDSN: testSockeyeDSN, HTTPClient: rewriteTransport(srv.URL), Clock: clock.Now}))
	defer Flush(time.Second)
	c := active.Load()

	for i := range breakerFailures {
		sendNow(c, "down", WithTag("i", string(rune('a'+i))))
	}
	tries := len(srv.requests())
	is.Eq(tries, breakerFailures*len(retryDelays))

	sendNow(c, "while open")
	is.Eq(len(srv.requests()), tries)

	srv.status.Store(202)
	clock.Advance(breakerPauses[0])
	sendNow(c, "back")

	reqs := srv.requests()
	last := reqs[len(reqs)-1]
	is.Eq(len(reqs), tries+1)
	extra := last.ev["extra"].(map[string]any)
	is.Eq(extra["dropped"], float64(breakerFailures+1))

	sendNow(c, "after")
	reqs = srv.requests()
	_, has := reqs[len(reqs)-1].ev["extra"]
	is.True(!has)
}

func TestSockeyeBreakerReopensAfterOneFailedTry(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	srv := newSockeyeServer(t)
	defer srv.Close()
	srv.status.Store(500)
	clock := newTestClock()
	is.NoErr(Init(Options{SockeyeDSN: testSockeyeDSN, HTTPClient: rewriteTransport(srv.URL), Clock: clock.Now}))
	defer Flush(time.Second)
	c := active.Load()

	for range breakerFailures {
		sendNow(c, "down")
	}
	clock.Advance(breakerPauses[0])
	before := len(srv.requests())
	sendNow(c, "probe")
	is.Eq(len(srv.requests()), before+len(retryDelays))
	sendNow(c, "next")
	is.Eq(len(srv.requests()), before+len(retryDelays))
}

func TestSockeyeBreakerPausesBackOff(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	srv := newSockeyeServer(t)
	defer srv.Close()
	srv.status.Store(500)
	clock := newTestClock()
	is.NoErr(Init(Options{SockeyeDSN: testSockeyeDSN, HTTPClient: rewriteTransport(srv.URL), Clock: clock.Now}))
	defer Flush(time.Second)
	c := active.Load()

	for range breakerFailures {
		sendNow(c, "down")
	}

	for i, pause := range breakerPauses {
		// Just before the pause ends, breaker remains open.
		clock.Advance(pause - time.Second)
		before := len(srv.requests())
		sendNow(c, "blocked")
		is.Eq(len(srv.requests()), before)

		// Advance past the pause to allow one probe.
		clock.Advance(time.Second)
		sendNow(c, string(rune('a'+i)))
		is.Eq(len(srv.requests()), before+len(retryDelays))
	}
}

func TestFullQueueCountsDropped(t *testing.T) {
	is := is.New(t)
	c := &client{queue: make(chan *event, 1)}
	c.dispatch(&event{})
	c.dispatch(&event{})
	is.Eq(c.dropped.Load(), int64(1))
}

func TestFingerprintKey(t *testing.T) {
	is := is.New(t)
	own := &event{Fingerprint: []string{"slow", "GET", "/x"}}
	is.Eq(fingerprintKey(own), "own:slow\x00GET\x00/x")

	a := &event{Message: "job 17 failed"}
	b := &event{Message: "job 18 failed"}
	is.Eq(fingerprintKey(a), fingerprintKey(b))

	framed := func(fn string) *event {
		return &event{Exception: &exceptionPayload{Values: []exceptionValue{{
			Type: "T", Value: "v 1",
			Stacktrace: &stacktrace{Frames: []frame{{Function: fn, Filename: "a.go", Lineno: 3, InApp: true}}},
		}}}}
	}
	is.Eq(fingerprintKey(framed("f")), fingerprintKey(framed("f")))
	is.True(fingerprintKey(framed("f")) != fingerprintKey(framed("g")))

	// The exact keys, so a change to their form shows in a test.
	is.Eq(fingerprintKey(a), "message:job ? failed")
	is.Eq(fingerprintKey(framed("f")), "frames:T\x00f@a.go")
	deep := framed("f")
	deep.Exception.Values[0].Stacktrace.Frames = []frame{
		{Function: "a", Filename: "a.go", InApp: true},
		{Function: "lib", Filename: "lib.go"},
		{Function: "b", Filename: "b.go", InApp: true},
		{Function: "c", Filename: "c.go", InApp: true},
		{Function: "d", Filename: "d.go", InApp: true},
	}
	is.Eq(fingerprintKey(deep), "frames:T\x00d@d.go\x00c@c.go\x00b@b.go")
	deep.Exception.Values[0].Stacktrace = nil
	is.Eq(fingerprintKey(deep), "type:T\x00v ?")
	is.Eq(fingerprintKey(&event{}), "")
}

func TestAsyncFlushSendsHeldCounts(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	srv := newSockeyeServer(t)
	defer srv.Close()
	is.NoErr(Init(Options{SockeyeDSN: testSockeyeDSN, HTTPClient: rewriteTransport(srv.URL), Clock: newTestClock().Now}))

	for range 25 {
		CaptureMessage("queued")
	}
	Flush(5 * time.Second)

	total := 0
	for _, r := range srv.requests() {
		n, _ := r.ev["count"].(float64)
		total += max(int(n), 1)
	}
	is.Eq(total, 25)
}

// TestSockeyeEventSurvivesBadValues: an event with invalid UTF-8 in its
// message, a tag, and an extra value still reaches sockeye, with U+FFFD
// in place of each bad byte. A time.Duration goes as integer
// nanoseconds, as encoding/json v1 sent it. A value JSON cannot encode
// goes as its fmt.Sprint text, and the rest of extra stays as it is.
func TestSockeyeEventSurvivesBadValues(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	srv := newSockeyeServer(t)
	defer srv.Close()
	is.NoErr(Init(Options{SockeyeDSN: testSockeyeDSN, HTTPClient: rewriteTransport(srv.URL)}))
	defer Flush(time.Second)

	CaptureMessage("bad \xff byte",
		WithTag("path", "/a\xffb"),
		WithExtra("output", "x\xfey"),
		WithExtra("took", 1500*time.Millisecond),
		WithExtra("cause", errors.New("boom")),
		WithExtra("ratio", math.NaN()),
		WithExtra("page", 2),
	)
	Flush(time.Second)

	ev := srv.firstEvent(t)
	is.Eq(ev["message"], "bad \uFFFD byte")
	is.Eq(ev["tags"].(map[string]any)["path"], "/a\uFFFDb")
	extra := ev["extra"].(map[string]any)
	is.Eq(extra["output"], "x\uFFFDy")
	is.Eq(extra["took"], 1.5e9)
	is.Eq(extra["cause"], "boom")
	is.Eq(extra["ratio"], "NaN")
	is.Eq(extra["page"], 2.0)
}

// TestFlushCountsForgetsQuietErrors: after many errors with distinct
// text, the worker's flushCounts keeps only the errors of the last
// sample window.
func TestFlushCountsForgetsQuietErrors(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	srv := newSockeyeServer(t)
	defer srv.Close()
	clock := newTestClock()
	is.NoErr(Init(Options{SockeyeDSN: testSockeyeDSN, HTTPClient: rewriteTransport(srv.URL), Clock: clock.Now}))
	defer Flush(time.Second)
	c := active.Load()

	for _, s := range []string{"a", "b", "c"} {
		sendNow(c, "old "+s)
	}
	clock.Advance(sampleWindow)
	sendNow(c, "new")
	c.flushCounts(false)

	c.breakerMu.Lock()
	defer c.breakerMu.Unlock()
	is.Eq(len(c.buckets), 1)
}

// sendNow runs a message event through c.send on the test goroutine, so
// a test of sampling or of the breaker reads the result at once.
func sendNow(c *client, msg string, opts ...Option) {
	e := c.newEvent("info", opts)
	e.Message = truncate(msg)
	c.send(e)
}

// benchEvent returns an exception event as CaptureException builds it.
func benchEvent(b *testing.B) *event {
	b.Helper()
	c := &client{environment: "production", release: "4387d81", serverName: "web-1"}
	e := c.newEvent("error", []Option{WithTag("job", "worker.RefreshItem"), WithRequestID("01J9ZQ4N8V2K7"), WithExtra("company_id", 42)})
	e.Exception = exceptionFromError(deepErr(16), nil, "/Users/dev/src/github.com/croaky/errs", "github.com/croaky/errs")
	return e
}

// BenchmarkFingerprintKey measures the sampling key for each of its
// three forms.
func BenchmarkFingerprintKey(b *testing.B) {
	framed := benchEvent(b)
	typed := benchEvent(b)
	typed.Exception.Values[0].Stacktrace = nil
	cases := []struct {
		name string
		e    *event
	}{
		{"own", &event{Fingerprint: []string{"slow", "GET", "/companies/show"}}},
		{"frames", framed},
		{"type", typed},
		{"message", &event{Message: "job 1742 failed after 3 tries in 2500 ms"}},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				fingerprintKey(c.e)
			}
		})
	}
}

// BenchmarkEncodeEvent measures the JSON body of one exception event.
func BenchmarkEncodeEvent(b *testing.B) {
	e := benchEvent(b)
	body, err := json.Marshal(e, wireJSON)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		_, _ = json.Marshal(e, wireJSON)
	}
}

// BenchmarkSendHeld measures the worker path for an error past
// fullPerWindow, which holds the event and sends nothing. An error
// storm runs this path for each event.
func BenchmarkSendHeld(b *testing.B) {
	clock := newTestClock()
	c := &client{clock: clock.Now, httpClient: acceptClient(), buckets: map[string]*bucket{}}
	e := benchEvent(b)
	for range fullPerWindow + 1 {
		c.send(e)
	}
	b.ReportAllocs()
	for b.Loop() {
		c.send(e)
	}
}

// TestWriteNoDigitsMatchesRegexp: writeNoDigits gives the result of the
// regexp [0-9]+ with ReplaceAllString, the form fingerprintKey had.
func TestWriteNoDigitsMatchesRegexp(t *testing.T) {
	is := is.New(t)
	re := regexp.MustCompile(`[0-9]+`)
	for _, s := range []string{
		"", "7", "abc", "job 17 failed", "0042abc99", "x1y22z333",
		"bad \xff 12 byte", "١٢٣ arabic digits stay", "a-1.5e10",
	} {
		var b strings.Builder
		writeNoDigits(&b, s)
		is.Eq(b.String(), re.ReplaceAllString(s, "?"))
	}
}
