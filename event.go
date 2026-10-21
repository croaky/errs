package errs

import (
	"errors"
	"reflect"
	"runtime"
	"strings"
	"time"
	"unicode"
)

var stackErrType = reflect.TypeFor[*stackErr]()

// event represents an error or message event payload.
type event struct {
	EventID     string            `json:"event_id"`
	Timestamp   string            `json:"timestamp"`
	Platform    string            `json:"platform"`
	Level       string            `json:"level"`
	Environment string            `json:"environment,omitempty"`
	Release     string            `json:"release,omitempty"`
	ServerName  string            `json:"server_name,omitempty"`
	SDK         sdkInfo           `json:"sdk"`
	Tags        map[string]string `json:"tags,omitempty"`
	Extra       map[string]any    `json:"extra,omitempty"`
	Fingerprint []string          `json:"fingerprint,omitempty"`
	Message     string            `json:"message,omitempty"`
	Count       int               `json:"count,omitempty"`
	Exception   *exceptionPayload `json:"exception,omitempty"`
}

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
func truncate(s string) string {
	if len(s) <= maxMessageBytes {
		return s
	}
	end := maxMessageBytes
	for end > 0 && !isRuneStart(s[end]) {
		end--
	}
	return s[:end]
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
	switch base.PkgPath() + "." + base.Name() {
	case "fmt.wrapError", "fmt.wrapErrors", "errors.joinError":
		return true
	default:
		return false
	}
}

// resolveFrames converts program counters into a slice of stack frames.
func resolveFrames(pcs []uintptr, root, inAppPrefix string) []frame {
	if len(pcs) == 0 {
		return nil
	}
	var out []frame
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

// inApp reports whether the function belongs to the application module.
func inApp(fn, prefix string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return false
	}
	return strings.HasPrefix(fn, prefix+"/") || strings.HasPrefix(fn, prefix+".")
}

// relativeTo trims root from the file path when path starts with root.
func relativeTo(path, root string) string {
	if root == "" {
		return path
	}
	if strings.HasPrefix(path, root+"/") {
		return path[len(root)+1:]
	}
	return path
}
