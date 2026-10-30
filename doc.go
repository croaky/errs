// Package errs provides an error reporting client for Go.
//
// The package exports a small surface: Init and Flush at startup
// and shutdown, CaptureException and CaptureMessage at call sites,
// and Wrap, Errorf, and PanicError to attach stack traces to errors.
//
// Events are queued on a bounded channel and sent to sockeye over HTTP
// with retries and backoff. The client includes sampling by error and
// a circuit breaker.
//
// RecordRequest sends served requests to sockeye's perf page through a
// sender of its own, with its own bounded queue and circuit breaker. A
// request can carry a user id for usage counts and extra properties
// for a request the service chose to inspect.
package errs
