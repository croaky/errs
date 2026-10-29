package errs

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/croaky/is"
)

// perfPost is one request the perf test server received.
type perfPost struct {
	path, auth, key, dropped string
	body                     string
	records                  []map[string]any
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
			r.Header.Get("Idempotency-Key"), r.Header.Get("Dropped-Records"), string(body), recs})
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
