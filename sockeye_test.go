package errs

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
		SockeyeDSN:   testSockeyeDSN,
		Environment:  "test",
		Release:      "abc123def",
		ServerName:   "host-1",
		HTTPClient:   rewriteTransport(srv.URL),
		SyncDispatch: true,
	}))
	defer Flush(time.Second)

	err := Wrap(testCustomErr{}, "load page")
	id := CaptureException(err, WithTag("job", "x"), WithRequestID("req-9"))
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
	is.NoErr(Init(Options{SockeyeDSN: testSockeyeDSN, HTTPClient: rewriteTransport(srv.URL), SyncDispatch: true}))
	defer Flush(time.Second)

	CaptureMessage("deploy finished", WithLevel("info"))

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
		SockeyeDSN:   testSockeyeDSN,
		HTTPClient:   rewriteTransport(srv.URL),
		SyncDispatch: true,
	}))
	defer Flush(time.Second)

	CaptureMessage("via alias")

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
		SockeyeDSN:   testSockeyeDSN,
		HTTPClient:   rewriteTransport(srv.URL),
		SyncDispatch: true,
		Clock:        clock.Now,
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
	is.NoErr(Init(Options{SockeyeDSN: testSockeyeDSN, HTTPClient: rewriteTransport(srv.URL), SyncDispatch: true, Clock: newTestClock().Now}))
	defer Flush(time.Second)

	for range fullPerWindow {
		CaptureMessage("first")
	}
	CaptureMessage("second")

	is.Eq(len(srv.requests()), fullPerWindow+1)
}

func TestSockeyeSamplingStartsOverAfterTheWindow(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	srv := newSockeyeServer(t)
	defer srv.Close()
	clock := newTestClock()
	is.NoErr(Init(Options{SockeyeDSN: testSockeyeDSN, HTTPClient: rewriteTransport(srv.URL), SyncDispatch: true, Clock: clock.Now}))
	defer Flush(time.Second)

	for range fullPerWindow {
		CaptureMessage("again")
	}
	clock.Advance(sampleWindow)
	CaptureMessage("again")

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
	is.NoErr(Init(Options{SockeyeDSN: testSockeyeDSN, HTTPClient: rewriteTransport(srv.URL), SyncDispatch: true, Clock: clock.Now}))
	c := active.Load()

	for range fullPerWindow + 3 {
		CaptureMessage("burst")
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
	is.NoErr(Init(Options{SockeyeDSN: testSockeyeDSN, HTTPClient: rewriteTransport(srv.URL), SyncDispatch: true, Clock: clock.Now}))
	defer Flush(time.Second)

	for i := range breakerFailures {
		CaptureMessage("down", WithTag("i", string(rune('a'+i))))
	}
	tries := len(srv.requests())
	is.Eq(tries, breakerFailures*len(retryDelays))

	CaptureMessage("while open")
	is.Eq(len(srv.requests()), tries)

	srv.status.Store(202)
	clock.Advance(breakerPause)
	CaptureMessage("back")

	reqs := srv.requests()
	last := reqs[len(reqs)-1]
	is.Eq(len(reqs), tries+1)
	extra := last.ev["extra"].(map[string]any)
	is.Eq(extra["dropped"], float64(breakerFailures+1))

	CaptureMessage("after")
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
	is.NoErr(Init(Options{SockeyeDSN: testSockeyeDSN, HTTPClient: rewriteTransport(srv.URL), SyncDispatch: true, Clock: clock.Now}))
	defer Flush(time.Second)

	for range breakerFailures {
		CaptureMessage("down")
	}
	clock.Advance(breakerPause)
	before := len(srv.requests())
	CaptureMessage("probe")
	is.Eq(len(srv.requests()), before+len(retryDelays))
	CaptureMessage("next")
	is.Eq(len(srv.requests()), before+len(retryDelays))
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
