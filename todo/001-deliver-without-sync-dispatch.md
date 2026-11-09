# Deliver without SyncDispatch

Fixes: `find_01FpUmL896ZW9ZbXNTeNAQoq`, `find_01KpWnz33z2AFxnngetYB1kZ`,
`find_01QKxN2TpQgJtes8BYj8okzF`, and `find_01SpX13jbctTjok8jrKuBvuV`.

## Need

Three findings need `Options.SyncDispatch`: the deadlock, the `Flush`
that ignores its timeout, and the bucket map that never shrinks. The
fourth, an unbounded `Retry-After`, stops all reporting in the default
mode, and with `SyncDispatch` it also blocks each capture call. No
host program sets `SyncDispatch`. Only tests set it: 22 call sites in
this repo and one test in a consumer repo. The option keeps a second
send path in `Init`, `dispatch`, and `flush`, and that path is the
cause of the first three findings.

In the default mode, the deadlock finding has a second part. A failed
send writes a line through the global `log` logger. A host that
bridges that logger into `SlogHandler` turns each failed send into a
new event about the failure. Step 4 removes that loop.

## Change

1. Remove the `SyncDispatch` field from `Options` and the
   `syncDispatch` field from `client`. `Init` always starts the
   worker. `dispatch` always queues. `flush` always closes the queue
   and waits up to its timeout. The worker ticker then prunes
   `c.buckets` in each run, so the bucket map holds only the keys of
   the last `sampleWindow`.
2. Change the tests. A test of sampling or of the breaker calls
   `c.send`, `c.flushCounts`, and `c.post` directly, with its own
   `Clock`. A test of the wire format calls `Flush` before it reads
   the server. Update `doc.go` and `README.md`. Tag a new minor
   version, because the removed field breaks a build that names it.
3. Cap `Retry-After`. `parseRetryAfter` returns at most the largest
   step of `retryDelays` (10 s). It compares the seconds before it
   multiplies, so no value can overflow `time.Duration`. One `deliver`
   call then takes at most 4 tries of 5 s plus 3 waits of 10 s, and
   one `perfSender.post` call less. A server that wants a longer pause
   gets one from the breaker after 5 failures.
4. Write the diagnostics of `deliver` and `perfSender.post` through a
   logger that errs owns, `log.New(os.Stderr, "", log.LstdFlags)`, not
   through the global `log` logger. A host that bridges `log` into
   `SlogHandler` then cannot feed a failed send back into a capture.
   The lines hold no event text and no key, as now.

## Options for step 4

- A logger that errs owns (recommended). Cost: about 3 lines. The
  lines go to stderr even when the host moves the global `log` output
  with `log.SetOutput`. A host under systemd keeps them in the
  journal.
- Delete the 7 log lines. Cost: none in code, and the code gets
  smaller. The `dropped` extra of the next event and `DroppedRequests`
  still count each loss. An operator then sees nothing local when a
  key is revoked, and the 401 shows only on the sockeye side.
- A re-entry guard in `slogHandler.Handle`. Cost: a goroutine-local
  flag, which Go does not have, or a context value that `log.Printf`
  does not carry. Rejected: more machinery than the two options above.

## Accepted

The retry wait of `deliver` stays a `time.Sleep` that `Flush` cannot
stop. Attacker: the endpoint or a peer on the network. Consequence:
after `Flush` returns at its timeout, the worker can stay parked for
at most about 50 s, and the process exits under it. Boundary: the cap
in step 3. A cancellable wait adds a `select` on a close channel in
two retry loops for no gain at exit.

## Verify

- With `SlogHandler` as the slog default, `log` bridged at error
  level, and an endpoint that refuses every request, a capture returns
  and a later `log.Print` returns.
- A transient status with `Retry-After: 99999999999` lets the worker
  and the perf sender go on to the next item within the bound.
- After 10,000 events with distinct keys and two sample windows, the
  bucket map holds only the keys of the last window.
