package errs_test

import (
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/croaky/is"

	"github.com/croaky/errs"
)

//go:noinline
func slogSiteA(l *slog.Logger, msg string) {
	l.Error(msg)
}

//go:noinline
func slogSiteB(l *slog.Logger, msg string) {
	l.Error(msg)
}

//go:noinline
func slogErrSiteA(l *slog.Logger, msg string, err error) {
	l.Error(msg, "err", err)
}

//go:noinline
func slogErrSiteB(l *slog.Logger, msg string, err error) {
	l.Error(msg, "err", err)
}

// TestSlogHandlerNoInAppFramesSeparatesCallSites verifies that when
// InAppPrefix matches no frame, SlogHandler records from two call
// sites use two sampling buckets, so a flood at one call site does
// not hold back whole events from the other.
func TestSlogHandlerNoInAppFramesSeparatesCallSites(t *testing.T) {
	cases := []struct {
		name  string
		siteA func(l *slog.Logger, msg string)
		siteB func(l *slog.Logger, msg string)
	}{
		{
			name:  "message",
			siteA: slogSiteA,
			siteB: slogSiteB,
		},
		{
			name: "exception",
			siteA: func(l *slog.Logger, msg string) {
				slogErrSiteA(l, msg, errors.New("boom"))
			},
			siteB: func(l *slog.Logger, msg string) {
				slogErrSiteB(l, msg, errors.New("other"))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			is := is.New(t)
			var (
				mu   sync.Mutex
				reqs []map[string]any
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var ev map[string]any
				_ = json.Unmarshal(body, &ev)
				mu.Lock()
				reqs = append(reqs, ev)
				mu.Unlock()
				w.WriteHeader(http.StatusAccepted)
			}))
			defer srv.Close()

			u, err := url.Parse(srv.URL)
			is.NoErr(err)
			dsn := "http://sek_test@" + u.Host + "/errors/app/web"
			fixedNow := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

			is.NoErr(errs.Init(errs.Options{
				DSN:         dsn,
				InAppPrefix: "example.com/no-match",
				Clock:       func() time.Time { return fixedNow },
			}))
			t.Cleanup(func() {
				errs.Flush(5 * time.Second)
				_ = errs.Init(errs.Options{})
			})

			l := slog.New(errs.SlogHandler(slog.NewTextHandler(io.Discard, nil)))

			const flood = 25
			for i := range flood {
				tc.siteA(l, "flood-"+strings.Repeat("a", i+1))
			}
			const other = 3
			for i := range other {
				tc.siteB(l, "other-"+strconv.Itoa(i))
			}
			errs.Flush(5 * time.Second)

			mu.Lock()
			got := append([]map[string]any(nil), reqs...)
			mu.Unlock()

			var (
				floodReqs  int
				floodTotal int
				otherWhole int
			)
			for _, ev := range got {
				tags, _ := ev["tags"].(map[string]any)
				tag, _ := tags["log"].(string)
				cnt, hasCount := ev["count"].(float64)
				switch {
				case strings.HasPrefix(tag, "flood-"):
					floodReqs++
					floodTotal += max(int(cnt), 1)
				case strings.HasPrefix(tag, "other-"):
					if !hasCount {
						otherWhole++
					}
				}
			}

			is.Eq(floodTotal, flood)
			is.True(floodReqs < flood)
			is.Eq(otherWhole, other)
		})
	}
}
