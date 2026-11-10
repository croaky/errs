package errs

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"time"
	"unicode"
)

var stackErrType = reflect.TypeFor[*stackErr]()

// event represents an error or message event payload.
type event struct {
	EventID     string                    `json:"event_id"`
	Timestamp   string                    `json:"timestamp"`
	Platform    string                    `json:"platform"`
	Level       string                    `json:"level"`
	Environment string                    `json:"environment,omitempty"`
	Release     string                    `json:"release,omitempty"`
	ServerName  string                    `json:"server_name,omitempty"`
	SDK         sdkInfo                   `json:"sdk"`
	Tags        map[string]string         `json:"tags,omitempty"`
	Extra       map[string]jsontext.Value `json:"extra,omitempty"`
	Fingerprint []string                  `json:"fingerprint,omitempty"`
	Message     string                    `json:"message,omitempty"`
	Count       int                       `json:"count,omitzero"`
	Exception   *exceptionPayload         `json:"exception,omitempty"`
}

// The limits on the caller text in one event. With the message and
// exception limits and a 64-frame stack, they keep an encoded event
// under about 200 KiB. The fingerprint limits also bound the sampling
// key of fingerprintKey.
const (
	maxTags             = 50
	maxExtras           = 50
	maxFingerprintParts = 10
	maxKeyBytes         = 64
	maxTagBytes         = 1 << 10
	maxExtraBytes       = 2 << 10
	maxFingerprintBytes = 256
	maxLevelBytes       = 16
)

type sdkInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type exceptionPayload struct {
	Values []exceptionValue `json:"values"`
}

type exceptionValue struct {
	Type       string      `json:"type"`
	Value      string      `json:"value"`
	Stacktrace *stacktrace `json:"stacktrace,omitempty"`
}

type stacktrace struct {
	Frames []frame `json:"frames"`
}

type frame struct {
	AbsPath  string `json:"abs_path,omitempty"`
	Filename string `json:"filename,omitempty"`
	Function string `json:"function,omitempty"`
	Lineno   int    `json:"lineno"`
	InApp    bool   `json:"in_app"`
}

// newEvent constructs a base event with default fields populated.
func (c *client) newEvent(level string, opts []Option) *event {
	now := c.now()
	e := &event{
		EventID:     newEventID(),
		Timestamp:   now.UTC().Format(time.RFC3339),
		Platform:    "go",
		Level:       level,
		Environment: c.environment,
		Release:     c.release,
		ServerName:  c.serverName,
		SDK:         sdkInfo{Name: sdkName, Version: sdkVersion},
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// truncate trims string s to maxMessageBytes bytes at a rune boundary.
func truncate(s string) string { return cut(s, maxMessageBytes) }

// cut trims s to n bytes at a rune boundary. It returns a copy of a
// cut string, so the result does not keep the long string in memory.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	end := n
	for end > 0 && !isRuneStart(s[end]) {
		end--
	}
	return strings.Clone(s[:end])
}

// encodeExtra encodes one extra value as JSON on the caller goroutine,
// so the worker reads no memory the caller can still change. A value
// that JSON cannot encode, such as an error, a channel, or a NaN, goes
// as its fmt.Sprint text. A value whose encoding is past maxExtraBytes,
// or whose marshal method panics, goes as a short note.
func encodeExtra(v any) (out jsontext.Value) {
	defer func() {
		if r := recover(); r != nil {
			out = noteValue("extra value panicked while it encoded")
		}
	}()
	b, err := json.Marshal(v, wireJSON)
	if err != nil {
		b, _ = json.Marshal(cut(fmt.Sprint(v), maxExtraBytes-64), wireJSON)
	}
	if len(b) > maxExtraBytes {
		return noteValue(fmt.Sprintf("extra was %d bytes; the limit is %d", len(b), maxExtraBytes))
	}
	return b
}

func noteValue(s string) jsontext.Value {
	b, _ := json.Marshal(s)
	return b
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// exceptionFromError builds an exception payload for err.
func exceptionFromError(err error, fallbackPCs []uintptr, root, inAppPrefix string) *exceptionPayload {
	pcs := stackFromError(err)
	if pcs == nil {
		pcs = fallbackPCs
	}
	val := exceptionValue{
		Type:  errorTypeName(err),
		Value: truncate(err.Error()),
	}
	if frames := resolveFrames(pcs, root, inAppPrefix); len(frames) > 0 {
		val.Stacktrace = &stacktrace{Frames: frames}
	}
	return &exceptionPayload{Values: []exceptionValue{val}}
}

var plainErrorType = reflect.TypeOf(errors.New(""))

const maxTitleBytes = 40

// errorTypeName returns the type name used for issue grouping.
func errorTypeName(err error) string {
	t := baseErrorType(err)
	if t == nil {
		return "error"
	}
	if t == plainErrorType {
		if title := titleFromMessage(err.Error()); title != "" {
			return title
		}
	}
	return t.String()
}

// titleFromMessage extracts the leading error phrase before the first ": ".
func titleFromMessage(msg string) string {
	title, _, _ := strings.Cut(msg, ": ")
	title = strings.TrimSpace(title)
	if title == "" || len(title) > maxTitleBytes {
		return ""
	}
	if strings.ContainsFunc(title, notLabelRune) {
		return ""
	}
	return title
}

func notLabelRune(r rune) bool {
	return unicode.IsDigit(r) || !unicode.IsPrint(r)
}

func baseErrorType(err error) reflect.Type {
	for e := err; e != nil; {
		t := reflect.TypeOf(e)
		if t != nil && !isWrapperType(t) {
			return t
		}
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			e = u.Unwrap()
		case interface{ Unwrap() []error }:
			for _, c := range u.Unwrap() {
				if t := baseErrorType(c); t != nil {
					return t
				}
			}
			return nil
		default:
			return nil
		}
	}
	return nil
}

func isWrapperType(t reflect.Type) bool {
	if t == stackErrType {
		return true
	}
	base := t
	if base.Kind() == reflect.Pointer {
		base = base.Elem()
	}
	// Compare the package and the name apart, so no string is built.
	switch base.PkgPath() {
	case "fmt":
		name := base.Name()
		return name == "wrapError" || name == "wrapErrors"
	case "errors":
		return base.Name() == "joinError"
	default:
		return false
	}
}

// resolveFrames converts program counters into a slice of stack frames.
func resolveFrames(pcs []uintptr, root, inAppPrefix string) []frame {
	if len(pcs) == 0 {
		return nil
	}
	out := make([]frame, 0, len(pcs))
	cf := runtime.CallersFrames(pcs)
	for {
		f, more := cf.Next()
		if f.Function == "" {
			if !more {
				break
			}
			continue
		}
		if strings.HasPrefix(f.Function, "runtime.") {
			if !more {
				break
			}
			continue
		}
		out = append(out, frame{
			AbsPath:  f.File,
			Filename: relativeTo(f.File, root),
			Function: f.Function,
			Lineno:   f.Line,
			InApp:    inApp(f.Function, inAppPrefix),
		})
		if !more {
			break
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// inApp reports whether the function belongs to the application module:
// fn starts with prefix and then "/" or ".". resolveFrames calls it
// for each frame, so it builds no string.
func inApp(fn, prefix string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" || len(fn) <= len(prefix) || !strings.HasPrefix(fn, prefix) {
		return false
	}
	c := fn[len(prefix)]
	return c == '/' || c == '.'
}

// relativeTo trims root and "/" from the file path when path starts
// with them. resolveFrames calls it for each frame, so it builds no
// string.
func relativeTo(path, root string) string {
	if root == "" || len(path) <= len(root) || !strings.HasPrefix(path, root) || path[len(root)] != '/' {
		return path
	}
	return path[len(root)+1:]
}
