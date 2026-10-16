# Agents guide

errs is an error client for Go applications. It sends to Sentry, to
sockeye, or to both. See `README.md` for usage and `doc.go` for design
details.

## Writing

Write every word in ASD-STE100 Simplified Technical English (STE):
Markdown docs, code comments, commit messages, change descriptions,
and replies in an agent conversation.
See <https://en.wikipedia.org/wiki/Simplified_Technical_English>.

STE is a controlled English for technical writing: one meaning per
word, one idea per sentence, and the actor named. It is not a house
style. It exists so every reader reads a sentence the same way. That
includes a tired reader, a reader in a second language, and an agent
that matches on words.

- One idea per sentence. Keep an instruction to 20 words and a
  description to 25.
- Active voice, present tense, and the actor named: say what acts,
  rather than writing "the token is refused".
- One word, one meaning. Keep a term the same everywhere rather than
  varying it for tone.
- Use the simple verb, not a noun made from it: "run the formatter",
  not "perform execution of the formatter".
- Cut what carries nothing: "simply", "just", "note that", "in order
  to".
- Put a warning or a limit before the step it applies to.
- STE limits a sentence, not a text. Keep each sentence short, but
  keep the sentence that defines a term or connects a cause to its
  effect.
- STE permits technical names. Name the helper, the type, or the
  function, not a vague noun such as "the snapshot".

Apply it to prose, not to code: an identifier, a command, and a quoted
error message stay as they are.

## Architecture

One package at the repo root. Standard library only, plus test
assertions from `github.com/croaky/is`.

- `errs.go` — `Init`, `Flush`, `CaptureException`, `CaptureMessage`,
  event dispatch, worker loop, and HTTP transport retry loop.
- `event.go` — event types, JSON envelope serialization, error
  unwrapping, and stack frame resolution.
- `dsn.go` — Sentry DSN parser and authentication header generation.
- `sockeye.go` — sockeye DSN parser, JSON body, sampling by error, and
  circuit breaker.
- `stack.go` — `Wrap`, `Errorf`, `PanicError`, stack program counter
  capture, and error chain walking.
- `slog.go` — `SlogHandler`, which reports error-level slog records.
- `doc.go` — package documentation.

## Checks

The root `Checkfile` is the list, and CI runs it on every push. Run the
same things before committing, since a check that fails locally fails
there:

```sh
go run golang.org/x/tools/cmd/goimports@v0.45.0 -local "$(go list -m)" -w .
go vet ./...
go test -trimpath -buildvcs=false -race -cover ./...
git ls-files -z '*.go' | xargs -0 gopls check -severity=hint
go run golang.org/x/tools/cmd/deadcode@v0.45.0 -test ./...
git ls-files -z 'scripts/*' '*.sh' | xargs -0 shellcheck
dprint fmt
```

`goimports` and `dprint` write here, where CI runs `goimports -l` and
`dprint check`. Fix formatting in the change, since a CI job that
rewrites a file has nowhere to put changes.

## Tests

Use red/green TDD where possible.

- Every exported helper needs a test. The `deadcode` check enforces
  it.
- Wire-format tests use `httptest.Server`.
- Zero retry delays in tests via `TestMain` so tests execute quickly.

## Documentation

`doc.go` and `README.md` describe package behavior. Update documentation
when package behavior changes.

## Changes

Work happens on a sockeye change. `soc checkout` allocates one and
prints a worktree; `soc edit` sets its title and description. Do the
edit before the code, not after.

After a push, read the checks with `soc push --wait`.

## Commits

- Prefix with what the change acts on: `errs:`, `doc:`, `ci:`.
- Imperative mood, lowercase except proper nouns. Hard-wrap at 72.
- Include why, not just what.
- Write the subject for a teammate who has not read the code.
- Write the body as a change description. The first push of a branch
  with one commit copies the commit message to the change.
- Sign your work with a `Co-Authored-By` trailer.

## Releases

sockeye is origin and holds no tags. `scripts/tag vX.Y.Z` publishes one
annotated tag to the GitHub mirror, which is what `go get` resolves.
