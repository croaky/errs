# Copy the exception type title at capture

Fixes: `find_01NCeHj3jHrxZ5ghsdEFee56`.

`claudesec show <id>` prints the attack path and the fix criteria.
Match the finding to this plan by its ID, not by its title.

## Need

`errorTypeName` calls `titleFromMessage(err.Error())` for a plain
`*errors.errorString` base error. `strings.Cut` and `strings.TrimSpace`
return a substring of the message. `exceptionFromError` stores that
substring in `exceptionValue.Type` with no copy. A title of at most 40
bytes then keeps the whole message in memory while the event waits in
`c.queue`, in the `deliver` retry loop, or as `bk.last`.

`cut` copies `Value`, and commit `7d9e251` copies the other event
strings. `Type` is the one event field that `7d9e251` did not copy.

Threat model: the repo has no `todo/security/README.md`. This plan
uses the proposed model from `todo/README.md` in commit `fa7a39e`. That
model puts the host process first.

- Attacker: a remote user of the host. The user needs no credential.
  The user writes text that the host puts into an error built with
  `errors.New` or `fmt.Errorf`, or into an `"err"` slog attribute.
- Consequence: host memory grows to about 1,000 times the message
  size, not 1,000 times 8 KiB. This lasts while sockeye is slow or
  down and before the breaker opens. A large message can exhaust host
  memory.
- Boundary: the `Type` field that errs copies from the host error into
  a queued event.

## Change

1. In `titleFromMessage`, return `strings.Clone(title)`. The clone
   happens after the length and rune tests, so it copies at most
   `maxTitleBytes` (40) bytes.
2. Add a test in the style of `TestEventCopiesShortStrings`. Build an
   exception from a plain error, and from an `errs.Wrap` or `%w` chain
   over a plain error. Give each one a message of about 1 MiB with a
   short `"title: "` prefix. Assert that `Type` equals the title and
   does not share memory with the message (`shares`).
3. Make the same test on the `SlogHandler` path with an untyped `"err"`
   attribute.

## Options

- Fix (this plan): one line and two tests. Cost: one allocation of at
  most 40 bytes for each plain error that has a title. Plain errors
  without a title, and typed errors, do not change. No new moving
  parts. CI time does not change in a measurable way.
- Remove the title feature: always send `*errors.errorString` as
  `Type`. This deletes about 15 lines. But sockeye then puts unrelated
  plain errors in one group, and operators must read `Value` to tell
  them apart. That costs more operator work than the fix costs code.
- Accept the risk: no code. A remote user can still pin about 1,000
  messages of any size in host memory during a sockeye outage. The
  threat model puts the host process first, so the risk does not fit.

Do the fix.

## Verify

- `go test -race ./...` passes with the new tests.
- `BenchmarkCaptureException` shows at most one more allocation of at
  most 40 bytes for a plain error with a title. Compare repeated runs
  on one toolchain.
