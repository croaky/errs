# Plans

Each `NNN-name.md` file in this directory is a plan, numbered in
order. This file holds the plan order, the threat model that judges
each scanner finding, and the map from each open finding to its plan.
A plan groups the findings that one change touches. It is not one plan
for each finding. Delete a plan when its change lands.

## Order

A change that deletes code comes before a change that adds code. A
host outage comes before a memory limit, and a memory limit comes
before a leak of a low-value key.

1. `001-deliver-without-sync-dispatch.md`: remove `SyncDispatch`, cap
   `Retry-After`, and stop the send path from writing to the global
   `log` logger. This removes the deadlock and three other paths.
2. `002-copy-and-bound-event-fields.md`: encode `Extra` on the caller
   goroutine, put a byte limit on each event field, and resolve slog
   `LogValuer` values.
3. `003-bound-perf-record-fields.md`: put a byte limit on `Route`,
   `Pattern`, and the `perfExtra` notes.

Plans `001` and `002` both edit `errs.go` and `sockeye.go`. Do them in
one queue, `001` first. Plan `003` touches other code and can go in
parallel.

## Security findings

Claude Security scans the errs project (`spj_011Cgt9jiS1fWnAHFP3ivVju`).
`claudesec show <id>` prints the attack path and the fix criteria of a
finding, so no copy lives here. Match a finding to its plan by its ID,
not by its title, because the scanner can change a title.

A change that fixes a finding names its ID in a `Fixes:` trailer. After
the change lands, the owner runs `claudesec resolve --reason fixed`.
A finding that this threat model accepts gets
`--reason accepted_risk` and a `--note` that names the attacker, the
consequence, and the boundary.

- `find_01FpUmL896ZW9ZbXNTeNAQoq` (high, deadlock from slog re-entry):
  `001`.
- `find_01KpWnz33z2AFxnngetYB1kZ` (medium, `Flush` timeout in sync
  mode): `001`.
- `find_01QKxN2TpQgJtes8BYj8okzF` (medium, bucket map in sync mode):
  `001`, and the key bound in `002`.
- `find_01SpX13jbctTjok8jrKuBvuV` (medium, unbounded `Retry-After`):
  `001`.
- `find_01HZGyT2h5YLizFrM9BBDjka` (high, `Extra` read on the worker):
  `002`.
- `find_018QCU972JUpYvQnvjymceny` (medium, no byte limit on event
  fields): `002`.
- `find_01HNZoye689yfFaMsVfvqaQL` (low, slog redaction skipped): `002`.
- `find_01SPoaitRjRALBfBdHoKjk2T` (medium, perf `Route` limit): `003`.
- `find_01R33icG43Zji1zZ67VDisKE` (medium, `perfExtra` note size):
  `003`.

## Threat model

These are proposed statements. The owner confirms them, then pastes
this section into the scanner's threat model.

errs is a library. A host program calls it, and it sends error events
and request records to one sockeye endpoint. The host runs on the
public internet.

errs protects, in this order:

1. The host process. errs must not crash it, hang it, or let its
   memory grow without a limit. This comes first, because a host
   outage costs more than a lost event.
2. The DSN key. The key lets a holder post forged events and request
   records for one service. It gives no read access.
3. The text of events and records. errs sends it only to the DSN host.

errs does not protect delivery. An event that errs drops is a lost
report, not a host failure. errs counts each drop.

Adversaries, most likely first:

1. A remote user of the host. The user writes request paths, request
   values, and the text of errors and log lines. The user needs no
   credential. The boundary is each field that errs copies from the
   host into an event or a record.
2. The sockeye endpoint and the network to it. A hostile endpoint, a
   proxy, a peer on an `http` DSN, an outage, and a revoked key all
   give the same answers: a stall, a failure status, or a
   `Retry-After`. The boundary is the response that `deliver` and
   `perfSender.post` read.
3. A reader of the host logs. The reader sees what the host logs, but
   not the host's secrets store. The boundary is each error and log
   line that errs writes.

The host program is trusted. A misuse by the host is a bug, but errs
must not turn a common host pattern into a crash.

Accepted risks:

- A reader of the sockeye error page sees the extras that the host
  sends. The readers are the operators and their agents, who also read
  the host logs.
- The operator as the attacker, or a DSN host that the operator names
  wrong. Out of scope, except where errs itself changes the host.
- A host `HTTPClient` with no `Timeout`. The host chose that client.
  The default client has a 5 s timeout.
