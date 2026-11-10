package errs

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/croaky/is"
)

// TestParseDSNPerfEndpoint: the perf endpoint keeps the DSN's scheme,
// host, and port, and changes only the path. A host, a repo, or a
// service named "errors" stays as it is.
func TestParseDSNPerfEndpoint(t *testing.T) {
	is := is.New(t)
	for _, c := range []struct{ dsn, errors, perf string }{
		{"https://k@sockeye.example/errors/app/web", "https://sockeye.example/errors/app/web", "https://sockeye.example/perf/app/web"},
		{"https://k@errors/errors/app/web", "https://errors/errors/app/web", "https://errors/perf/app/web"},
		{"http://k@errors:8080/errors/app/web", "http://errors:8080/errors/app/web", "http://errors:8080/perf/app/web"},
		{"https://k@sockeye.example/errors/errors/errors", "https://sockeye.example/errors/errors/errors", "https://sockeye.example/perf/errors/errors"},
	} {
		d, err := parseDSN(c.dsn)
		is.NoErr(err)
		is.Eq(d.endpoint, c.errors)
		is.Eq(d.perfEndpoint, c.perf)
	}
}

// TestRecordRequestPostsToDSNHost: a DSN whose host is "errors" sends
// its perf batches, with the key, to that host and not to a host named
// "perf".
func TestRecordRequestPostsToDSNHost(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	var mu sync.Mutex
	var urls []string
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		urls = append(urls, r.URL.String())
		mu.Unlock()
		return &http.Response{StatusCode: 202, Body: http.NoBody, Header: http.Header{}}, nil
	})}
	is.NoErr(Init(Options{DSN: "https://sek_test@errors/errors/app/web", HTTPClient: client}))

	RecordRequest(Request{Route: "/a", Status: 200})
	Flush(time.Second)

	mu.Lock()
	defer mu.Unlock()
	is.True(len(urls) > 0)
	for _, u := range urls {
		is.Eq(u, "https://errors/perf/app/web")
	}
}

// TestParseDSNErrorOmitsKey: a DSN that url.Parse refuses gives an
// error that names the expected form. Neither the error nor any error
// it wraps quotes the key.
func TestParseDSNErrorOmitsKey(t *testing.T) {
	is := is.New(t)
	const key = "sek_MARKER"
	for _, s := range []string{
		`"https://` + key + `@sockeye.example/errors/app/web"`,
		"https://" + key + "@sockeye.example:port/errors/app/web",
		"https://" + key + "@sockeye.example/errors/%zz/web",
		"https://" + key + "@sockeye.example/errors/app/web\x7f",
		"https://" + key + "^@sockeye.example/errors/app/web",
		"https://" + key + "@sock eye.example/errors/app/web",
	} {
		_, err := parseDSN(s)
		is.HasErr(err)
		is.True(strings.Contains(err.Error(), "want https://<key>@<host>/errors/<repo>/<service>"))
		for e := err; e != nil; e = errors.Unwrap(e) {
			if strings.Contains(e.Error(), key) {
				t.Fatalf("%q: error quotes the key: %v", s, e)
			}
		}
	}
}

// TestParseDSNAcceptsKeyInGoodForm: the error rule does not refuse a
// DSN that url.Parse reads.
func TestParseDSNAcceptsKeyInGoodForm(t *testing.T) {
	is := is.New(t)
	d, err := parseDSN("  https://sek_MARKER@sockeye.example/errors/app/web\n")
	is.NoErr(err)
	is.Eq(d.key, "sek_MARKER")
}
