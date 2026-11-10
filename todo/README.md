# Plans

Each `NNN-name.md` file in this directory is a plan, numbered in
order. This file holds the plan order and the map from each open
scanner finding to its plan.
A plan groups the findings that one change touches. It is not one plan
for each finding. Delete a plan when its change lands.

## Order

A change that deletes code comes before a change that adds code. A
host outage comes before a memory limit, and a memory limit comes
before a leak of a low-value key.

1. `002-copy-and-bound-event-fields.md`: encode `Extra` on the caller
   goroutine, put a byte limit on each event field, and resolve slog
   `LogValuer` values.
2. `003-bound-perf-record-fields.md`: put a byte limit on `Route`,
   `Pattern`, and the `perfExtra` notes.

Plans `002` and `003` touch different code and can go in parallel.

## Security findings

Claude Security scans the errs project (`spj_011Cgt9jiS1fWnAHFP3ivVju`).
`claudesec show <id>` prints the attack path and the fix criteria of a
finding, so no copy lives here. Match a finding to its plan by its ID,
not by its title, because the scanner can change a title.

A change that fixes a finding names its ID in a `Fixes:` trailer. After
the change lands, the owner runs `claudesec resolve --reason fixed`.
A finding that a plan accepts gets
`--reason accepted_risk` and a `--note` that names the attacker, the
consequence, and the boundary.

- `find_01HZGyT2h5YLizFrM9BBDjka` (high, `Extra` read on the worker):
  `002`.
- `find_018QCU972JUpYvQnvjymceny` (medium, no byte limit on event
  fields): `002`.
- `find_01HNZoye689yfFaMsVfvqaQL` (low, slog redaction skipped): `002`.
- `find_01SPoaitRjRALBfBdHoKjk2T` (medium, perf `Route` limit): `003`.
- `find_01R33icG43Zji1zZ67VDisKE` (medium, `perfExtra` note size):
  `003`.
