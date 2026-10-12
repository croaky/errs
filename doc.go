// Package errs provides a Sentry error reporting client for Go.
//
// The package exports a small surface: Init and Flush at startup
// and shutdown, CaptureException and CaptureMessage at call sites,
// and Wrap, Errorf, and PanicError to attach stack traces to errors.
//
// Events are queued on a bounded channel and sent to Sentry over HTTP
// with retries and backoff.
package errs
