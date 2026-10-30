package errs

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	sdkName         = "croaky.errs"
	sdkVersion      = "0.5.0"
	userAgent       = "croaky-errs-go/0.5.0"
	maxMessageBytes = 8192
	queueLimit      = 1000
)

var defaultRetryDelays = []time.Duration{
	1 * time.Second,
	3 * time.Second,
	10 * time.Second,
	0,
}

var retryDelays = defaultRetryDelays

// zeroRetryDelays clears retry delays for tests.
func zeroRetryDelays() {
	retryDelays = make([]time.Duration, len(defaultRetryDelays))
}

// Options configures the client.
//
// DSN is a sockeye DSN, the output of `soc error dsn create`.
// SockeyeDSN is an alias for DSN kept for backwards compatibility.
// With neither set, error reporting is off and Init returns no error.
type Options struct {
	DSN          string
	SockeyeDSN   string
	Environment  string
	Release      string
	ServerName   string
	HTTPClient   *http.Client
	SyncDispatch bool
	InAppPrefix  string
	Clock        func() time.Time
	// CaptureUsage says that the service sets Request.User on the
	// requests it counts, such as every signed-in request. Each perf
	// batch then carries the header Perf-Capture: usage, and sockeye
	// reads the time as covered for usage, not as no data.
	CaptureUsage bool
}

// Option modifies an outgoing event.
type Option func(*event)

// WithTag attaches a key-value tag to the event.
func WithTag(key, value string) Option {
	return func(e *event) {
		if e.Tags == nil {
			e.Tags = map[string]string{}
		}
		e.Tags[key] = value
	}
}

// WithExtra attaches structured metadata to the event.
func WithExtra(key string, value any) Option {
	return func(e *event) {
		if e.Extra == nil {
			e.Extra = map[string]any{}
		}
		e.Extra[key] = value
	}
}

// WithFingerprint overrides event grouping.
func WithFingerprint(parts ...string) Option {
	return func(e *event) { e.Fingerprint = parts }
}

// WithRequestID tags the event with the request that caused it.
// sockeye links the event to the log lines of that request.
func WithRequestID(id string) Option { return WithTag("request_id", id) }

// WithLevel sets the event level.
func WithLevel(level string) Option {
	return func(e *event) { e.Level = level }
}

type client struct {
	dsn          dsn
	environment  string
	release      string
	serverName   string
	root         string
	inAppPrefix  string
	clock        func() time.Time
	httpClient   *http.Client
	syncDispatch bool
	captureUsage bool

	mu     sync.Mutex
	queue  chan *event
	wg     sync.WaitGroup
	closed atomic.Bool

	// dropped counts the events the client discarded without
	// delivering them. The next event sent to sockeye reports it.
	dropped atomic.Int64

	breakerMu sync.Mutex
	buckets   map[string]*bucket
	failures  int
	openUntil time.Time

	// perf sends request records. It starts on the first
	// RecordRequest. See perf.go.
	perfOnce sync.Once
	perf     atomic.Pointer[perfSender]
}

func (c *client) now() time.Time {
	if c.clock != nil {
		return c.clock()
	}
	return time.Now()
}

var active atomic.Pointer[client]

// Init initializes the global error reporting client.
// With no DSN and no SockeyeDSN, reporting is off and Init returns nil.
func Init(opts Options) error {
	rawDSN := opts.DSN
	if rawDSN == "" {
		rawDSN = opts.SockeyeDSN
	}
	if rawDSN == "" {
		active.Store(nil)
		return nil
	}
	d, err := parseDSN(rawDSN)
	if err != nil {
		return err
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	serverName := opts.ServerName
	if serverName == "" {
		if h, err := os.Hostname(); err == nil {
			serverName = h
		}
	}
	root := ""
	if wd, err := os.Getwd(); err == nil {
		root = wd
	}
	inAppPrefix := strings.TrimSpace(opts.InAppPrefix)
	if inAppPrefix == "" {
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Path != "" {
			inAppPrefix = info.Main.Path
		}
	}
	c := &client{
		dsn:          d,
		environment:  opts.Environment,
		release:      opts.Release,
		serverName:   serverName,
		root:         root,
		inAppPrefix:  inAppPrefix,
		clock:        opts.Clock,
		httpClient:   hc,
		syncDispatch: opts.SyncDispatch,
		captureUsage: opts.CaptureUsage,
		queue:        make(chan *event, queueLimit),
		buckets:      map[string]*bucket{},
	}
	if !c.syncDispatch {
		c.wg.Add(1)
		go c.worker()
	}
	active.Store(c)
	return nil
}

// Flush waits for queued events and request records to send, up to
// the timeout.
func Flush(timeout time.Duration) {
	c := active.Load()
	if c == nil {
		return
	}
	c.flush(timeout)
}

// CaptureException reports an error with stack trace frames.
func CaptureException(err error, opts ...Option) string {
	if err == nil {
		return ""
	}
	c := active.Load()
	if c == nil {
		return ""
	}
	fallback := callers(2)
	e := c.newEvent("error", opts)
	e.Exception = exceptionFromError(err, fallback, c.root, c.inAppPrefix)
	c.dispatch(e)
	return e.EventID
}

// CaptureMessage reports a message event.
func CaptureMessage(msg string, opts ...Option) string {
	c := active.Load()
	if c == nil {
		return ""
	}
	e := c.newEvent("info", opts)
	e.Message = truncate(msg)
	c.dispatch(e)
	return e.EventID
}

func (c *client) dispatch(e *event) {
	if c.syncDispatch {
		c.send(e)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return
	}
	c.wg.Add(1)
	select {
	case c.queue <- e:
	default:
		c.wg.Done()
		c.dropped.Add(1)
	}
}

func (c *client) worker() {
	defer c.wg.Done()
	t := time.NewTicker(countEvery)
	defer t.Stop()
	for {
		select {
		case e, ok := <-c.queue:
			if !ok {
				c.flushCounts(true)
				return
			}
			c.send(e)
			c.wg.Done()
		case <-t.C:
			c.flushCounts(false)
		}
	}
}

func (c *client) flush(timeout time.Duration) {
	if c.closed.Swap(true) {
		return
	}
	if c.syncDispatch {
		c.flushCounts(true)
	} else {
		c.mu.Lock()
		close(c.queue)
		c.mu.Unlock()
	}
	perf := c.perf.Load()
	if perf != nil {
		perf.close()
	}
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		if perf != nil {
			<-perf.done
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func isTransient(code int) bool {
	switch code {
	case 429, 500, 502, 503, 504:
		return true
	default:
		return false
	}
}

// deliver sends one request with the retry loop and reports whether
// the server accepted it. build makes a fresh request for each try.
// name starts each log line.
func (c *client) deliver(name string, build func() (*http.Request, error)) bool {
	delays := retryDelays
	var (
		lastStatus int
		lastErr    error
	)
	for i, delay := range delays {
		req, err := build()
		if err != nil {
			log.Printf("%s: build request: %v", name, err)
			return false
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			if i == len(delays)-1 {
				break
			}
			if delay > 0 {
				time.Sleep(delay)
			}
			continue
		}

		_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		lastStatus = resp.StatusCode

		if readErr != nil {
			lastErr = readErr
			if i == len(delays)-1 {
				break
			}
			if delay > 0 {
				time.Sleep(delay)
			}
			continue
		}

		if isTransient(resp.StatusCode) {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			if i == len(delays)-1 {
				break
			}
			d := delay
			if ra := parseRetryAfter(resp.Header.Get("Retry-After")); ra > 0 {
				d = max(delay, ra)
			}
			if d > 0 {
				time.Sleep(d)
			}
			continue
		}

		if resp.StatusCode/100 != 2 {
			log.Printf("%s: send: HTTP %d (non-retriable)", name, resp.StatusCode)
			return false
		}
		return true
	}

	if lastStatus != 0 {
		log.Printf("%s: send: HTTP %d (exhausted)", name, lastStatus)
	} else if lastErr != nil {
		log.Printf("%s: send (exhausted): %v", name, lastErr)
	} else {
		log.Printf("%s: send: exhausted retries", name)
	}
	return false
}

func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	secs, err := strconv.Atoi(h)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

func newEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// buildRevision returns the VCS commit that the Go build info records,
// or an empty string when the build carries none.
func buildRevision() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return ""
}
