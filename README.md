# errs

errs is a Sentry error reporting client for Go applications.

The package provides a small API: `Init` and `Flush` at application
startup and shutdown, `CaptureException` and `CaptureMessage` at call
sites, and `Wrap` and `Errorf` to attach caller stack traces.

## Installation

```sh
go get github.com/croaky/errs@v0.1.0
```

## Usage

Configure the client during application startup:

```go
err := errs.Init(errs.Options{
	DSN:         os.Getenv("SENTRY_DSN"),
	Environment: "production",
	Release:     "v1.0.0",
})
if err != nil {
	log.Fatal(err)
}
defer errs.Flush(2 * time.Second)
```

Wrap errors to record caller stack traces:

```go
if err := processItem(id); err != nil {
	return errs.Wrap(err, "process item")
}
```

Report errors to Sentry:

```go
if err := runJob(); err != nil {
	errs.CaptureException(err, errs.WithTag("job", "worker"))
}
```

Report informational messages:

```go
errs.CaptureMessage("task completed", errs.WithLevel("info"))
```

## Architecture

- The client formats events into Sentry envelopes and sends them over
  HTTP.
- An asynchronous worker queues events up to 1,000 items and drops
  events when the queue is full.
- The sender retries transport errors and transient HTTP status codes
  (429, 500, 502, 503, 504) with backoff.
- The sender obeys the `Retry-After` HTTP header.
- Frames matching the application module path are marked as `in_app`.
