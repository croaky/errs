package errs

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"regexp"
	"strings"
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
	// breakerFailures failed sends in a row open the breaker.
	breakerFailures = 5
)

// breakerPauses is the schedule of pauses when consecutive sends fail.
// It steps through the table up to the final delay.
var breakerPauses = []time.Duration{
	30 * time.Second,
	1 * time.Minute,
	2 * time.Minute,
	5 * time.Minute,
	10 * time.Minute,
}

func breakerPause(failures int) time.Duration {
	idx := failures - breakerFailures
	if idx < 0 {
		return 0
	}
	if idx >= len(breakerPauses) {
		idx = len(breakerPauses) - 1
	}
	return breakerPauses[idx]
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

// send sends an event whole while its error is under the limit. Past
// the limit, it holds the event and sends one count event every
// countEvery, so the group total stays exact.
func (c *client) send(e *event) {
	now := c.now()
	key := fingerprintKey(e)

	c.breakerMu.Lock()
	bk := c.buckets[key]
	if bk == nil {
		bk = &bucket{windowStart: now}
		c.buckets[key] = bk
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
			c.breakerMu.Unlock()
			return
		}
		count, bk.held, bk.lastCount = bk.held, 0, now
	}
	c.breakerMu.Unlock()

	c.post(e, count)
}

// flushCounts sends the held counts that are due. With all set, it
// sends every held count, for shutdown. It also forgets errors that
// have been quiet for a window.
func (c *client) flushCounts(all bool) {
	now := c.now()
	type pending struct {
		e     *event
		count int
	}
	var due []pending

	c.breakerMu.Lock()
	for key, bk := range c.buckets {
		if bk.held > 0 && (all || now.Sub(bk.lastCount) >= countEvery) {
			due = append(due, pending{bk.last, bk.held})
			bk.held, bk.lastCount = 0, now
		}
		if bk.held == 0 && now.Sub(bk.windowStart) >= sampleWindow && now.Sub(bk.lastCount) >= sampleWindow {
			delete(c.buckets, key)
		}
	}
	c.breakerMu.Unlock()

	for _, p := range due {
		c.post(p.e, p.count)
	}
}

// post sends one event. A breaker that is open drops it. The event
// carries the number of events dropped since the last send.
func (c *client) post(e *event, count int) {
	lost := max(count, 1)
	now := c.now()
	if !c.allow(now) {
		c.dropped.Add(int64(lost))
		return
	}

	e.Count = count
	dropped := c.dropped.Swap(0)
	if dropped > 0 {
		e.Extra = maps.Clone(e.Extra)
		if e.Extra == nil {
			e.Extra = map[string]any{}
		}
		e.Extra["dropped"] = dropped
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		c.dropped.Add(dropped + int64(lost))
		return
	}
	body := buf.Bytes()

	ok := c.deliver("sockeye", func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, c.dsn.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Authorization", "Bearer "+c.dsn.key)
		return req, nil
	})
	c.record(ok, c.now())
	if !ok {
		c.dropped.Add(dropped + int64(lost))
	}
}

// allow reports whether the breaker lets a send through. After the
// pause it lets one through, and the result of that send decides.
func (c *client) allow(now time.Time) bool {
	c.breakerMu.Lock()
	defer c.breakerMu.Unlock()
	return !now.Before(c.openUntil)
}

// record counts a send result. A success closes the breaker. The
// breakerFailures-th failure in a row opens it, and consecutive
// failures step through the breakerPauses schedule.
func (c *client) record(ok bool, now time.Time) {
	c.breakerMu.Lock()
	defer c.breakerMu.Unlock()
	if ok {
		c.failures = 0
		return
	}
	c.failures++
	if c.failures >= breakerFailures {
		c.openUntil = now.Add(breakerPause(c.failures))
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
	if e.Message != "" {
		return "message:" + digits.ReplaceAllString(e.Message, "?")
	}
	return ""
}
