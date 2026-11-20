package errs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json/jsontext"
	"fmt"
	"hash/maphash"
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
	// maxRetryAfter caps the wait a server asks for in Retry-After.
	// It equals the largest step of defaultRetryDelays. A server that
	// needs a longer pause gets one from the circuit breaker.
	maxRetryAfter = 10 * time.Second
)

// logger writes the client's own diagnostics to stderr. It is not the
// global log logger, because a host can bridge that logger into
// SlogHandler, and a failed send would then become a new event.
var logger = log.New(os.Stderr, "", log.LstdFlags)

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
// With neither a DSN nor Journal set, error reporting is off.
type Options struct {
	DSN         string
	SockeyeDSN  string
	Environment string
	Release     string
	ServerName  string
	// HTTPClient supplies the HTTP client for sockeye requests. Init copies
	// the client value and overrides CheckRedirect so errs does not follow
	// redirects. A caller's CheckRedirect does not apply, because a redirect
	// can send the DSN key to another host. Init does not modify the caller's
	// client value.
	HTTPClient  *http.Client
	InAppPrefix string
	Clock       func() time.Time
	// Journal writes versioned, single-line error events to this writer.
	// It is exclusive with DSN/SockeyeDSN and sends no request metrics.
	// The caller owns the writer and its trusted host collection binding.
	Journal io.Writer
	// CaptureUsage says that the service sets Request.User on the
	// requests it counts, such as every signed-in request. Each perf
	// batch then carries the header Perf-Capture: usage, and sockeye
	// reads the time as covered for usage, not as no data.
	CaptureUsage bool
}

// Option modifies an outgoing event.
type Option func(*event)

// WithTag attaches a key-value tag to the event. An event keeps its
// first maxTags tags. The client cuts a key past maxKeyBytes and a
// value past maxTagBytes. It replaces each run of invalid UTF-8 in a
// key with U+FFFD, so keys that differ only in invalid bytes are one
// tag, and the last one wins.
func WithTag(key, value string) Option {
	return func(e *event) {
		k := eventKey(key)
		if _, ok := e.Tags[k]; !ok && len(e.Tags) >= maxTags {
			return
		}
		if e.Tags == nil {
			e.Tags = map[string]string{}
		}
		e.Tags[k] = cut(value, maxTagBytes)
	}
}

// WithExtra attaches structured metadata to the event. The capture
// call encodes value as JSON before it returns, so the caller can
// change value afterward. An event keeps its first maxExtras extras.
// A key follows the rules of WithTag.
// See encodeExtra for a value that is too large or does not encode.
func WithExtra(key string, value any) Option {
	return func(e *event) {
		k := eventKey(key)
		if _, ok := e.Extra[k]; !ok && len(e.Extra) >= maxExtras {
			return
		}
		if e.Extra == nil {
			e.Extra = map[string]jsontext.Value{}
		}
		e.Extra[k] = encodeExtra(value)
	}
}

// WithFingerprint overrides event grouping. The event keeps a copy of
// the first maxFingerprintParts parts, each cut to maxFingerprintBytes.
func WithFingerprint(parts ...string) Option {
	return func(e *event) {
		n := min(len(parts), maxFingerprintParts)
		e.Fingerprint = make([]string, n)
		for i, p := range parts[:n] {
			e.Fingerprint[i] = cut(p, maxFingerprintBytes)
		}
	}
}

// WithRequestID tags the event with the request that caused it.
// sockeye links the event to the log lines of that request.
func WithRequestID(id string) Option { return WithTag("request_id", id) }

// WithLevel sets the event level, cut to maxLevelBytes.
func WithLevel(level string) Option {
	return func(e *event) { e.Level = cut(level, maxLevelBytes) }
}

type client struct {
	dsn          dsn
	journal      io.Writer
	environment  string
	release      string
	serverName   string
	root         string
	inAppPrefix  string
	clock        func() time.Time
	httpClient   *http.Client
	captureUsage bool

	mu     sync.Mutex
	queue  chan *event
	wg     sync.WaitGroup
	closed atomic.Bool

	// dropped counts the events the client discarded without
	// delivering them. The next event sent to sockeye reports it.
	dropped atomic.Int64

	breakerMu sync.Mutex
	buckets   map[uint64]*bucket
	// heldEvents counts the buckets that keep a held event. It stays at
	// maxHeldEvents or fewer.
	heldEvents int
	// seed keys the hash of fingerprintKey. The worker makes it with
	// buckets, on the first send.
	seed      maphash.Seed
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
// With no DSN, SockeyeDSN or Journal, reporting is off and Init returns nil.
func Init(opts Options) error {
	rawDSN := opts.DSN
	if rawDSN == "" {
		rawDSN = opts.SockeyeDSN
	}
	if rawDSN != "" && opts.Journal != nil {
		return fmt.Errorf("choose Journal or an HTTP DSN, not both")
	}
	if rawDSN == "" && opts.Journal == nil {
		active.Store(nil)
		return nil
	}
	var d dsn
	if rawDSN != "" {
		var err error
		d, err = parseDSN(rawDSN)
		if err != nil {
			return err
		}
	}
	var hc http.Client
	if opts.HTTPClient != nil {
		hc = *opts.HTTPClient
	} else {
		hc = http.Client{Timeout: 5 * time.Second}
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
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
		journal:      opts.Journal,
		environment:  opts.Environment,
		release:      opts.Release,
		serverName:   serverName,
		root:         root,
		inAppPrefix:  inAppPrefix,
		clock:        opts.Clock,
		httpClient:   &hc,
		captureUsage: opts.CaptureUsage,
		queue:        make(chan *event, queueLimit),
	}
	c.wg.Add(1)
	go c.worker()
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
	// Read the caller stack only when err carries none, since
	// runtime.Callers costs more than the rest of the capture.
	pcs := stackFromError(err)
	if pcs == nil {
		pcs = callers(2)
	}
	e := c.newEvent("error", opts)
	e.Exception = exceptionFromError(err, pcs, c.root, c.inAppPrefix)
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
	c.mu.Lock()
	close(c.queue)
	c.mu.Unlock()
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
			logger.Printf("%s: build request: %v", name, err)
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
			logger.Printf("%s: send: HTTP %d (non-retriable)", name, resp.StatusCode)
			return false
		}
		return true
	}

	if lastStatus != 0 {
		logger.Printf("%s: send: HTTP %d (exhausted)", name, lastStatus)
	} else if lastErr != nil {
		logger.Printf("%s: send (exhausted): %v", name, lastErr)
	} else {
		logger.Printf("%s: send: exhausted retries", name)
	}
	return false
}

// parseRetryAfter reads a Retry-After header in seconds, up to
// maxRetryAfter. It compares the seconds before it multiplies, so no
// value overflows time.Duration.
func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	secs, err := strconv.Atoi(h)
	if err != nil || secs <= 0 {
		return 0
	}
	if secs >= int(maxRetryAfter/time.Second) {
		return maxRetryAfter
	}
	return time.Duration(secs) * time.Second
}

func newEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000000000000000000000000000"
	}
	var h [32]byte
	hex.Encode(h[:], b[:])
	return string(h[:])
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
