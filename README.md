# errs

errs is an error reporting client for Go applications. It sends events
to sockeye.

The package provides a small API: `Init` and `Flush` at application
startup and shutdown, `CaptureException` and `CaptureMessage` at call
sites, and `Wrap` and `Errorf` to attach caller stack traces.

## Installation

```sh
go get github.com/croaky/errs@v0.4.0
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

Wrap the default slog handler to report every `slog.Error` call. The
call still writes its log line:

```go
slog.SetDefault(slog.New(errs.SlogHandler(slog.Default().Handler())))
```

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
- After 5 failed sends in a row, the client stops sending for 30
  seconds. It then tries once. A success closes the breaker, and a
  failure opens it again.
- The client counts events it discards: a full queue, an open
  breaker, and a send that fails after its retries. The next event
  that reaches sockeye carries the number as `extra.dropped`.

## Architecture

- The client posts each event as JSON to sockeye with a Bearer key.
- An asynchronous worker queues events up to 1,000 items and drops
  events when the queue is full.
- The sender retries transport errors and transient HTTP status codes
  (429, 500, 502, 503, 504) with backoff.
- The sender obeys the `Retry-After` HTTP header.
- Frames matching the application module path are marked as `in_app`.
