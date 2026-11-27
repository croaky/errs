package errs

import (
	"encoding"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
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
	Stacktrace  *stacktrace               `json:"stacktrace,omitempty"`
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

// cut trims s so its JSON-encoded string content stays within n bytes
// at a rune boundary. It returns a copy, also of a short s, so the
// result does not keep a longer string in memory. A short s can be a
// substring of a long string, such as r.Method of the request line.
func cut(s string, n int) string {
	encoded := 0
	for i := 0; i < len(s); {
		var step, enc int
		if b := s[i]; b < utf8.RuneSelf {
			step = 1
			switch {
			case b < 0x20:
				switch b {
				case '\b', '\f', '\n', '\r', '\t':
					enc = 2
				default:
					enc = 6
				}
			case b == '"' || b == '\\':
				enc = 2
			default:
				enc = 1
			}
		} else {
			r, size := utf8.DecodeRuneInString(s[i:])
			step = size
			if r == utf8.RuneError && size == 1 {
				enc = 3
			} else {
				enc = size
			}
		}
		if encoded+enc > n {
			return strings.Clone(s[:i])
		}
		encoded += enc
		i += step
	}
	return strings.Clone(s)
}

// eventKey makes a tag or extra key canonical. It cuts the key to
// maxKeyBytes, replaces each run of invalid UTF-8 with U+FFFD, and cuts
// again, because a replacement is longer than the byte it replaces.
// The wire encoder also replaces invalid bytes, but it refuses two map
// keys that become one JSON name. After eventKey, the event holds one
// entry for them. The first cut bounds the work for a long key.
func eventKey(s string) string {
	k := cut(s, maxKeyBytes)
	if utf8.ValidString(k) {
		return k
	}
	return cut(strings.ToValidUTF8(k, "\uFFFD"), maxKeyBytes)
}

var (
	jsonMarshalerType   = reflect.TypeFor[json.Marshaler]()
	jsonMarshalerToType = reflect.TypeFor[json.MarshalerTo]()
	textMarshalerType   = reflect.TypeFor[encoding.TextMarshaler]()
	textAppenderType    = reflect.TypeFor[encoding.TextAppender]()
)

// hasCustomMarshaler reports whether encoding/json/v2 invokes a JSON or
// text marshal method for v, including a pointer-receiver method on a
// non-pointer value.
func hasCustomMarshaler(v any) bool {
	switch v.(type) {
	case json.Marshaler, json.MarshalerTo, encoding.TextMarshaler, encoding.TextAppender:
		return true
	}
	t := reflect.TypeOf(v)
	if t == nil || t.Kind() == reflect.Pointer {
		return false
	}
	pt := reflect.PointerTo(t)
	return pt.Implements(jsonMarshalerType) ||
		pt.Implements(jsonMarshalerToType) ||
		pt.Implements(textMarshalerType) ||
		pt.Implements(textAppenderType)
}

// encodeExtra encodes one extra value as JSON on the caller goroutine,
// so the worker reads no memory the caller can still change. An error
// without a JSON or text marshal method goes as Error(), and a
// non-finite float goes as its text. Any other value that JSON cannot
// encode, or whose marshal method panics, goes as a fixed note so
// hidden fields never fall back to Error() or fmt text. A value whose
// encoding is past maxExtraBytes goes as a size note.
func encodeExtra(v any) (out jsontext.Value) {
	defer func() {
		if r := recover(); r != nil {
			out = noteValue("extra value panicked while it encoded")
		}
	}()
	var (
		b   []byte
		err error
	)
	switch x := v.(type) {
	case error:
		if hasCustomMarshaler(v) {
			b, err = json.Marshal(v, wireJSON)
		} else {
			b, err = json.Marshal(x.Error(), wireJSON)
		}
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			b, err = json.Marshal(strconv.FormatFloat(x, 'g', -1, 64), wireJSON)
		} else {
			b, err = json.Marshal(x, wireJSON)
		}
	case float32:
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			b, err = json.Marshal(strconv.FormatFloat(float64(x), 'g', -1, 32), wireJSON)
		} else {
			b, err = json.Marshal(x, wireJSON)
		}
	default:
		b, err = json.Marshal(v, wireJSON)
	}
	if err != nil {
		return noteValue("extra value could not encode")
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
// It returns a copy, so the title does not keep msg in memory.
func titleFromMessage(msg string) string {
	title, _, _ := strings.Cut(msg, ": ")
	title = strings.TrimSpace(title)
	if title == "" || len(title) > maxTitleBytes {
		return ""
	}
	if strings.ContainsFunc(title, notLabelRune) {
		return ""
	}
	return strings.Clone(title)
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
