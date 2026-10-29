package errs

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Request is one HTTP request a service served. RecordRequest sends it
// to sockeye's perf page.
type Request struct {
	// Route names the page, with no query string and no ids:
	// "/companies/show", not "/companies/show?id=42". RecordRequest
	// removes a query string in all cases.
	Route string
	// Pattern is the matched http.Request.Pattern, when the router supplies it.
	// It stays exact, including method, host and wildcard names.
	Pattern string
	Status  int
	// Duration is the whole request. DB is the time its queries took,
	// and Acquire the time they waited for a pool connection.
	Duration time.Duration
	DB       time.Duration
	Acquire  time.Duration
	// DBTimed says the service measured DB and Acquire, so a zero is a
	// request with no database time. When it is false, RecordRequest
	// sends neither, and sockeye shows the request as untimed rather
	// than as zero database work.
	DBTimed bool
	// Elapsed is the request's wall time split by what ran: only
	// queries, only pool waits, or both at once. The rest is time in
	// which neither ran. nil sends no split, and sockeye shows none.
	Elapsed *DBElapsed
	// Time is when the request happened. Zero means now.
	Time time.Time
}

// DBElapsed is a request's wall time in which only queries ran, only
// pool waits ran, or both ran at once. The three never overlap, so
// their sum is at most the request's Duration. DB and Acquire add up
// work, and can be more than Duration when queries run at the same
// time; these do not.
type DBElapsed struct {
	QueryOnly    time.Duration
	WaitOnly     time.Duration
	QueryAndWait time.Duration
}

const (
	// perfQueue is how many records wait to send. RecordRequest drops
	// a record past it, and counts it.
	perfQueue = 5000
	// perfBatchMax is the most records sockeye takes in one POST.
	perfBatchMax = 50
	// perfTries is how many times the sender sends one batch.
	perfTries = 3
	// perfMaxAge is how old a batch's oldest record may be at a retry.
	// sockeye refuses records older than one hour.
	perfMaxAge = 55 * time.Minute
)

// perfEvery is how often the sender posts what it has, and an empty
// batch when it has nothing, so sockeye reads a quiet minute as quiet
// and not as missing.
var perfEvery = 10 * time.Second

// perfRecord is one request as sockeye's POST /perf reads it.
type perfRecord struct {
	Route       string    `json:"route"`
	Pattern     string    `json:"pattern,omitempty"`
	Status      int       `json:"status"`
	DurationMs  float64   `json:"duration_ms"`
	DBMs        *float64  `json:"db_ms,omitempty"`
	AcquireMs   *float64  `json:"acquire_ms,omitempty"`
	QueryOnlyMs *float64  `json:"query_only_ms,omitempty"`
	WaitOnlyMs  *float64  `json:"wait_only_ms,omitempty"`
	QueryWaitMs *float64  `json:"query_wait_ms,omitempty"`
	Time        time.Time `json:"time"`
}

// perfSender sends request records to sockeye. It shares the DSN with
// error delivery and nothing else: its own queue, goroutine, and
// circuit breaker, so a slow perf endpoint does not stop errors, and
// the reverse.
type perfSender struct {
	c        *client
	endpoint string

	mu     sync.Mutex
	closed bool
	queue  chan perfRecord
	done   chan struct{}

	// dropped counts every record this process dropped. unsent counts
	// those dropped since sockeye last accepted a batch, which the next
	// batch reports in its Dropped-Records header.
	dropped atomic.Int64
	unsent  atomic.Int64

	breakerMu sync.Mutex
	failures  int
	openUntil time.Time
}

// RecordRequest queues one request for sockeye's perf page. It never
// blocks: a full queue drops the request and counts it. It does
// nothing when Init got no DSN. The sender starts on the first call,
// so a process that records no requests posts nothing.
func RecordRequest(r Request) {
	c := active.Load()
	if c == nil {
		return
	}
	if s := c.perfSender(); s != nil {
		s.add(r)
	}
}

// DroppedRequests returns how many request records this process
// dropped: a full queue, an open breaker, or a batch that failed
// after its tries.
func DroppedRequests() int64 {
	c := active.Load()
	if c == nil {
		return 0
	}
	if s := c.perf.Load(); s != nil {
		return s.dropped.Load()
	}
	return 0
}

// perfSender returns the client's sender, and starts it on first use.
// A client that Flush closed starts none.
func (c *client) perfSender() *perfSender {
	c.perfOnce.Do(func() {
		if c.closed.Load() {
			return
		}
		s := &perfSender{
			c: c,
			// The DSN names /errors/<repo>/<service>, and the same
			// key posts to /perf/<repo>/<service>.
			endpoint: strings.Replace(c.dsn.endpoint, "/errors/", "/perf/", 1),
			queue:    make(chan perfRecord, perfQueue),
			done:     make(chan struct{}),
		}
		c.perf.Store(s)
		go s.run()
	})
	return c.perf.Load()
}

func (s *perfSender) add(r Request) {
	route := r.Route
	if i := strings.IndexByte(route, '?'); i >= 0 {
		route = route[:i]
	}
	if route == "" {
		route = "/"
	}
	at := r.Time
	if at.IsZero() {
		at = s.c.now()
	}
	rec := perfRecord{
		Route:      route,
		Pattern:    r.Pattern,
		Status:     r.Status,
		DurationMs: ms(r.Duration),
		Time:       at.UTC(),
	}
	if r.DBTimed {
		db, acquire := ms(r.DB), ms(r.Acquire)
		rec.DBMs, rec.AcquireMs = &db, &acquire
	}
	if e := r.Elapsed; e != nil {
		q, w, b := ms(e.QueryOnly), ms(e.WaitOnly), ms(e.QueryAndWait)
		rec.QueryOnlyMs, rec.WaitOnlyMs, rec.QueryWaitMs = &q, &w, &b
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.queue <- rec:
	default:
		s.drop(1)
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func (s *perfSender) drop(n int64) {
	s.dropped.Add(n)
	s.unsent.Add(n)
}

// close stops the sender. run sends what is queued, and closes done.
func (s *perfSender) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.queue)
	}
}

// run posts a batch at perfBatchMax records, and every perfEvery
// whatever it has, an empty batch too.
func (s *perfSender) run() {
	defer close(s.done)
	t := time.NewTicker(perfEvery)
	defer t.Stop()
	batch := make([]perfRecord, 0, perfBatchMax)
	for {
		select {
		case r, ok := <-s.queue:
			if !ok {
				if len(batch) > 0 {
					s.send(batch)
				}
				return
			}
			batch = append(batch, r)
			if len(batch) == perfBatchMax {
				s.send(batch)
				batch = batch[:0]
			}
		case <-t.C:
			s.send(batch)
			batch = batch[:0]
		}
	}
}

// send posts one batch. It encodes the body once, with its drop count,
// and every try sends the same bytes under the same Idempotency-Key,
// so sockeye counts the batch once whichever try it took. A 409 says
// another producer counts the service's requests, so the batch is
// discarded and not counted as dropped. Any other failure drops the
// batch, and the next batch reports its records with the drops before
// it.
func (s *perfSender) send(batch []perfRecord) {
	if !s.allow() {
		s.drop(int64(len(batch)))
		return
	}
	unsent := s.unsent.Swap(0)
	body, err := json.Marshal(batch)
	if err != nil {
		s.unsent.Add(unsent)
		s.drop(int64(len(batch)))
		return
	}
	oldest := s.c.now()
	for _, r := range batch {
		if r.Time.Before(oldest) {
			oldest = r.Time
		}
	}
	status := s.post(body, newEventID(), unsent, oldest)
	switch {
	case status/100 == 2:
		s.record(true)
	case status == 409:
		s.record(true)
		s.unsent.Add(unsent)
	default:
		s.record(false)
		s.unsent.Add(unsent)
		s.drop(int64(len(batch)))
	}
}

// post sends body up to perfTries times and returns the last status,
// or 0 when no try got an answer. It retries a transport error and a
// transient status, after Retry-After when the server sends one, and
// stops when the oldest record nears the age sockeye refuses.
func (s *perfSender) post(body []byte, key string, unsent int64, oldest time.Time) int {
	status := 0
	var wait time.Duration
	for i := range perfTries {
		if i > 0 {
			if s.c.now().Sub(oldest) > perfMaxAge {
				break
			}
			if wait > 0 {
				time.Sleep(wait)
			}
		}
		wait = retryDelays[min(i, len(retryDelays)-1)]
		req, err := http.NewRequest(http.MethodPost, s.endpoint, bytes.NewReader(body))
		if err != nil {
			log.Printf("errs perf: build request: %v", err)
			return 0
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Authorization", "Bearer "+s.c.dsn.key)
		req.Header.Set("Idempotency-Key", key)
		if unsent > 0 {
			req.Header.Set("Dropped-Records", strconv.FormatInt(unsent, 10))
		}
		resp, err := s.c.httpClient.Do(req)
		if err != nil {
			status = 0
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		status = resp.StatusCode
		if !isTransient(status) {
			break
		}
		if ra := parseRetryAfter(resp.Header.Get("Retry-After")); ra > wait {
			wait = ra
		}
	}
	if status/100 != 2 && status != 409 {
		log.Printf("errs perf: send: HTTP %d after retries", status)
	}
	return status
}

// allow reports whether the breaker lets a batch through.
func (s *perfSender) allow() bool {
	s.breakerMu.Lock()
	defer s.breakerMu.Unlock()
	return !s.c.now().Before(s.openUntil)
}

// record counts a send result, on the schedule error delivery uses.
func (s *perfSender) record(ok bool) {
	s.breakerMu.Lock()
	defer s.breakerMu.Unlock()
	if ok {
		s.failures = 0
		return
	}
	s.failures++
	if s.failures >= breakerFailures {
		s.openUntil = s.c.now().Add(breakerPause(s.failures))
	}
}
