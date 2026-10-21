package errs

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/croaky/is"
)

func slogTestClient(t *testing.T) *sockeyeServer {
	t.Helper()
	resetActive(t)
	srv := newSockeyeServer(t)
	t.Cleanup(srv.Close)
	is.New(t).NoErr(Init(Options{
		DSN:          testDSN,
		HTTPClient:   rewriteTransport(srv.URL),
		SyncDispatch: true,
	}))
	t.Cleanup(func() { Flush(time.Second) })
	return srv
}

func TestSlogHandlerSendsErrorRecordAsException(t *testing.T) {
	is := is.New(t)
	cs := slogTestClient(t)
	var buf bytes.Buffer
	log := slog.New(SlogHandler(slog.NewTextHandler(&buf, nil)))

	log.Error("fetch scores", "err", errors.New("kaboom"), "league", "nfl")

	is.True(strings.Contains(buf.String(), "fetch scores")) // the log line still prints
	ev := cs.firstEvent(t)
	tags, _ := ev["tags"].(map[string]any)
	is.Eq(tags["log"], "fetch scores")
	extra, _ := ev["extra"].(map[string]any)
	is.Eq(extra["league"], "nfl")
	exc := ev["exception"].(map[string]any)
	val := exc["values"].([]any)[0].(map[string]any)
	is.True(strings.HasPrefix(val["value"].(string), "fetch scores: kaboom"))
}

func TestSlogHandlerSendsRecordWithoutErrorAsMessage(t *testing.T) {
	is := is.New(t)
	cs := slogTestClient(t)
	log := slog.New(SlogHandler(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	log.Error("queue full", "depth", 9)

	ev := cs.firstEvent(t)
	is.Eq(ev["message"], "queue full")
}

func TestSlogHandlerIgnoresLowerLevels(t *testing.T) {
	is := is.New(t)
	srv := slogTestClient(t)
	log := slog.New(SlogHandler(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	log.Warn("slow feed")
	log.With("job", "ingest").Info("done")

	is.Eq(len(srv.requests()), 0)
}

func TestSlogHandlerKeepsAttrsAndGroups(t *testing.T) {
	is := is.New(t)
	cs := slogTestClient(t)
	var buf bytes.Buffer
	log := slog.New(SlogHandler(slog.NewTextHandler(&buf, nil))).With("job", "ingest").WithGroup("g")

	log.Error("failed", "n", 1)

	is.True(strings.Contains(buf.String(), "job=ingest"))
	is.True(strings.Contains(buf.String(), "g.n=1"))
	cs.firstEvent(t)
}

func TestSlogHandlerWithoutClientOnlyLogs(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	var buf bytes.Buffer
	log := slog.New(SlogHandler(slog.NewTextHandler(&buf, nil)))

	log.Error("no client", "err", errors.New("x"))

	is.True(strings.Contains(buf.String(), "no client"))
}
