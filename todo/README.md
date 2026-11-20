# Plans

Each `NNN-name.md` file in this directory is a plan, numbered in
order. This file holds the plan order, the threat model that judges
each scanner finding, and the map from each open finding to its plan.
A plan groups the findings that one shared cause produces. It is not
one plan for each finding. Delete a plan when its change lands. Keep
this file while a plan or an open finding remains.

## Order

The threat model puts the host process first, then the DSN key, then
the event text. The plans follow that order.

Plans `001`, `002`, and `003` landed. No plan files remain.

## Security findings

Claude Security scans the errs project (`spj_011Cgt9jiS1fWnAHFP3ivVju`).
`claudesec show <id>` prints the attack path and the fix criteria of a
finding, so no copy lives here. Match a finding to its plan by its ID,
not by its title, because the scanner can change a title.

A change that fixes a finding names its ID in a `Fixes:` trailer. A
structural change does not close a finding that its tests do not
cover. After the change lands and each host that needs it runs the
new release, the owner runs `claudesec resolve --reason fixed`. A
finding that this threat model accepts gets `--reason accepted_risk`
and a `--note` that names the attacker, the consequence, and the
boundary.

Each finding below was scanned at `07a8a84`. A probe through the
public API reproduced each one at that commit.

- `find_01VLctdCqkFe3FrVemjNMPBV` (medium, the sampling bucket map has
  no item or byte bound): no plan yet. Part (1), the bound in items
  and bytes, landed in the change that deleted plan `001`. Part (2)
  stays open: one call site can still make many keys, because
  `writeNoDigits` and `titleFromMessage` split one site by its message
  text. A fix must group by frames and ignore the message when frames
  exist. sockeye groups by the same rule, so sockeye must change with
  it. Write a plan after the owner decides the grouping. This finding
  follows `find_01QKxN2TpQgJtes8BYj8okzF`, which `36f754c` fixed for
  growth over the life of the process.
- `find_01SCtdUmkWvecg3YEyhaU9Rt` (medium, keys that differ only in
  invalid UTF-8 drop the event): fixed in the change that deleted plan
  `001`. Run `claudesec resolve` after the rollout below.
- `find_01VcuEW9LtaXDRWn6CKcMsLs` (medium, the default client follows
  redirects with the key): fixed in the change that deleted plan `002`.
  Run `claudesec resolve` after the rollout below.
- `find_01DvC4sWG6qxuswqAMFRSsAc` (low, `SlogHandler` formats values
  with `fmt`): fixed in the change that deleted plan `003`. Run
  `claudesec resolve` after the rollout below. This finding follows
  `find_01HNZoye689yfFaMsVfvqaQL`, which `955f83d` fixed for
  `slog.LogValuer` only.

## Rollout

errs is a library. A fix closes a finding only where a host runs it.

1. Merge the change, then run `scripts/tag vX.Y.Z`.
2. Update errs in each host that the finding applies to: `dancroak`,
   `ivp`, the runners, and the workstation CLI. Read the `Init` call of
   each host to find whether it uses a DSN or the journal sink.
3. Run `claudesec resolve` after each such host deploys.

## Threat model

These are proposed statements, first written in `fa7a39e`. The owner
confirms them, then pastes this section into the scanner's threat
model.

errs is a library. A host program calls it, and it sends error events
and request records to one sockeye endpoint or to a journal writer.
The host runs on the public internet.

errs protects, in this order:

1. The host process. errs must not crash it, hang it, or let its
   memory grow without a limit. This comes first, because a host
   outage costs more than a lost event.
2. The DSN key. The key lets a holder post forged events and request
   records for one service. It gives no read access.
3. The text of events and records. errs sends it only to the DSN host
   or to the journal writer.

errs does not protect delivery. An event that errs drops is a lost
report, not a host failure. errs counts each drop.

Adversaries, most likely first:

1. A remote user of the host. The user writes request paths, request
   values, and the text of errors and log lines. The user needs no
   credential. The boundary is each field that errs copies from the
   host into an event or a record.
2. The sockeye endpoint and the network to it. A hostile endpoint, a
   proxy, a peer on an `http` DSN, an outage, and a revoked key all
   give the same answers: a stall, a failure status, a redirect, or a
   `Retry-After`. The boundary is the response that `deliver` and
   `perfSender.post` read.
3. A reader of the host logs or of the sockeye error page. The reader
   sees what the host logs and what the host sends, but not the
   host's secrets store. The boundary is each error, log line, and
   event field that errs writes.

The host program is trusted. A misuse by the host is a bug, but errs
must not turn a common host pattern into a crash.

Accepted risks:

- A reader of the sockeye error page sees the extras that the host
  sends. The readers are the operators and their agents, who also read
  the host logs. This does not cover a value that the host hides
  through its type's redaction. Plan `003` landed to handle that value.
- The operator as the attacker, or a DSN host that the operator names
  wrong. Out of scope, except where errs itself changes the host.
- A host `HTTPClient` with no `Timeout`. The host chose that client.
  The default client has a 5 s timeout.
- An `http` DSN. A peer on that network reads the key from each
  request. The operator chose the scheme. This statement is new, and
  `fa7a39e` did not have it.

None of the open findings fits an accepted risk.
