package errs

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"hash/maphash"
	"maps"
	"net/http"
	"strconv"
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
	// maxBuckets bounds the sampling map in items. A bucket is small and
	// its key is 8 bytes, so the map stays under about 100 KiB. The
	// overflow bucket counts toward it.
	maxBuckets = 1024
	// maxHeldEvents bounds the held events in bytes. An event is under
	// about 200 KiB, so at most 32 of them keep the worker under about
	// 6.4 MiB. A bucket with no free slot still counts its events.
	maxHeldEvents = 32
	// overflowKey names the one bucket that errors share once the map
	// holds maxBuckets entries. fingerprintKey never returns it.
	overflowKey = 0
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
// countEvery, so the group total stays exact. The worker is the only
// caller. Once the map holds maxBuckets entries, a new error goes to
// the overflow bucket, which samples and counts like any other bucket.
// A bucket holds its latest event only while fewer than maxHeldEvents
// buckets do. A bucket with no slot counts its events, and the next
// event of the bucket carries the count.
func (c *client) send(e *event) {
	now := c.now()
	if c.buckets == nil {
		c.buckets = map[uint64]*bucket{}
		c.seed = maphash.MakeSeed()
	}
	key := fingerprintKey(c.seed, e)

	c.breakerMu.Lock()
	bk := c.buckets[key]
	if bk == nil {
		if len(c.buckets) >= maxBuckets-1 {
			key = overflowKey
			bk = c.buckets[key]
		}
		if bk == nil {
			bk = &bucket{windowStart: now}
			c.buckets[key] = bk
		}
	}
	if now.Sub(bk.windowStart) >= sampleWindow {
		bk.windowStart, bk.sent = now, 0
	}
	count := 0
	if bk.sent < fullPerWindow {
		bk.sent++
	} else {
		bk.held++
		if now.Sub(bk.lastCount) < countEvery {
			c.hold(bk, e)
			c.breakerMu.Unlock()
			return
		}
		count, bk.held, bk.lastCount = bk.held, 0, now
		c.freeHeld(bk)
	}
	c.breakerMu.Unlock()

	c.post(e, count)
}

// hold keeps e as the latest held event of bk, if bk has one already
// or if fewer than maxHeldEvents buckets have one. The caller holds
// breakerMu.
func (c *client) hold(bk *bucket, e *event) {
	if bk.last == nil {
		if c.heldEvents >= maxHeldEvents {
			return
		}
		c.heldEvents++
	}
	bk.last = e
}

// freeHeld drops the held event of bk and frees its slot. The caller
// holds breakerMu.
func (c *client) freeHeld(bk *bucket) {
	if bk.last != nil {
		bk.last = nil
		c.heldEvents--
	}
}

// flushCounts sends the held counts that are due. With all set, it
// sends every held count, for shutdown. It also forgets errors that
// have been quiet for a window. A bucket with no held event has no
// event to carry its count, so the count goes to the dropped counter,
// and the next event sent reports it.
func (c *client) flushCounts(all bool) {
	now := c.now()
	type pending struct {
		e     *event
		count int
	}
	var (
		due  []pending
		lost int
	)

	c.breakerMu.Lock()
	for key, bk := range c.buckets {
		if bk.held > 0 && (all || now.Sub(bk.lastCount) >= countEvery) {
			if bk.last != nil {
				due = append(due, pending{bk.last, bk.held})
			} else {
				lost += bk.held
			}
			bk.held, bk.lastCount = 0, now
			c.freeHeld(bk)
		}
		if bk.held == 0 && now.Sub(bk.windowStart) >= sampleWindow && now.Sub(bk.lastCount) >= sampleWindow {
			delete(c.buckets, key)
		}
	}
	c.breakerMu.Unlock()
	c.dropped.Add(int64(lost))

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

	if c.journal != nil {
		switch c.writeJournal(body) {
		case journalOK:
			c.record(true, c.now())
		case journalWriteFailed:
			c.record(false, c.now())
			c.dropped.Add(dropped + int64(lost))
		case journalTooLarge:
			c.dropped.Add(dropped + int64(lost))
		}
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
// fingerprint, else the top three in-app frames alone, else the type
// and the message with its digits replaced. Dropping the type and
// message when in-app frames exist keeps one call site in one bucket
// even when caller text varies. It returns an 8-byte hash of that
// text, whatever its length, so a key holds no caller text. The seed
// is random for each client, so a caller cannot craft two texts with
// one hash. A real collision makes two errors share one sampling
// allowance. It loses no count. The worker calls it for each event, so
// it hashes in place and builds no string.
func fingerprintKey(seed maphash.Seed, e *event) uint64 {
	var h maphash.Hash
	h.SetSeed(seed)
	switch {
	case len(e.Fingerprint) > 0:
		h.WriteString("own:")
		for i, p := range e.Fingerprint {
			if i > 0 {
				h.WriteByte(0)
			}
			h.WriteString(p)
		}
	default:
		if frames := eventFrames(e); len(frames) > 0 {
			var (
				top  [3]*frame
				nTop int
			)
			for i := len(frames) - 1; i >= 0 && nTop < len(top); i-- {
				if f := &frames[i]; f.InApp {
					top[nTop] = f
					nTop++
				}
			}
			if nTop > 0 {
				h.WriteString("frames:")
				for i, f := range top[:nTop] {
					if i > 0 {
						h.WriteByte(0)
					}
					h.WriteString(f.Function)
					h.WriteByte('@')
					h.WriteString(f.Filename)
				}
				break
			}
		}
		switch {
		case e.Exception != nil && len(e.Exception.Values) > 0:
			v := e.Exception.Values[len(e.Exception.Values)-1]
			h.WriteString("type:")
			h.WriteString(v.Type)
			h.WriteByte(0)
			writeNoDigits(&h, v.Value)
		case e.Message != "":
			h.WriteString("message:")
			writeNoDigits(&h, e.Message)
		}
	}
	if key := h.Sum64(); key != overflowKey {
		return key
	}
	return overflowKey + 1
}

func eventFrames(e *event) []frame {
	if e.Exception != nil && len(e.Exception.Values) > 0 {
		v := e.Exception.Values[len(e.Exception.Values)-1]
		if v.Stacktrace != nil && len(v.Stacktrace.Frames) > 0 {
			return v.Stacktrace.Frames
		}
	}
	if e.Stacktrace != nil {
		return e.Stacktrace.Frames
	}
	return nil
}

// writeNoDigits writes s to h with each run of ASCII digits replaced
// by one "?". It gives the result of the regexp [0-9]+ with
// ReplaceAllString, at a small part of the cost. Other bytes, invalid
// UTF-8 too, go through unchanged.
func writeNoDigits(h *maphash.Hash, s string) {
	for i := 0; i < len(s); {
		if !isDigit(s[i]) {
			j := i + 1
			for j < len(s) && !isDigit(s[j]) {
				j++
			}
			h.WriteString(s[i:j])
			i = j
			continue
		}
		h.WriteByte('?')
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	}
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }
