package errs

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// sampleWindow is the span in which one error sends fullPerWindow
	// events whole.
	sampleWindow  = time.Minute
	fullPerWindow = 10
	// countEvery is the pause between the count events of one error
	// after it passes fullPerWindow.
	countEvery = 10 * time.Second
	// breakerFailures failed sends in a row open the breaker. It
	// stays open for breakerPause.
	breakerFailures = 5
	breakerPause    = 30 * time.Second
)

var parseSockeyeDSN = parseDSN

// sockeyeEvent is an event as sockeye reads it. sockeye takes the
// message as a string and the number of events one event stands for as
// count. The outer Message field hides the embedded one in the JSON.
type sockeyeEvent struct {
	event
	Message string `json:"message,omitempty"`
	Count   int    `json:"count,omitempty"`
}

// sockeyeBackend sends events to sockeye. It samples by error, and it
// stops sending while sockeye is down.
type sockeyeBackend struct {
	dsn dsn

	mu        sync.Mutex
	buckets   map[string]*bucket
	failures  int
	openUntil time.Time
}

// bucket tracks one error: the events it sent whole in this window,
// and the events it held back since its last count.
type bucket struct {
	windowStart time.Time
	sent        int
	held        int
	lastCount   time.Time
	last        *event
}

func newSockeyeBackend(d dsn) *sockeyeBackend {
	return &sockeyeBackend{dsn: d, buckets: map[string]*bucket{}}
}

// send sends an event whole while its error is under the limit. Past
// the limit, it holds the event and sends one count event every
// countEvery, so the group total stays exact.
func (b *sockeyeBackend) send(c *client, e *event) {
	now := c.now()
	key := fingerprintKey(e)

	b.mu.Lock()
	bk := b.buckets[key]
	if bk == nil {
		bk = &bucket{windowStart: now}
		b.buckets[key] = bk
	}
	if now.Sub(bk.windowStart) >= sampleWindow {
		bk.windowStart, bk.sent = now, 0
	}
	count := 0
	if bk.sent < fullPerWindow {
		bk.sent++
	} else {
		bk.held++
		bk.last = e
		if now.Sub(bk.lastCount) < countEvery {
			b.mu.Unlock()
			return
		}
		count, bk.held, bk.lastCount = bk.held, 0, now
	}
	b.mu.Unlock()

	b.post(c, e, count)
}

// flushCounts sends the held counts that are due. With all set, it
// sends every held count, for shutdown. It also forgets errors that
// have been quiet for a window.
func (b *sockeyeBackend) flushCounts(c *client, all bool) {
	now := c.now()
	type pending struct {
		e     *event
		count int
	}
	var due []pending

	b.mu.Lock()
	for key, bk := range b.buckets {
		if bk.held > 0 && (all || now.Sub(bk.lastCount) >= countEvery) {
			due = append(due, pending{bk.last, bk.held})
			bk.held, bk.lastCount = 0, now
		}
		if bk.held == 0 && now.Sub(bk.windowStart) >= sampleWindow && now.Sub(bk.lastCount) >= sampleWindow {
			delete(b.buckets, key)
		}
	}
	b.mu.Unlock()

	for _, p := range due {
		b.post(c, p.e, p.count)
	}
}

// post sends one event. A breaker that is open drops it. The event
// carries the number of events dropped since the last send.
func (b *sockeyeBackend) post(c *client, e *event, count int) {
	lost := max(count, 1)
	now := c.now()
	if !b.allow(now) {
		c.dropped.Add(int64(lost))
		return
	}

	se := sockeyeEvent{event: *e, Count: count}
	if e.Message != nil {
		se.Message = e.Message.Formatted
	}
	dropped := c.dropped.Swap(0)
	if dropped > 0 {
		se.Extra = maps.Clone(e.Extra)
		if se.Extra == nil {
			se.Extra = map[string]any{}
		}
		se.Extra["dropped"] = dropped
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(se); err != nil {
		c.dropped.Add(dropped + int64(lost))
		return
	}
	body := buf.Bytes()

	ok := c.deliver("sockeye", func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, b.dsn.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Authorization", "Bearer "+b.dsn.key)
		return req, nil
	})
	b.record(ok, c.now())
	if !ok {
		c.dropped.Add(dropped + int64(lost))
	}
}

// allow reports whether the breaker lets a send through. After the
// pause it lets one through, and the result of that send decides.
func (b *sockeyeBackend) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !now.Before(b.openUntil)
}

// record counts a send result. A success closes the breaker. The
// breakerFailures-th failure in a row opens it for breakerPause.
func (b *sockeyeBackend) record(ok bool, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ok {
		b.failures = 0
		return
	}
	b.failures++
	if b.failures >= breakerFailures {
		b.openUntil = now.Add(breakerPause)
	}
}

var digits = regexp.MustCompile(`[0-9]+`)

// fingerprintKey names the error an event belongs to, for sampling.
// It follows the order sockeye groups by: the event's own
// fingerprint, else the type and the top three in-app frames, else the
// type and the message with its digits replaced.
func fingerprintKey(e *event) string {
	if len(e.Fingerprint) > 0 {
		return "own:" + strings.Join(e.Fingerprint, "\x00")
	}
	if e.Exception != nil && len(e.Exception.Values) > 0 {
		v := e.Exception.Values[len(e.Exception.Values)-1]
		var top []string
		if v.Stacktrace != nil {
			frames := v.Stacktrace.Frames
			for i := len(frames) - 1; i >= 0 && len(top) < 3; i-- {
				if frames[i].InApp {
					top = append(top, frames[i].Function+"@"+frames[i].Filename)
				}
			}
		}
		if len(top) > 0 {
			return "frames:" + v.Type + "\x00" + strings.Join(top, "\x00")
		}
		return "type:" + v.Type + "\x00" + digits.ReplaceAllString(v.Value, "?")
	}
	if e.Message != nil {
		return "message:" + digits.ReplaceAllString(e.Message.Formatted, "?")
	}
	return ""
}
