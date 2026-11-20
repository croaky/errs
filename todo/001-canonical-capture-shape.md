# Make event keys canonical and bound the sampling buckets

Fixes: `find_01VLctdCqkFe3FrVemjNMPBV` part (1) and
`find_01SCtdUmkWvecg3YEyhaU9Rt`.

`claudesec show <id>` prints the attack path and the fix criteria.
Match each finding to this plan by its ID, not by its title.

## Need

Both findings have one cause. The capture call owns its bytes, but it
does not make them canonical. The worker then derives JSON object
names and sampling keys from raw caller text. A failure or growth then
happens where no caller sees it.

Keys: `WithTag` (`errs.go:84`) and `WithExtra` (`errs.go:101`) store
`cut(key, maxKeyBytes)`. `cut` keeps invalid UTF-8. `captureRecord`
passes each slog attribute key to `WithExtra` (`slog.go:51`).
`wireJSON` (`sockeye.go:42-47`) replaces each invalid byte with
U+FFFD, but it still refuses a duplicate object name. Two Go map keys
then become one JSON name, and `post` drops the whole event
(`sockeye.go:150-154`). The caller already holds the event ID.

Buckets: `send` adds a `bucket` for each new `fingerprintKey` result
(`sockeye.go:78-82`). Nothing checks `len(c.buckets)` or the key size.
`flushCounts` deletes a bucket only after `sampleWindow`
(`sockeye.go:120-122`). A message key holds up to 8 KiB of caller
text (`sockeye.go:263-267`). `writeNoDigits` folds only ASCII digit
runs, so any other variation makes a new key.

Probe at `07a8a84`, journal sink, public API only:

- `WithExtra("k\xff", 1)` with `WithExtra("k\xfe", 2)` dropped the
  event. So did `WithTag("k\xff", …)` with `WithTag("k\uFFFD", …)`,
  and two such slog attributes. A control event arrived.
- 20,000 distinct 4 KB messages in about 1 s kept 80.3 MiB of heap.

Threat model, from `todo/README.md`:

- Attacker: a remote user of the host, with no credential.
- Consequence: host memory grows in proportion to the event rate for
  60 to 70 s (`find_01VLct…`). The user can stop the report of their
  own request (`find_01SCtd…`).
- Boundary: each key and each text field that errs copies from the
  host into an event.

## Change

1. Add one helper, `eventKey(s string) string`. It calls
   `strings.ToValidUTF8(s, "\uFFFD")`, then `cut(…, maxKeyBytes)`.
   `WithTag` and `WithExtra` call it. The map then holds one entry for
   keys that become one JSON name. The last option for that name
   wins, in option order.
2. Keep the drop in `post` for an encode failure, as a last guard.
   Add a test that no caller key reaches it.
3. Make the sampling key a fixed size. `fingerprintKey` writes into a
   `hash/maphash.Hash` with a seed for each client, and returns a
   `uint64`. `c.buckets` becomes `map[uint64]*bucket`. The random seed
   stops a user who crafts two texts with one hash. A real collision
   makes two errors share one sampling allowance. That is a sampling
   error, not a lost count.
4. Cap the bucket map at `maxBuckets` entries (proposed: 1,024). At
   the cap, `send` puts a new key in one overflow bucket. The overflow
   bucket samples and counts like any other bucket, so the group total
   stays exact.
5. Bound the held events. Each bucket past `fullPerWindow` keeps
   `bk.last`, an event of up to about 200 KiB. At the cap, that is
   about 200 MiB. Pick one bound, state it in bytes, and test it:
   - keep `bk.last` only for a fixed number of buckets, and send a
     count with no held event for the rest; or
   - keep a byte budget for all held events, from the cut sizes that
     `newEvent` already knows.

Decide step 5 in the change, and write the reason in its description.

## Not covered

`find_01VLct…` part (2): one call site can still make many keys.
`writeNoDigits` and `titleFromMessage` split one site by its message
text. A fix must group by frames and ignore the message when frames
exist. sockeye groups by the same rule, so sockeye must change with
it. Keep part (2) open, and write it in a later plan after the owner
decides the grouping.

## Options

- This plan: one helper for keys, one digest, and one cap. Cost: one
  hash for each event on the worker. Normal errors under the cap
  sample as before. No new moving parts. No operator work.
- Patch each key setter alone: a later setter can miss the rule. One
  helper covers `WithTag`, `WithExtra`, and `SlogHandler`.
- Accept the risk: the threat model puts host memory first, so the
  risk does not fit.

## Verify

- A test sends keys that differ only in invalid bytes, and an invalid
  byte against U+FFFD. It uses `WithTag`, `WithExtra`, and
  `SlogHandler`, for a whole event and for a held count event. The
  event arrives, and `c.dropped` does not change. Repeat at the 64-byte
  cut.
- A test sends more distinct keys than `maxBuckets` in one window,
  once with an open breaker and once with the journal sink. The map
  stays at `maxBuckets` entries. The held events stay under the step 5
  bound. The sum of each `count` equals the events sent.
- `TestFingerprintKey` and the sampling tests still pass for an error
  under the cap.
- `BenchmarkFingerprintKey` and `BenchmarkSendHeld`: compare time,
  bytes, and allocations with repeated runs on one toolchain.
