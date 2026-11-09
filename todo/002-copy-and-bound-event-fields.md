# Copy and bound event fields

Fixes: `find_01HZGyT2h5YLizFrM9BBDjka`, `find_018QCU972JUpYvQnvjymceny`,
and `find_01HNZoye689yfFaMsVfvqaQL`. Adds the key bound of
`find_01QKxN2TpQgJtes8BYj8okzF`.

Do this after `001`. Both plans edit `errs.go` and `sockeye.go`.

## Need

An event keeps the caller's `Extra` values and `Fingerprint` slice by
reference. The worker encodes them later, without the caller's locks.
A host that writes a map after it passes the map to `WithExtra` can
crash with `concurrent map iteration and map write`, which `recover`
cannot stop. A remote user who repeats the error path makes the race
more likely.

Only `Message` and the exception `Value` have a byte limit. A remote
user whose text reaches a tag, an extra, a fingerprint, or a slog
record can make 1,000 queued events of any size. `truncate` returns a
substring, so a cut value keeps the whole string alive.

`captureRecord` formats `a.Value.Any()`. That skips the `LogValue`
method of a `slog.LogValuer`, so a value that the host redacts in its
logs goes to sockeye in clear text.

## Change

1. Encode at capture. `newEvent` runs the options, then encodes each
   extra to a `jsontext.Value` with `wireJSON` on the caller
   goroutine. A value that does not encode becomes its `fmt.Sprint`
   text, as `printExtra` does now. `event.Extra` becomes
   `map[string]jsontext.Value`. `WithFingerprint` copies its parts.
   The worker then runs no host code, so it needs no `recover`.
2. Add one helper, `cut(s string, n int) string`. It cuts at a rune
   boundary and returns `strings.Clone` of the result, so the cut
   value holds no reference to the long string. `truncate` calls it.
3. Apply limits in `newEvent`: 50 tags, 50 extras, and 10 fingerprint
   parts. Tag keys and extra keys get 64 bytes, tag values 1 KiB,
   fingerprint parts 256 bytes. An encoded extra past 2 KiB becomes a
   short note that gives its size, as `perfExtra` does. The `level`
   gets 16 bytes. With these limits and the 64-frame stack, the body
   has a known maximum of about 200 KiB, so no second check on the
   body is needed. The fingerprint limit also bounds the bucket key.
4. Clear `bk.last` after `send` or `flushCounts` posts the count of
   the bucket. A bucket then holds no event after its count goes out.
5. In `captureRecord`, call `a.Value.Resolve()` before it reads a
   value, and resolve each member of a group. `Resolve` stops a
   `LogValue` that panics or loops. Cut the `log` tag with `cut`, as
   `message` is cut.
6. State in the `SlogHandler` comment and in `README.md` that the
   `ReplaceAttr` of the wrapped handler does not apply to events.

## Cost

Step 1 moves one encode per extra from the worker to the goroutine
that calls `CaptureException`. An error path is not a hot path, but a
sampled-out event now pays for its encode too. Steps 2 to 5 add about
40 lines and one test for each limit.

## Options for step 1

- Encode on the caller goroutine (recommended). The worker reads only
  bytes that errs owns.
- Deep-copy each value with `reflect`. Rejected: the copy must walk
  each map, slice, pointer, and struct type, which is more code than
  the encode. The worker still runs host `MarshalJSON` methods later.
- Document "do not change a value after `WithExtra`" and add a
  `recover` on the worker. Cost: 3 lines. Rejected: a concurrent map
  write is a fatal error, not a panic, so the crash stays.

## Options for the slog attributes

- Resolve `LogValuer` and document `ReplaceAttr` (recommended). Cost:
  about 8 lines.
- Add a `ReplaceAttr` hook to `SlogHandler`. Cost: new API and its
  tests. Rejected under the threat model: the readers of sockeye are
  the readers of the host logs.
- Send no attributes. `captureRecord` sends the message and the `err`
  attribute only. Cost: the event loses its context. This removes the
  redaction gap and the attribute part of the size finding. Choose
  this if the attributes do not help triage.

## Accepted

Part 2 of `find_01HNZoye689yfFaMsVfvqaQL`: a redaction set with
`ReplaceAttr` on the wrapped handler does not reach the event.
Attacker: a reader of the sockeye error page. Consequence: that reader
sees an attribute that the host log line hides. Boundary: the
operators and their agents read both the host logs and sockeye. Step
6 documents the gap, and step 5 honors `LogValuer`.

Part 2 of `find_01QKxN2TpQgJtes8BYj8okzF`: the bucket map has no
fixed entry cap. Attacker: a remote user who makes errors with
distinct text. Consequence: after `001`, the map holds the keys of one
`sampleWindow`. The 1,000-event queue and one worker limit the rate of
new keys, because each new key costs one post. Step 3 and
`maxMessageBytes` limit each key to about 8 KiB. Boundary: the queue
limit. A fixed cap adds an eviction rule for no gain.

## Verify

- A race test changes a map right after it passes the map to
  `WithExtra`, for a sent event and for a held event. `go test -race`
  reports no race.
- A slog record with a message and an attribute of several megabytes
  gives a body under the maximum.
- An attribute whose `LogValue` returns a placeholder shows only that
  placeholder, at the top level, in a group, and under `err`.
