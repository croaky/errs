package errs

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/croaky/is"
)

// perfPost is one request the perf test server received.
type perfPost struct {
	path, auth, key, dropped, capture string
	body                              string
	records                           []map[string]any
}

// perfServer records each POST and answers with the next status in
// statuses, then 202. gate, when set, holds each answer until a test
// closes it.
type perfServer struct {
	*httptest.Server
	mu       sync.Mutex
	posts    []perfPost
	statuses []int
	gate     chan struct{}
	got      chan struct{}
}

func newPerfServer(t *testing.T, statuses ...int) *perfServer {
	t.Helper()
	s := &perfServer{statuses: statuses, got: make(chan struct{}, 100)}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var recs []map[string]any
		_ = json.Unmarshal(body, &recs)
		s.mu.Lock()
		s.posts = append(s.posts, perfPost{r.URL.Path, r.Header.Get("Authorization"),
			r.Header.Get("Idempotency-Key"), r.Header.Get("Dropped-Records"),
			r.Header.Get("Perf-Capture"), string(body), recs})
		status := 202
		if len(s.statuses) > 0 {
			status, s.statuses = s.statuses[0], s.statuses[1:]
		}
		gate := s.gate
		s.mu.Unlock()
		// wait polls received, so a full channel loses nothing.
		select {
		case s.got <- struct{}{}:
		default:
		}
		if gate != nil {
			<-gate
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *perfServer) received() []perfPost {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]perfPost(nil), s.posts...)
}

// wait blocks until the server has received n posts in all.
func (s *perfServer) wait(t *testing.T, n int) []perfPost {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for len(s.received()) < n {
		select {
		case <-s.got:
		case <-deadline:
			t.Fatalf("got %d posts, want %d", len(s.received()), n)
		}
	}
	return s.received()
}

func initPerf(t *testing.T, srv *perfServer) {
	t.Helper()
	resetActive(t)
	if err := Init(Options{DSN: testSockeyeDSN, HTTPClient: rewriteTransport(srv.URL)}); err != nil {
		t.Fatal(err)
	}
}

// TestRecordRequestBatches: fifty records post at once, to the perf
// path of the DSN's service, with the DSN's key. A smaller batch posts
// at the tick, and a tick with no records posts an empty batch, so
// sockeye reads the minute as quiet, not missing.
func TestRecordRequestBatches(t *testing.T) {
	is := is.New(t)
	srv := newPerfServer(t)
	initPerf(t, srv)
	defer Flush(time.Second)

	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for range perfBatchMax {
		RecordRequest(Request{Route: "/a", Status: 200, Duration: 12 * time.Millisecond, Time: at})
	}
	posts := srv.wait(t, 1)
	is.Eq(posts[0].path, "/perf/app/web")
	is.Eq(posts[0].auth, "Bearer sek_test")
	is.Eq(len(posts[0].records), perfBatchMax)
	is.Eq(posts[0].records[0]["duration_ms"], 12.0)
	is.Eq(posts[0].records[0]["time"], "2026-10-28T12:00:00Z")
	// A record whose service did not mark its database timing as
	// measured carries neither key, so sockeye reads it as untimed.
	_, hasDB := posts[0].records[0]["db_ms"]
	_, hasAcquire := posts[0].records[0]["acquire_ms"]
	is.True(!hasDB)
	is.True(!hasAcquire)

	// A heartbeat can land before the next record, so find the batch
	// that holds it, and an empty batch after it.
	RecordRequest(Request{Route: "/b", Status: 500, Time: at})
	deadline := time.Now().Add(5 * time.Second)
	for {
		posts = srv.received()
		b := slices.IndexFunc(posts, func(p perfPost) bool {
			return len(p.records) == 1 && p.records[0]["route"] == "/b"
		})
		if b > 0 && slices.ContainsFunc(posts[b+1:], func(p perfPost) bool { return p.body == "[]" }) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no batch with /b followed by a heartbeat in %d posts", len(posts))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRecordRequestDBTimed: a record marked DBTimed carries db_ms and
// acquire_ms, a 0 too, since a request with no queries did measure
// zero database time.
func TestRecordRequestDBTimed(t *testing.T) {
	is := is.New(t)
	srv := newPerfServer(t)
	initPerf(t, srv)

	RecordRequest(Request{Route: "/t", Status: 200, DB: 30 * time.Millisecond, Acquire: time.Millisecond, DBTimed: true})
	RecordRequest(Request{Route: "/z", Status: 200, DBTimed: true})
	Flush(time.Second)
	var recs []map[string]any
	for _, p := range srv.received() {
		recs = append(recs, p.records...)
	}
	is.Eq(len(recs), 2)
	is.Eq(recs[0]["db_ms"], 30.0)
	is.Eq(recs[0]["acquire_ms"], 1.0)
	is.Eq(recs[1]["db_ms"], 0.0)
	is.Eq(recs[1]["acquire_ms"], 0.0)
	// No Elapsed, no split keys.
	_, hasSplit := recs[0]["query_only_ms"]
	is.True(!hasSplit)
}

// TestRecordRequestElapsed: a record with Elapsed carries all three
// split keys, zeros too, so sockeye counts the split as measured.
func TestRecordRequestElapsed(t *testing.T) {
	is := is.New(t)
	srv := newPerfServer(t)
	initPerf(t, srv)

	RecordRequest(Request{Route: "/e", Status: 200, Duration: 20 * time.Millisecond,
		Elapsed: &DBElapsed{QueryOnly: 10 * time.Millisecond, WaitOnly: 2 * time.Millisecond}})
	Flush(time.Second)
	var recs []map[string]any
	for _, p := range srv.received() {
		recs = append(recs, p.records...)
	}
	is.Eq(len(recs), 1)
	is.Eq(recs[0]["query_only_ms"], 10.0)
	is.Eq(recs[0]["wait_only_ms"], 2.0)
	is.Eq(recs[0]["query_wait_ms"], 0.0)
}

// TestRecordRequestDetail: a record sends its user, method, and extra.
// A record without them sends none of the keys. An extra too large or
// with a key sockeye refuses becomes a small note, so one record does
// not fail its batch. A user and a method past their limits are cut.
func TestRecordRequestDetail(t *testing.T) {
	is := is.New(t)
	srv := newPerfServer(t)
	initPerf(t, srv)

	RecordRequest(Request{Route: "/s", Status: 200, User: "42", Method: "get",
		Extra: map[string]any{"q": "acme", "page": 2}})
	RecordRequest(Request{Route: "/p", Status: 200})
	RecordRequest(Request{Route: "/big", Status: 200, Extra: map[string]any{"q": strings.Repeat("x", perfExtraMax)}})
	RecordRequest(Request{Route: "/key", Status: 200, Extra: map[string]any{"Bad-Key": 1}})
	RecordRequest(Request{Route: "/long", Status: 200, User: strings.Repeat("u", 100), Method: "PROPFINDPROPFINDX"})
	Flush(time.Second)
	var recs []map[string]any
	for _, p := range srv.received() {
		recs = append(recs, p.records...)
	}
	is.Eq(len(recs), 5)
	is.Eq(recs[0]["user"], "42")
	is.Eq(recs[0]["method"], "GET")
	is.Eq(recs[0]["extra"], map[string]any{"q": "acme", "page": 2.0})
	for _, k := range []string{"user", "method", "extra"} {
		if _, ok := recs[1][k]; ok {
			t.Errorf("record with no %s sent %s", k, k)
		}
	}
	is.Eq(recs[2]["extra"], map[string]any{"truncated": fmt.Sprintf("extra was %d bytes; the limit is %d", perfExtraMax+8, perfExtraMax)})
	is.Eq(recs[3]["extra"], map[string]any{"invalid": "extra keys must match [a-z0-9_] and be at most 64 bytes"})
	is.Eq(len(recs[4]["user"].(string)), perfUserMax)
	_, hasMethod := recs[4]["method"]
	is.True(!hasMethod)
}

// TestRecordRequestCaptureUsage: with CaptureUsage, every batch, an
// empty one too, carries Perf-Capture: usage, so sockeye reads the
// minutes as usage-covered. Without it, no batch carries the header.
func TestRecordRequestCaptureUsage(t *testing.T) {
	is := is.New(t)
	srv := newPerfServer(t)
	resetActive(t)
	is.NoErr(Init(Options{DSN: testSockeyeDSN, HTTPClient: rewriteTransport(srv.URL), CaptureUsage: true}))
	RecordRequest(Request{Route: "/a", Status: 200, User: "1"})
	Flush(time.Second)
	posts := srv.received()
	is.True(len(posts) >= 1)
	for _, p := range posts {
		is.Eq(p.capture, "usage")
	}

	srv2 := newPerfServer(t)
	initPerf(t, srv2)
	RecordRequest(Request{Route: "/a", Status: 200})
	Flush(time.Second)
	for _, p := range srv2.received() {
		is.Eq(p.capture, "")
	}
}

// TestRecordRequestRetry: a 503 and then a 202 carry the same
// Idempotency-Key and the same bytes, so sockeye counts the batch once
// whichever try it took.
func TestRecordRequestRetry(t *testing.T) {
	is := is.New(t)
	srv := newPerfServer(t, 503)
	initPerf(t, srv)
	defer Flush(time.Second)

	RecordRequest(Request{Route: "/r", Status: 200})
	posts := srv.wait(t, 2)
	is.True(posts[0].key != "")
	is.Eq(posts[1].key, posts[0].key)
	is.Eq(posts[1].body, posts[0].body)
	is.Eq(DroppedRequests(), int64(0))
}

// TestRecordRequestConflict: a 409 says another producer counts the
// service's requests, so the client discards the batch without a
// retry and without counting it as dropped.
func TestRecordRequestConflict(t *testing.T) {
	is := is.New(t)
	srv := newPerfServer(t, 409)
	initPerf(t, srv)
	defer Flush(time.Second)

	RecordRequest(Request{Route: "/c", Status: 200})
	srv.wait(t, 2)
	posts := srv.received()
	is.True(posts[1].key != posts[0].key)
	is.Eq(posts[1].dropped, "")
	is.Eq(DroppedRequests(), int64(0))
}

// TestRecordRequestDrops: a full queue drops records and counts them,
// and the next batch sockeye accepts reports the count.
func TestRecordRequestDrops(t *testing.T) {
	is := is.New(t)
	srv := newPerfServer(t)
	srv.gate = make(chan struct{})
	initPerf(t, srv)
	defer Flush(time.Second)

	// The first batch holds the sender at the server, so the queue
	// fills behind it.
	for range perfBatchMax {
		RecordRequest(Request{Route: "/d", Status: 200})
	}
	srv.wait(t, 1)
	for range perfQueue + 3 {
		RecordRequest(Request{Route: "/d", Status: 200})
	}
	is.Eq(DroppedRequests(), int64(3))
	srv.mu.Lock()
	gate := srv.gate
	srv.gate = nil
	srv.mu.Unlock()
	close(gate)

	posts := srv.wait(t, 2)
	is.Eq(posts[1].dropped, "3")
}

// TestFlushSendsRequests: Flush sends the records still queued, and a
// route arrives without its query string.
func TestFlushSendsRequests(t *testing.T) {
	is := is.New(t)
	srv := newPerfServer(t)
	initPerf(t, srv)

	RecordRequest(Request{Route: "/companies/show?id=42", Status: 200})
	RecordRequest(Request{Route: "", Status: 200})
	Flush(time.Second)

	posts := srv.received()
	is.Eq(len(posts), 1)
	is.Eq(posts[0].records[0]["route"], "/companies/show")
	is.Eq(posts[0].records[1]["route"], "/")
}

// TestRecordRequestOff: with no DSN, RecordRequest does nothing.
func TestRecordRequestOff(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	RecordRequest(Request{Route: "/x", Status: 200})
	is.Eq(DroppedRequests(), int64(0))
}

func TestMatchedPatternPreserved(t *testing.T) {
	c := &client{}
	s := &perfSender{c: c, queue: make(chan perfRecord, 1)}
	const pattern = "GET example.com/b/{bucket}/o/{name...}"
	s.add(Request{Route: "/b/photos/o/a/b?secret=removed", Pattern: pattern, Status: 200, Time: time.Now()})
	r := <-s.queue
	if r.Pattern != pattern || r.Route != "/b/photos/o/a/b" {
		t.Fatalf("pattern/path changed: %#v", r)
	}
}

// TestRecordRequestExtraEncoding: a perf record with invalid UTF-8
// still sends, with U+FFFD in place of each bad byte, and a
// time.Duration in Extra goes as integer nanoseconds. An Extra that
// JSON cannot encode becomes an invalid note with no error text.
func TestRecordRequestExtraEncoding(t *testing.T) {
	is := is.New(t)
	srv := newPerfServer(t)
	initPerf(t, srv)

	RecordRequest(Request{Route: "/u\xff", Status: 200, User: "u\xff",
		Extra: map[string]any{"q": "a\xffb", "took": 2 * time.Second}})
	RecordRequest(Request{Route: "/c", Status: 200, Extra: map[string]any{"ch": make(chan int)}})
	Flush(time.Second)
	var recs []map[string]any
	for _, p := range srv.received() {
		recs = append(recs, p.records...)
	}
	is.Eq(len(recs), 2)
	is.Eq(recs[0]["route"], "/u\uFFFD")
	is.Eq(recs[0]["user"], "u\uFFFD")
	is.Eq(recs[0]["extra"], map[string]any{"q": "a\uFFFDb", "took": 2e9})
	is.Eq(recs[1]["extra"], map[string]any{"invalid": "extra does not encode as JSON"})
}

// TestRecordRequestLongFields: a 1 MB Route and a 1 MB Pattern give a
// record under 1 KiB, and its batch posts with the other records. A
// long route goes as /{long}, the label sockeye gives it, and a long
// pattern goes cut to perfPatternMax bytes.
func TestRecordRequestLongFields(t *testing.T) {
	is := is.New(t)
	srv := newPerfServer(t)
	initPerf(t, srv)

	big := "/" + strings.Repeat("a", 1<<20)
	RecordRequest(Request{Route: "/before", Status: 200})
	RecordRequest(Request{Route: big + "?q=1", Pattern: "GET " + big, Status: 200})
	RecordRequest(Request{Route: "/" + strings.Repeat("b", perfRouteMax-1), Status: 200})
	RecordRequest(Request{Route: "/after", Status: 200})
	Flush(time.Second)

	posts := srv.received()
	is.Eq(len(posts), 1)
	is.True(len(posts[0].body) < 2<<10)
	recs := posts[0].records
	is.Eq(len(recs), 4)
	is.Eq(recs[0]["route"], "/before")
	is.Eq(recs[1]["route"], "/{long}")
	is.Eq(recs[1]["pattern"], "GET "+big[:perfPatternMax-4])
	is.Eq(len(recs[2]["route"].(string)), perfRouteMax)
	is.Eq(recs[3]["route"], "/after")
}

// TestPerfRecordCopiesCutFields: a queued record does not hold the
// caller's large string, so 5,000 queued records hold only the bytes
// they send.
func TestPerfRecordCopiesCutFields(t *testing.T) {
	is := is.New(t)
	s := &perfSender{c: &client{}, queue: make(chan perfRecord, 1)}
	big := "/" + strings.Repeat("a", 1<<20)
	route := "/r?" + big
	s.add(Request{Route: route, Pattern: big, User: big, Status: 200, Time: time.Now()})
	r := <-s.queue
	is.Eq(r.Route, "/r")
	is.True(!shares(r.Route, route))
	is.True(!shares(r.Pattern, big))
	is.True(!shares(r.User, big))
}

// TestPerfRecordCopiesShortFields: a field within its limit that is a
// substring of a long request line, as r.Method and r.URL.Path are, does
// not keep that line in memory while the record waits in the queue.
func TestPerfRecordCopiesShortFields(t *testing.T) {
	is := is.New(t)
	s := &perfSender{c: &client{}, queue: make(chan perfRecord, 1)}
	line := "GET /companies/show?q=" + strings.Repeat("a", 1<<20) + " HTTP/1.1 PATCH 1742 GET /companies/{id}"
	sub := func(s string) string {
		i := strings.Index(line, s)
		return line[i : i+len(s)]
	}
	for _, method := range []string{sub("GET"), sub("PATCH")} {
		s.add(Request{Route: sub("/companies/show"), Pattern: sub("GET /companies/{id}"),
			User: sub("1742"), Method: method, Status: 200, Time: time.Now()})
		r := <-s.queue
		is.Eq(r.Route, "/companies/show")
		is.Eq(r.Pattern, "GET /companies/{id}")
		is.Eq(r.User, "1742")
		is.Eq(r.Method, method)
		for _, f := range []string{r.Route, r.Pattern, r.User, r.Method} {
			is.True(!shares(f, line))
		}
	}
}

// TestPerfSendClearsBatch: after the sender sends or drops a batch, its
// batch buffer holds no record, so a sent record can be freed.
func TestPerfSendClearsBatch(t *testing.T) {
	is := is.New(t)
	s := &perfSender{c: &client{}, openUntil: time.Now().Add(time.Hour)} // drop, do not post
	batch := make([]perfRecord, 0, perfBatchMax)
	for range perfBatchMax {
		batch = append(batch, perfRecord{Route: "/r", Extra: jsontext.Value(`{"q":1}`)})
	}
	batch = s.sendBatch(batch)
	is.Eq(len(batch), 0)
	for _, r := range batch[:cap(batch)] {
		is.Eq(r.Route, "")
		is.Nil(r.Extra)
	}
	is.Eq(s.dropped.Load(), int64(perfBatchMax))
}

// shares reports whether part points into whole's bytes.
func shares(part, whole string) bool {
	if part == "" {
		return false
	}
	p := uintptr(unsafe.Pointer(unsafe.StringData(part)))
	w := uintptr(unsafe.Pointer(unsafe.StringData(whole)))
	return p >= w && p < w+uintptr(len(whole))
}

// TestPerfExtraNoteSize: the note for a bad key or an encode error is
// at most perfExtraMax bytes, for a long key printable or not, and for
// a long encode error.
func TestPerfExtraNoteSize(t *testing.T) {
	is := is.New(t)
	long := strings.Repeat("K", 8<<10)
	for _, extra := range []map[string]any{
		{long: 1},
		{strings.Repeat("\x01", 8<<10): 1},
		{"q": map[string]any{long: make(chan int)}},
	} {
		b := perfExtra(extra)
		is.True(len(b) <= perfExtraMax)
		var note map[string]string
		is.NoErr(json.Unmarshal(b, &note))
		is.True(note["invalid"] != "")
	}
}

// BenchmarkRecordRequest measures RecordRequest on the caller
// goroutine, for a plain record, a record with a user, a method, and
// an extra, and a record whose route has a query string.
func BenchmarkRecordRequest(b *testing.B) {
	cases := []struct {
		name string
		r    Request
	}{
		{"plain", Request{Route: "/companies/show", Pattern: "GET /companies/{id}", Status: 200,
			Duration: 42 * time.Millisecond, DB: 12 * time.Millisecond, Acquire: time.Millisecond, DBTimed: true}},
		{"detail", Request{Route: "/search", Status: 200, Duration: 80 * time.Millisecond,
			User: "1742", Method: "get", Extra: map[string]any{"q": "acme", "sort": "name", "page": 2},
			Elapsed: &DBElapsed{QueryOnly: 30 * time.Millisecond, WaitOnly: time.Millisecond}}},
		{"query", Request{Route: "/companies/show?id=42&tab=positions", Status: 200, Duration: 42 * time.Millisecond}},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			benchInit(b)
			RecordRequest(c.r) // start the sender and its queue
			b.ReportAllocs()
			for b.Loop() {
				RecordRequest(c.r)
			}
		})
	}
}

// BenchmarkPerfExtra measures the encode of a Request Extra, valid and
// with a key that sockeye refuses.
func BenchmarkPerfExtra(b *testing.B) {
	valid := map[string]any{"q": "acme", "sort": "name", "page": 2, "tool_name": "search_companies"}
	bad := map[string]any{"q": "acme", "Bad-Key": 1}
	b.Run("valid", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			perfExtra(valid)
		}
	})
	b.Run("badkey", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			perfExtra(bad)
		}
	})
}

// BenchmarkEncodePerfBatch measures the JSON body of one full batch.
func BenchmarkEncodePerfBatch(b *testing.B) {
	db, acquire := 12.0, 1.0
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	batch := make([]perfRecord, perfBatchMax)
	for i := range batch {
		batch[i] = perfRecord{Route: "/companies/show", Pattern: "GET /companies/{id}", Status: 200,
			DurationMs: 42.5, DBMs: &db, AcquireMs: &acquire, Time: at, User: "1742", Method: "GET",
			Extra: perfExtra(map[string]any{"q": "acme", "page": 2})}
	}
	body, err := json.Marshal(batch, wireJSON)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		_, _ = json.Marshal(batch, wireJSON)
	}
}

// TestPerfExtraKeyMatchesRegexp: perfExtraKey takes the keys that the
// regexp ^[a-z0-9_]{1,64}$ takes, and no others.
func TestPerfExtraKeyMatchesRegexp(t *testing.T) {
	is := is.New(t)
	re := regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	for _, k := range []string{
		"", "q", "tool_name", "page2", "Q", "bad-key", "a b", "é",
		strings.Repeat("a", 64), strings.Repeat("a", 65), "a\n", "\x00",
	} {
		is.Eq(perfExtraKey(k), re.MatchString(k))
	}
}
