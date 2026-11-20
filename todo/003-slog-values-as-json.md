# Encode slog attribute values as JSON

Fixes: `find_01DvC4sWG6qxuswqAMFRSsAc`.

`claudesec show <id>` prints the attack path and the fix criteria.
Match the finding to this plan by its ID, not by its title.

## Need

`captureRecord` stores `WithExtra(a.Key, attrText(v))` for each
attribute (`slog.go:51`). `attrText` resolves each `slog.LogValuer`,
then returns `v.String()` (`slog.go:64-67`). For a `KindAny` value,
that is `fmt` text. `fmt` prints each field, exported or not. It
ignores `MarshalJSON`, `MarshalText`, and `json:"-"`. `encodeExtra`
then gets a string, so the type's own redaction cannot apply.

`README.md:39-42` says that a type that redacts itself in logs is
redacted in events too. That is true only for `slog.LogValuer`.
Commit `955f83d` fixed `find_01HNZoye689yfFaMsVfvqaQL` for
`LogValuer` only.

Probe at `07a8a84`, journal sink: a struct with `MarshalJSON`, a
`json:"-"` field, and an unexported field sent both secret values to
the journal.

Threat model, from `todo/README.md`:

- Attacker: a reader of the sockeye error page, the journal, or a
  compromised endpoint. No active step is needed.
- Consequence: a field that the host keeps out of its logs goes to
  the sink, up to 2 KiB for each value logged at error level.
- Boundary: each event field that `SlogHandler` writes.

## Change

1. For a `KindAny` value that is not an `error`, encode the resolved
   value with `json.Marshal(v, wireJSON)` on the caller goroutine.
   The `json` v2 encoder honors `MarshalJSON`, `MarshalText`,
   `json:"-"`, and field visibility. Store the result as the extra
   value, under the `maxExtraBytes` limit.
2. For an `error`, store `Error()`, as `slog.JSONHandler` does.
3. If the encode fails or panics, store a fixed note. Do not fall back
   to `fmt` text, because that text can hold the hidden fields.
4. Keep scalar kinds as they are now. Encode a group as a JSON object
   with the same rules for each member. This changes the group form
   from `[k=v …]` to an object, so confirm that the sockeye error page
   shows an object extra.
5. Update the `SlogHandler` comment and `README.md:39-42`. Name each
   path that events honor: `LogValuer`, `MarshalJSON`, `MarshalText`,
   `json` tags, and field visibility. Name the path they do not honor:
   the wrapped handler's `ReplaceAttr`.

## Not covered

`encodeExtra` falls back to `fmt.Sprint` when an encode fails
(`event.go:127`). A host that calls `WithExtra` with a struct that has
a `chan` field and a `json:"-"` secret can send the secret. The threat
model trusts the host, so this plan does not change that path. After
this change lands, ask the scanner to judge it.

## Options

- This plan: one function changes, and the documentation follows.
  Cost: an extra changes form from a flat string to a JSON value. No
  new moving parts. No CI cost.
- Document only: the leak stays, so the finding stays open.
- Accept the risk: the accepted risk in `todo/README.md` covers what
  the host sends, not what the host's type hides. It does not fit.

## Verify

- Tests next to the `LogValuer` test cover `MarshalJSON`,
  `MarshalText`, `json:"-"`, and an unexported field. Each test runs
  at the top level and in a group, for the HTTP sink and the journal
  sink. The event holds only the redacted form.
- A string, a number, an error, and a group still arrive.
- A value whose encode fails or panics arrives as the note.
- The capture-ownership tests still pass with `-race`, and an extra
  past 2 KiB still becomes the size note.
