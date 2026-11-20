package errs

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/croaky/is"
)

type redirectTarget struct {
	name     string
	location string
}

var redirectStatuses = []int{
	http.StatusMovedPermanently,
	http.StatusFound,
	http.StatusSeeOther,
	http.StatusTemporaryRedirect,
	http.StatusPermanentRedirect,
}

var redirectTargets = []redirectTarget{
	{name: "other-host", location: "http://other.example/sink"},
	{name: "other-port", location: "https://sockeye.example:9443/sink"},
	{name: "http-same-host", location: "http://sockeye.example/sink"},
	{name: "subdomain", location: "https://sub.sockeye.example/sink"},
}

func newRedirectHarness(t *testing.T, status int, location string) (*http.Client, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	var primaryHits, targetHits atomic.Int64

	primary := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		w.Header().Set("Location", location)
		w.WriteHeader(status)
	}))
	t.Cleanup(primary.Close)

	httpTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(httpTarget.Close)

	httpsTarget := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(httpsTarget.Close)

	routes := map[string]string{
		"sockeye.example:443":     primary.Listener.Addr().String(),
		"other.example:80":        httpTarget.Listener.Addr().String(),
		"sockeye.example:80":      httpTarget.Listener.Addr().String(),
		"sockeye.example:9443":    httpsTarget.Listener.Addr().String(),
		"sub.sockeye.example:443": httpsTarget.Listener.Addr().String(),
	}
	var dialer net.Dialer
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dest, ok := routes[addr]
			if !ok {
				return nil, fmt.Errorf("unexpected dial target %s", addr)
			}
			return dialer.DialContext(ctx, network, dest)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}, &primaryHits, &targetHits
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := logger.Writer()
	logger.SetOutput(&buf)
	t.Cleanup(func() { logger.SetOutput(old) })
	return &buf
}

func TestErrorSendDoesNotFollowRedirects(t *testing.T) {
	for _, status := range redirectStatuses {
		for _, target := range redirectTargets {
			t.Run(fmt.Sprintf("%d/%s", status, target.name), func(t *testing.T) {
				is := is.New(t)
				resetActive(t)
				logs := captureLogs(t)
				hc, primaryHits, targetHits := newRedirectHarness(t, status, target.location)

				err := Init(Options{
					DSN:        testDSN,
					HTTPClient: hc,
				})
				is.NoErr(err)
				c := active.Load()

				CaptureMessage("redirect probe")
				Flush(time.Second)

				is.Eq(primaryHits.Load(), int64(1))
				is.Eq(targetHits.Load(), int64(0))
				is.Eq(c.dropped.Load(), int64(1))
				c.breakerMu.Lock()
				failures := c.failures
				c.breakerMu.Unlock()
				is.Eq(failures, 1)
				is.True(!strings.Contains(logs.String(), target.location))
			})
		}
	}
}

func TestPerfSendDoesNotFollowRedirects(t *testing.T) {
	for _, status := range redirectStatuses {
		for _, target := range redirectTargets {
			t.Run(fmt.Sprintf("%d/%s", status, target.name), func(t *testing.T) {
				is := is.New(t)
				resetActive(t)
				logs := captureLogs(t)
				hc, primaryHits, targetHits := newRedirectHarness(t, status, target.location)

				err := Init(Options{
					DSN:        testDSN,
					HTTPClient: hc,
				})
				is.NoErr(err)
				c := active.Load()

				RecordRequest(Request{Route: "/redirect-probe", Status: 200, Duration: time.Millisecond})
				Flush(time.Second)

				is.True(primaryHits.Load() >= 1)
				is.Eq(targetHits.Load(), int64(0))
				is.Eq(DroppedRequests(), int64(1))
				ps := c.perf.Load()
				is.True(ps != nil)
				ps.breakerMu.Lock()
				failures := ps.failures
				ps.breakerMu.Unlock()
				is.True(failures >= 1)
				is.True(!strings.Contains(logs.String(), target.location))
			})
		}
	}
}

func TestDefaultHTTPClientDoesNotFollowRedirects(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	logs := captureLogs(t)

	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	location := target.URL + "/sink"
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusFound)
	}))
	defer primary.Close()

	dsn := "http://sek_test@" + primary.Listener.Addr().String() + "/errors/app/web"
	is.NoErr(Init(Options{DSN: dsn}))
	c := active.Load()

	CaptureMessage("default client redirect")
	RecordRequest(Request{Route: "/default-redirect", Status: 200})
	Flush(time.Second)

	is.Eq(targetHits.Load(), int64(0))
	is.Eq(c.dropped.Load(), int64(1))
	is.Eq(DroppedRequests(), int64(1))
	is.True(!strings.Contains(logs.String(), location))
}

func TestCallerHTTPClientCheckRedirectIgnoredAndUnchanged(t *testing.T) {
	is := is.New(t)
	resetActive(t)

	var callerPolicyCalled atomic.Bool
	callerPolicy := func(req *http.Request, via []*http.Request) error {
		callerPolicyCalled.Store(true)
		return nil
	}

	hc, _, targetHits := newRedirectHarness(t, http.StatusFound, "http://sockeye.example/sink")
	hc.CheckRedirect = callerPolicy

	is.NoErr(Init(Options{
		DSN:        testDSN,
		HTTPClient: hc,
	}))
	c := active.Load()

	CaptureMessage("caller client redirect")
	Flush(time.Second)

	is.Eq(targetHits.Load(), int64(0))
	is.Eq(c.dropped.Load(), int64(1))
	is.True(!callerPolicyCalled.Load())

	// Calling the caller's client policy directly proves Init did not
	// overwrite hc.CheckRedirect.
	is.NoErr(hc.CheckRedirect(nil, nil))
	is.True(callerPolicyCalled.Load())
}
