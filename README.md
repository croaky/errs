# errs

errs is an error reporting client for Go applications. It sends events
to sockeye.

The package provides a small API: `Init` and `Flush` at application
startup and shutdown, `CaptureException` and `CaptureMessage` at call
sites, and `Wrap` and `Errorf` to attach caller stack traces.

## Installation

```sh
go get github.com/croaky/errs@v0.9.0
```

## Usage

Configure the client during application startup:

```go
err := errs.Init(errs.Options{
	DSN:         os.Getenv("SOCKEYE_DSN"),
	Environment: "production",
	Release:     "v1.0.0",
})
if err != nil {
	log.Fatal(err)
}
defer errs.Flush(2 * time.Second)
```

Wrap an explicit slog handler (such as `slog.NewTextHandler` or
`slog.NewJSONHandler`) to report every `slog.Error` call. The call still
writes its log line:

```go
h := slog.NewTextHandler(os.Stderr, nil)
slog.SetDefault(slog.New(errs.SlogHandler(h)))
```

If you pass Go's built-in default handler from `slog.Default().Handler()`
to `SlogHandler`, `SlogHandler` replaces it with a text handler on
`log.Writer()` so `slog.SetDefault` does not deadlock `log`.

The handler sends the record attributes as extras. It resolves each
`slog.LogValuer` first and encodes non-scalar values as JSON, matching
`slog.JSONHandler`. Event extras honor `LogValuer`, `MarshalJSON`,
`MarshalJSONTo`, `MarshalText`, `AppendText`, `json` tags, and field
visibility, including on error values. Plain errors without a marshal
method use `Error()`. An `"err"` attribute that resolves to an error
becomes an exception that always uses `Error()`, so only
`slog.LogValuer` redacts on that path. Events ignore `fmt.Stringer` and
`fmt.Formatter`, follow reachable exported pointers, and do not honor
the wrapped handler's `ReplaceAttr`.

An empty `Release` reads the commit from the Go build info.

Wrap errors to record caller stack traces:

```go
if err := processItem(id); err != nil {
	return errs.Wrap(err, "process item")
}
```

Report errors:

```go
if err := runJob(); err != nil {
	errs.CaptureException(err, errs.WithTag("job", "worker"))
}
```

Report informational messages:

```go
errs.CaptureMessage("task completed", errs.WithLevel("info"))
```

## Sending to sockeye

[sockeye](https://sockeye.sh) groups errors on its `/errors` page and
links each group to the deploy that introduced it. Issue a key for a
registered service, and set `DSN` (or `SockeyeDSN`):

```sh
soc error dsn create <repo> <service>
# https://<key>@<host>/errors/<repo>/<service>
```

```go
err := errs.Init(errs.Options{
	DSN:         os.Getenv("SOCKEYE_DSN"),
	Environment: "production",
	Release:     os.Getenv("GIT_SHA"),
})
```

`Release` is the commit sha that runs. sockeye matches it to a deploy.
An invalid DSN makes `Init` return an error that names the expected form.
The error does not quote the DSN, so a log of it does not show the key.

Tag an event with its request, so sockeye links it to the log lines of
that request:

```go
errs.CaptureException(err, errs.WithRequestID(requestID))
```

The sockeye path protects both ends of the connection:

- The first 10 events of one error in a minute go out whole. After
  that, the client sends one event every 10 seconds with a `count` of
  the events it held back, so the group total stays exact. `Flush`
  sends the held counts.
- The client tracks at most 1,024 errors at one time. A new error past
  that shares one overflow allowance of 10 whole events in a minute,
  and the rest are counted. The client keeps the latest held event for
  at most 32 errors at one time, which is under about 6.4 MiB. Another
  error counts its held events, and its next event carries the count.
  If no next event comes, the count goes to the dropped counter.
- After 5 failed sends in a row, the client stops sending. It pauses
  for 30 seconds, then 1 minute, 2 minutes, 5 minutes, and 10 minutes
  on consecutive failures. A success closes the breaker.
- The client counts events it discards: a full queue, an open
  breaker, and a send that fails after its retries. The next event
  that reaches sockeye carries the number as `extra.dropped`.
- Each event has a size limit. An event keeps its first 50 tags and
  50 extras, and its first 10 fingerprint parts. The client cuts a
  key past 64 bytes, a tag value past 1 KiB, a fingerprint part past
  256 bytes, and a message or error text past 8 KiB. An extra whose
  JSON is past 2 KiB becomes a note that gives its size.
- The client replaces each run of invalid UTF-8 in a tag or extra key
  with U+FFFD. Keys that then match are one key, and the last one in
  option order wins.
- The capture call encodes each extra before it returns, so the caller
  can change a map or a slice after it passes it to `WithExtra`.
- The client does not follow HTTP redirects. `Init` copies
  `Options.HTTPClient` and ignores any caller `CheckRedirect`, because a
  redirect can send the DSN key to another host. A `3xx` response counts
  as a failed send, and an endpoint that moves needs a new DSN.

## Request metrics

`RecordRequest` sends one served request to sockeye's perf page, with
the key in the same DSN. It posts to `/perf/<repo>/<service>` on the
scheme, host, and port of the DSN. Call it where the service logs a
request:

```go
errs.RecordRequest(errs.Request{
	Route:    "/companies/show",
	Status:   200,
	Duration: time.Since(start),
	DB:       dbTime,
	Acquire:  poolWait,
	DBTimed:  true,
	Time:     start,
})
```

Set `DBTimed` only when the service measures its query time and pool
wait, as a pgx query tracer does. Then a request with no queries sends
`db_ms: 0`. Without `DBTimed`, the client sends no `db_ms` and no
`acquire_ms`, and sockeye's perf page shows the request's database
work as unavailable, not as zero.

`DB` and `Acquire` add up work: queries that run at the same time each
count, so they can be more than `Duration`. To show where the wall
time went, set `Elapsed` to a `DBElapsed`: the time in which only
queries ran, only pool waits ran, or both. The three never overlap, so
their sum is at most `Duration`, and sockeye shows them with the time
in which neither ran as four shares of the request. A nil `Elapsed`
sends no split.

Pass a route with no query string and no ids, so each page is one row.
`RecordRequest` removes a query string in all cases. It sends a route
past 256 bytes as `/{long}`, the label sockeye gives such a route.

For a standard Go `http.ServeMux`, set `Pattern: r.Pattern` after the
handler returns. The client sends this optional matched pattern without
changing its method, host, case or wildcard names. Sockeye uses it as
the shared metrics and log-filter label. Include the same `pattern`
field in the request log. A router that does not set `Request.Pattern`
can leave it empty and keep the existing route behavior. Do not build a
pattern from an untrusted URL. `RecordRequest` cuts a pattern past 256
bytes.

A record can also say who made the request and what it asked for:

```go
errs.RecordRequest(errs.Request{
	Route:  "/search",
	Status: 200,
	User:   strconv.FormatInt(userID, 10),
	Method: r.Method,
	Extra:  map[string]any{"q": q, "sort": sort},
	// ...
})
```

`User` is the service's own id for the signed-in user. sockeye counts
requests for each route, user, and day, and stores no other fact about
the user, so send an id, not a name or an email address. Set
`Options.CaptureUsage` when the service sets `User` on the requests it
counts. Each batch then carries the header `Perf-Capture: usage`, and
sockeye reads the time as covered for usage rather than as no data.

`Extra` is for a request the service chose to inspect, such as a
search's filters or an MCP tool's name and arguments, and sockeye keeps
a row for each record that has it. Its keys must match `[a-z0-9_]`, and
its JSON must be at most 2 KiB. `RecordRequest` replaces an `Extra`
that breaks a limit with an `invalid` or `truncated` note, because
sockeye refuses a whole batch for one bad record. A note holds no key
and no error text, so it is always under the limit. It cuts a `User`
past 64 bytes and drops a `Method` that is not one to sixteen letters.
Do not send a token, a password, a cookie, or a mail body.

sockeye takes the records only after an operator names the service's
producer: `soc perf source <repo>/<service> post`. Until then it
refuses the batches with 409, and the client discards them.

The request path protects both ends of the connection too:

- `RecordRequest` never blocks. It queues up to 5,000 records, and
  drops a record past that. It also drops a record whose `Time` is
  older than 55 minutes or whose year is outside `0..9999`, so one bad
  record does not fail its batch.
- `RecordRequest` copies each string it keeps. A queued record holds
  only the bytes it sends, not the request line that `r.Method` and
  `r.URL.Path` come from.
- A sender of its own posts a batch at 50 records, and every 10 seconds
  whatever it has. A quiet interval posts an empty batch, so sockeye
  reads it as quiet and not as missing. The sender starts on the first
  `RecordRequest`, so a process that records no requests posts
  nothing.
- Each batch has an `Idempotency-Key`, and each retry sends the same
  key and the same bytes, so sockeye counts a batch once. The sender
  tries a batch three times, after `Retry-After` (up to 10 seconds)
  when the server sends one.
- The sender has a circuit breaker of its own, on the schedule above.
  A broken perf endpoint does not stop errors, and the reverse.
- The next batch reports the records the client dropped in a
  `Dropped-Records` header. `DroppedRequests` returns the same count
  for the process. A 409 is not a drop.
- `Flush` sends the queued records too, within the same timeout.

## Architecture

- The client posts each event as JSON to sockeye with a Bearer key.
- An encode does not drop an event. A string with invalid UTF-8 goes
  with U+FFFD in place of each bad byte. A `time.Duration` in extra
  goes as integer nanoseconds. An `error` extra without a JSON or text
  marshal method goes as `Error()`, and a non-finite float goes as its
  text. Any other extra value that JSON cannot encode becomes a fixed
  note.
- An asynchronous worker queues events up to 1,000 items and drops
  events when the queue is full.
- The sender retries transport errors and transient HTTP status codes
  (429, 500, 502, 503, 504) with backoff.
- The sender obeys the `Retry-After` HTTP header, up to 10 seconds. A
  longer pause comes from the circuit breaker.
- The client writes its own failure lines to stderr, not through the
  global `log` logger. A host that bridges `log` into `SlogHandler`
  does not turn a failed send into a new event.
- Frames matching the application module path are marked as `in_app`.

## Explicit journal sink

A TLS-free service can choose `Options{Journal: os.Stdout}` instead of an
HTTP DSN. Journal and DSN/SockeyeDSN are mutually exclusive. This option
writes error events only; `RecordRequest` sends nothing for this client.
Request logs keep their separate journal producer.

The existing bounded queue, capture-time copies, sampling and circuit
breaker stay in place. Each output is one `sockeye.error.v1` marker,
then the existing event JSON and a newline. Exception type and stack,
event ID, release and fingerprint retain their existing fields. String
newlines are JSON escapes. An encoded body over 256 KiB is dropped with
a size diagnostic, not a partial event. The SDK does not add a source
claim or credential to the line. Trusted host policy must bind the console
unit to its service before collection is enabled.

The writer gets one Write per event. A failed or short write is counted
as dropped, not retried into a possible partial line. Diagnostics do not
print event contents or a writer's arbitrary error text. The SDK does not
close a writer owned by the caller. A successful Write proves only that
the writer accepted bytes, not journal persistence or server acceptance.
Process death can lose buffered events, and journal loss is unknown time.

Self-redacting slog values still resolve at capture time. This is not a
general secret scanner: do not put secrets in event text or metadata.
The SDK installs no panic hook. A caller that logs a recovered panic must
re-panic when continuing would leave the process unsafe.

This library option does not change a consumer, enable a live source,
create credentials, or configure a host collector.
