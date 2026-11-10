# Agents guide

errs is an error client for Go applications. It sends to sockeye.
See `README.md` for usage and `doc.go` for design details.

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
- `event.go` — event types, error unwrapping, and stack frame resolution.
- `dsn.go` — sockeye DSN parser.
- `sockeye.go` — sockeye backend, JSON body, sampling by error, and
  circuit breaker.
- `stack.go` — `Wrap`, `Errorf`, `PanicError`, stack program counter
  capture, and error chain walking.
- `slog.go` — `SlogHandler`, which reports error-level slog records.
- `perf.go` — `RecordRequest` and `DroppedRequests`, the request-record
  sender for sockeye's perf page.
- `doc.go` — package documentation.

## Checks

Pin every tool a check runs. `tools.mod` and `tools.sum` hold the
version and checksum of each tool. Run a tool as
`go tool -modfile=tools.mod <name>`. Add or update a tool with
`go get -modfile=tools.mod -tool <pkg>@<version>`. Do not run
`go run pkg@latest` or `go install pkg@latest` in a `Checkfile`, a
script, or CI. A moving version makes the same commit pass one week
and fail the next, and it runs new tool code nobody reviewed. A pinned
tool changes in a change, where the diff shows the update and CI runs
it.

Keep the tools out of `go.mod`. A `tool` line there puts the tool's
requirements in the module graph of every module that requires this
one. `tools.mod` names this module, but only
`go tool -modfile=tools.mod` reads it. The tool then reads `go.mod`,
so `govulncheck` scans the versions this module ships. Do not run
`go mod tidy -modfile=tools.mod`, because it copies the requirements
of `go.mod` into `tools.mod`. An app that no module requires puts its
`tool` lines in `go.mod`, unless a tool raises a requirement that the
app builds with.

Pin the tool, not its data. `govulncheck` reads the live vulnerability
database on every run, so the weekly scan still finds advisories
published after the commit. Never vendor or cache that data.

The root `Checkfile` is the list, and CI runs it on every push. Run the
same things before committing, since a check that fails locally fails
there:

```sh
go tool -modfile=tools.mod goimports -local "$(go list -m)" -w .
go vet ./...
go test -trimpath -buildvcs=false -race -cover ./...
git ls-files -z '*.go' | xargs -0 gopls check -severity=hint
go tool -modfile=tools.mod deadcode -test ./...
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

- Subject: `scope: imperative summary`, at most 72 characters, lowercase
  except proper nouns, no trailing period.
- The scope is what the change acts on: `event:` for the error events,
  `perf:` for the request records, `slog:`, `dsn:`, `stack:`. Not
  `errs:`, because every commit here is errs.
- Each repo also uses these scopes: `security:` for a fix to a security
  finding, `deps:` for a dependency or the Go version, `doc:` for the
  docs, `todo:` for a plan, and `ci:` for the Checkfile and its checks.
- A change that acts on more than one area takes the scope whose
  behavior changed. Write two scopes, with a comma between them, only
  when each one changed: `event, perf:`.
- The `merge:` line in the root `Checkfile` checks the change title at
  merge. It caps the text after the scope at 64 characters.
- Include _why_, not just _what_. Hard-wrap the body at 72.
- Write the subject for a teammate who has not read the code. Name the
  function, the field, or the request, not a term the change makes up.
- Write the body as a change description. sockeye squashes a change into
  one commit whose message is the change's title and description. The
  first push of a branch with one commit copies the commit message to
  the change, without the trailers the merge derives.
- Sign your work with a `Co-Authored-By` trailer at the end of the
  message, after a blank line. A trailer on the line after the subject
  becomes part of the subject.

## Trailers

End the change description with one block of trailers, with no blank
line between them. Keep each trailer on one line, even past 72
columns.

- `Requested-by: name, channel, thread or subject, date` records who
  asked for the work, when somebody other than the author asked.
- `Reported-by: Claude Security at claude.ai/security` credits a
  finding from the Claude Security scanner.
- `Fixes:` names each Claude Security finding the change fixes
  (`Fixes: find_01... and find_02...`), or each production error group
  (`Fixes: ERR-1F`). Do not write `Fixes: CWE-...`. A CWE names a
  category, not a finding.
- Do not type `Co-Authored-By`, `Reviewed-by`, or `Signed-off-by` in
  the description. The merge derives them from the commits and the
  comments.

The squash merge writes the description to `main`, not the commit
messages. The first push of a branch with one commit copies its
trailers to the description, except the ones the merge derives. A
later `soc edit` writes only what it reads, so keep the trailers in
it:

    git --no-pager log -1 --format=%B | sed '/^Co-Authored-By:/d' | soc edit

Before the merge, read the description in `soc show` and confirm that
it ends with the trailers.

## Releases

sockeye is origin and holds no tags. `scripts/tag vX.Y.Z` publishes one
annotated tag to the GitHub mirror, which is what `go get` resolves.
