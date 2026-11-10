package errs

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"net/http"
	"strconv"
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

// wireJSON holds the encode options for each body errs posts. An
// encode that fails drops the record, so errs keeps two v1 behaviors
// that v2 makes strict. A string with invalid UTF-8, such as command
// output in an error message, goes out with U+FFFD in place of each
// bad byte. A time.Duration, which v2 refuses with no format tag, goes
// out as integer nanoseconds.
var wireJSON = json.JoinOptions(
	jsontext.AllowInvalidUTF8(true),
	json.WithMarshalers(json.MarshalToFunc(func(enc *jsontext.Encoder, d time.Duration) error {
		return enc.WriteToken(jsontext.Int(int64(d)))
	})),
)

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
		count, bk.held, bk.lastCount, bk.last = bk.held, 0, now, nil
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
			bk.held, bk.lastCount, bk.last = 0, now, nil
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
			e.Extra = map[string]jsontext.Value{}
		}
		e.Extra["dropped"] = jsontext.Value(strconv.FormatInt(dropped, 10))
	}
	body, err := json.Marshal(e, wireJSON)
	if err != nil {
		c.dropped.Add(dropped + int64(lost))
		return
	}

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

// fingerprintKey names the error an event belongs to, for sampling.
// It follows the order sockeye groups by: the event's own
// fingerprint, else the type and the top three in-app frames, else the
// type and the message with its digits replaced. The worker calls it
// for each event, so it builds the key in one strings.Builder.
func fingerprintKey(e *event) string {
	var b strings.Builder
	if len(e.Fingerprint) > 0 {
		n := len("own:") + len(e.Fingerprint) - 1
		for _, p := range e.Fingerprint {
			n += len(p)
		}
		b.Grow(n)
		b.WriteString("own:")
		for i, p := range e.Fingerprint {
			if i > 0 {
				b.WriteByte(0)
			}
			b.WriteString(p)
		}
		return b.String()
	}
	if e.Exception != nil && len(e.Exception.Values) > 0 {
		v := e.Exception.Values[len(e.Exception.Values)-1]
		var (
			top  [3]*frame
			nTop int
		)
		n := len("frames:") + len(v.Type)
		if v.Stacktrace != nil {
			frames := v.Stacktrace.Frames
			for i := len(frames) - 1; i >= 0 && nTop < len(top); i-- {
				if f := &frames[i]; f.InApp {
					top[nTop] = f
					nTop++
					n += 2 + len(f.Function) + len(f.Filename)
				}
			}
		}
		if nTop > 0 {
			b.Grow(n)
			b.WriteString("frames:")
			b.WriteString(v.Type)
			for _, f := range top[:nTop] {
				b.WriteByte(0)
				b.WriteString(f.Function)
				b.WriteByte('@')
				b.WriteString(f.Filename)
			}
			return b.String()
		}
		b.Grow(len("type:") + len(v.Type) + 1 + len(v.Value))
		b.WriteString("type:")
		b.WriteString(v.Type)
		b.WriteByte(0)
		writeNoDigits(&b, v.Value)
		return b.String()
	}
	if e.Message != "" {
		b.Grow(len("message:") + len(e.Message))
		b.WriteString("message:")
		writeNoDigits(&b, e.Message)
		return b.String()
	}
	return ""
}

// writeNoDigits writes s to b with each run of ASCII digits replaced
// by one "?". It gives the result of the regexp [0-9]+ with
// ReplaceAllString, at a small part of the cost. Other bytes, invalid
// UTF-8 too, go through unchanged.
func writeNoDigits(b *strings.Builder, s string) {
	for i := 0; i < len(s); {
		if !isDigit(s[i]) {
			j := i + 1
			for j < len(s) && !isDigit(s[j]) {
				j++
			}
			b.WriteString(s[i:j])
			i = j
			continue
		}
		b.WriteByte('?')
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	}
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }
