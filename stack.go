package errs

import (
	"fmt"
	"runtime"
)

// stackCarrier represents an error that provides a stack trace.
// Wrap and Errorf return errors that implement this interface.
type stackCarrier interface {
	StackTrace() []uintptr
}

// stackErr attaches program counters to an underlying error.
type stackErr struct {
	err error
	pcs []uintptr
}

func (e *stackErr) Error() string         { return e.err.Error() }
func (e *stackErr) Unwrap() error         { return e.err }
func (e *stackErr) StackTrace() []uintptr { return e.pcs }

// Wrap returns an error that prepends msg to err.
// It wraps err with %w and records the caller stack trace.
// It returns nil when err is nil.
func Wrap(err error, msg string) error {
	if err == nil {
		return nil
	}
	return &stackErr{
		err: fmt.Errorf("%s: %w", msg, err),
		pcs: callers(2),
	}
}

// Errorf formats an error message and records the caller stack trace.
// It supports the %w format verb.
func Errorf(format string, args ...any) error {
	return &stackErr{
		err: fmt.Errorf(format, args...),
		pcs: callers(2),
	}
}

// PanicError converts a recovered panic value into an error.
// It records the stack trace of the panic site.
// Call PanicError inside a deferred function that calls recover.
func PanicError(rec any) error {
	err, ok := rec.(error)
	if !ok {
		err = fmt.Errorf("%v", rec)
	}
	return &stackErr{
		err: fmt.Errorf("panic recovered: %w", err),
		pcs: callers(3),
	}
}

// callers captures program counters for the caller stack.
// skip specifies the count of stack frames to omit.
func callers(skip int) []uintptr {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(skip+1, pcs)
	return pcs[:n]
}

// stackFromError walks the error chain to find a stack trace.
// It inspects errors from Unwrap and Unwrap() []error and recovers
// panics from host StackTrace or Unwrap methods.
func stackFromError(err error) []uintptr {
	for e := err; e != nil; {
		if s, ok := e.(stackCarrier); ok {
			return safeStackTrace(s)
		}
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			e = safeUnwrapSingle(u)
		case interface{ Unwrap() []error }:
			for _, c := range safeUnwrapMulti(u) {
				if pcs := stackFromError(c); pcs != nil {
					return pcs
				}
			}
			return nil
		default:
			return nil
		}
	}
	return nil
}

func safeStackTrace(s stackCarrier) (pcs []uintptr) {
	defer func() {
		if recover() != nil {
			pcs = nil
		}
	}()
	return s.StackTrace()
}

func safeUnwrapSingle(u interface{ Unwrap() error }) (err error) {
	defer func() {
		if recover() != nil {
			err = nil
		}
	}()
	return u.Unwrap()
}

func safeUnwrapMulti(u interface{ Unwrap() []error }) (errs []error) {
	defer func() {
		if recover() != nil {
			errs = nil
		}
	}()
	return u.Unwrap()
}
