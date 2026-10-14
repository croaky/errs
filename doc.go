// Package errs provides an error reporting client for Go.
//
// The package exports a small surface: Init and Flush at startup
// and shutdown, CaptureException and CaptureMessage at call sites,
// and Wrap, Errorf, and PanicError to attach stack traces to errors.
//
// Events are queued on a bounded channel and sent over HTTP with
// retries and backoff. Options.DSN sends them to Sentry.
// Options.SockeyeDSN sends them to sockeye, with sampling by error and
// a circuit breaker. With both set, the client sends to both.
package errs
