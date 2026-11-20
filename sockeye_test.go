package errs

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"hash/maphash"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"strconv"
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
	seed := maphash.MakeSeed()
	key := func(e *event) uint64 { return fingerprintKey(seed, e) }
	own := &event{Fingerprint: []string{"slow", "GET", "/x"}}
	is.Eq(key(own), maphash.String(seed, "own:slow\x00GET\x00/x"))

	a := &event{Message: "job 17 failed"}
	b := &event{Message: "job 18 failed"}
	is.Eq(key(a), key(b))

	framed := func(fn string) *event {
		return &event{Exception: &exceptionPayload{Values: []exceptionValue{{
			Type: "T", Value: "v 1",
			Stacktrace: &stacktrace{Frames: []frame{{Function: fn, Filename: "a.go", Lineno: 3, InApp: true}}},
		}}}}
	}
	is.Eq(key(framed("f")), key(framed("f")))
	is.True(key(framed("f")) != key(framed("g")))

	// The text that each key hashes, so a change to its form shows in a
	// test.
	is.Eq(key(a), maphash.String(seed, "message:job ? failed"))
	is.Eq(key(framed("f")), maphash.String(seed, "frames:T\x00f@a.go"))
	deep := framed("f")
	deep.Exception.Values[0].Stacktrace.Frames = []frame{
		{Function: "a", Filename: "a.go", InApp: true},
		{Function: "lib", Filename: "lib.go"},
		{Function: "b", Filename: "b.go", InApp: true},
		{Function: "c", Filename: "c.go", InApp: true},
		{Function: "d", Filename: "d.go", InApp: true},
	}
	is.Eq(key(deep), maphash.String(seed, "frames:T\x00d@d.go\x00c@c.go\x00b@b.go"))
	deep.Exception.Values[0].Stacktrace = nil
	is.Eq(key(deep), maphash.String(seed, "type:T\x00v ?"))
	is.Eq(key(&event{}), maphash.String(seed, ""))

	// A seed sets the key, so a caller cannot craft two texts with one
	// hash for every client. The key never names the overflow bucket.
	is.True(key(a) != fingerprintKey(maphash.MakeSeed(), a))
	is.True(key(a) != overflowKey)
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
// nanoseconds, as encoding/json v1 sent it. An error goes as Error(),
// a non-finite float goes as its text, and the rest of extra stays as
// it is.
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

// keyCollisions are pairs of keys that name one JSON object member
// once the encoder replaces each invalid byte with U+FFFD.
var keyCollisions = [][2]string{
	{"k\xff", "k\xfe"},
	{"k\xff", "k\uFFFD"},
	{strings.Repeat("a", 60) + "\xff\xfe\xfe\xfe", strings.Repeat("a", 60) + "\xff\xfe\xfe\xfd"},
	// Each key is 64 bytes. The replacement makes each one longer, and
	// the cut then trims both to the same 63 bytes.
	{strings.Repeat("a", 63) + "\xff", strings.Repeat("a", 63) + "\xfe"},
}

// TestEventKeysAreCanonical: tag keys, extra keys, and slog attribute
// keys that differ only in invalid UTF-8 do not drop the event. The
// event arrives with one key, and the last one in option order wins.
// The same holds for a held count event, and the dropped counter does
// not change.
func TestEventKeysAreCanonical(t *testing.T) {
	captures := []struct {
		name  string
		field string
		send  func(a, b string)
	}{
		{"WithTag", "tags", func(a, b string) {
			CaptureMessage("keys", WithTag(a, "first"), WithTag(b, "second"))
		}},
		{"WithExtra", "extra", func(a, b string) {
			CaptureMessage("keys", WithExtra(a, "first"), WithExtra(b, "second"))
		}},
		{"SlogHandler", "extra", func(a, b string) {
			slog.New(SlogHandler(slog.NewTextHandler(io.Discard, nil))).Error("keys", a, "first", b, "second")
		}},
	}
	for _, cp := range captures {
		for i, pair := range keyCollisions {
			for _, held := range []bool{false, true} {
				name := cp.name + "/" + strconv.Itoa(i) + "/whole"
				n := 1
				if held {
					name = cp.name + "/" + strconv.Itoa(i) + "/held"
					n = fullPerWindow + 3
				}
				t.Run(name, func(t *testing.T) {
					is := is.New(t)
					resetActive(t)
					srv := newSockeyeServer(t)
					defer srv.Close()
					is.NoErr(Init(Options{SockeyeDSN: testSockeyeDSN, HTTPClient: rewriteTransport(srv.URL), Clock: newTestClock().Now}))
					c := active.Load()

					for range n {
						cp.send(pair[0], pair[1])
					}
					Flush(5 * time.Second)

					total := 0
					for _, r := range srv.requests() {
						f, _ := r.ev[cp.field].(map[string]any)
						delete(f, "log")
						is.Eq(len(f), 1)
						for _, v := range f {
							is.Eq(v, "second")
						}
						count, _ := r.ev["count"].(float64)
						total += max(int(count), 1)
					}
					is.Eq(total, n)
					is.Eq(c.dropped.Load(), int64(0))
				})
			}
		}
	}
}

// TestEventKey: eventKey makes a key valid UTF-8 and cuts it to
// maxKeyBytes at a rune boundary. A valid key within the limit stays.
func TestEventKey(t *testing.T) {
	is := is.New(t)
	is.Eq(eventKey("request_id"), "request_id")
	is.Eq(eventKey("naïve"), "naïve")
	is.Eq(eventKey("k\xff"), "k\uFFFD")
	is.Eq(eventKey("k\xff\xfe"), eventKey("k\xfd"))
	is.Eq(eventKey(strings.Repeat("a", 100)), strings.Repeat("a", maxKeyBytes))
	is.Eq(eventKey(strings.Repeat("a", 63)+"\xff"), strings.Repeat("a", 63))
	for _, s := range []string{strings.Repeat("\xff", 1000), strings.Repeat("\xffa", 1000), strings.Repeat("é", 100)} {
		k := eventKey(s)
		is.True(len(k) <= maxKeyBytes)
		is.True(strings.ToValidUTF8(k, "") == k)
	}
}

// samplingClient returns a client with no worker, for a test that
// calls send. A nil journal and a breaker pushed an hour out make
// post drop each event, as an outage does.
func samplingClient(journal io.Writer, clock *testClock) *client {
	c := &client{clock: clock.Now}
	if journal != nil {
		c.journal = journal
	} else {
		c.openUntil = clock.Now().Add(time.Hour)
	}
	return c
}

// countWriter is a journal writer that adds up the events its lines
// carry, and keeps no line. A count event adds its count. Any event
// adds the value of extra.dropped.
type countWriter struct {
	t     *testing.T
	total int
}

func (w *countWriter) Write(b []byte) (int, error) {
	w.t.Helper()
	body, ok := strings.CutPrefix(strings.TrimSuffix(string(b), "\n"), journalPrefix)
	if !ok {
		w.t.Fatalf("journal line has no prefix: %q", b)
	}
	var e event
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		w.t.Fatal(err)
	}
	w.total += max(e.Count, 1)
	if d, ok := e.Extra["dropped"]; ok {
		n, err := strconv.Atoi(string(d))
		if err != nil {
			w.t.Fatal(err)
		}
		w.total += n
	}
	return len(b), nil
}

// TestBucketMapIsBounded: a flood of distinct errors in one window
// leaves the bucket map at maxBuckets entries or fewer, and the held
// events at maxHeldEvents or fewer. It keeps little heap, and it loses
// no count: each event is in a count, in extra.dropped, or in the
// dropped counter. It runs once with the journal sink and once with an
// open breaker.
func TestBucketMapIsBounded(t *testing.T) {
	for _, sink := range []string{"journal", "open breaker"} {
		t.Run(sink, func(t *testing.T) {
			is := is.New(t)
			clock := newTestClock()
			out := &countWriter{t: t}
			var c *client
			if sink == "journal" {
				c = samplingClient(out, clock)
			} else {
				c = samplingClient(nil, clock)
			}
			sent := 0

			// Errors that go past their limit, more than the held cap.
			for i := range maxHeldEvents + 20 {
				for range fullPerWindow + 2 {
					sendNow(c, "repeat "+strings.Repeat("x", i+1)+"!")
					sent++
				}
			}

			// A flood of distinct 4 KB errors.
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			for i := range 3 * maxBuckets {
				sendNow(c, strings.Repeat(string(rune('a'+i%26)), 2000)+strconv.Itoa(i)+strings.Repeat("-", 2000)+"x"+strings.Repeat(string(rune('A'+i%25)), i%7+1))
				sent++
			}
			runtime.GC()
			runtime.ReadMemStats(&after)

			c.breakerMu.Lock()
			is.True(len(c.buckets) <= maxBuckets)
			held := 0
			for _, bk := range c.buckets {
				if bk.last != nil {
					held++
				}
			}
			c.breakerMu.Unlock()
			is.True(held <= maxHeldEvents)
			is.True(after.HeapAlloc < before.HeapAlloc+2<<20)

			c.flushCounts(true)
			runtime.KeepAlive(c)

			accounted := int(c.dropped.Load()) + out.total
			is.Eq(accounted, sent)
		})
	}
}

// TestBucketOverflowSamplesLikeAnyBucket: past maxBuckets, new errors
// share one overflow bucket. It sends fullPerWindow events whole in a
// window, and it counts the rest. Errors that already have a bucket
// keep their own allowance.
func TestBucketOverflowSamplesLikeAnyBucket(t *testing.T) {
	is := is.New(t)
	clock := newTestClock()
	var out bytes.Buffer
	c := samplingClient(&out, clock)

	// The map holds maxBuckets entries with the overflow bucket, so
	// maxBuckets-1 errors have their own bucket.
	for i := range maxBuckets - 1 {
		sendNow(c, "known "+strings.Repeat("k", i+1)+"!")
	}
	lines := func() int { return strings.Count(out.String(), "\n") }
	base := lines()

	// New errors past the cap: fullPerWindow whole, then counts.
	for i := range fullPerWindow + 5 {
		sendNow(c, "new "+strings.Repeat("n", i+1)+"!")
	}
	is.Eq(lines()-base, fullPerWindow+1) // 10 whole and the first count

	// An error that has a bucket sends whole again.
	before := lines()
	sendNow(c, "known k!")
	is.Eq(lines()-before, 1)

	c.breakerMu.Lock()
	defer c.breakerMu.Unlock()
	is.True(len(c.buckets) <= maxBuckets)
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
			seed := maphash.MakeSeed()
			for b.Loop() {
				fingerprintKey(seed, c.e)
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
	c := &client{clock: clock.Now, httpClient: acceptClient()}
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
	seed := maphash.MakeSeed()
	for _, s := range []string{
		"", "7", "abc", "job 17 failed", "0042abc99", "x1y22z333",
		"bad \xff 12 byte", "١٢٣ arabic digits stay", "a-1.5e10",
	} {
		var h maphash.Hash
		h.SetSeed(seed)
		writeNoDigits(&h, s)
		is.Eq(h.Sum64(), maphash.String(seed, re.ReplaceAllString(s, "?")))
	}
}
