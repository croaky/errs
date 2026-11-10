package errs

import (
	"bytes"
	"errors"
	"io"
	"log"
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
		DSN:        testDSN,
		HTTPClient: rewriteTransport(srv.URL),
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
	Flush(time.Second)

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
	Flush(time.Second)

	ev := cs.firstEvent(t)
	is.Eq(ev["message"], "queue full")
}

func TestSlogHandlerIgnoresLowerLevels(t *testing.T) {
	is := is.New(t)
	srv := slogTestClient(t)
	log := slog.New(SlogHandler(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	log.Warn("slow feed")
	log.With("job", "ingest").Info("done")
	Flush(time.Second)

	is.Eq(len(srv.requests()), 0)
}

func TestSlogHandlerKeepsAttrsAndGroups(t *testing.T) {
	is := is.New(t)
	cs := slogTestClient(t)
	var buf bytes.Buffer
	log := slog.New(SlogHandler(slog.NewTextHandler(&buf, nil))).With("job", "ingest").WithGroup("g")

	log.Error("failed", "n", 1)
	Flush(time.Second)

	is.True(strings.Contains(buf.String(), "job=ingest"))
	is.True(strings.Contains(buf.String(), "g.n=1"))
	cs.firstEvent(t)
}

// TestSendFailureDoesNotReenterSlog: a host bridges the global log
// logger into SlogHandler at error level, and sockeye refuses each
// send. The failure line that errs writes does not become a new event,
// and a later log.Print returns.
func TestSendFailureDoesNotReenterSlog(t *testing.T) {
	is := is.New(t)
	srv := slogTestClient(t)
	srv.status.Store(400)

	oldSlog, oldOut, oldFlags := slog.Default(), log.Writer(), log.Flags()
	t.Cleanup(func() {
		slog.SetDefault(oldSlog)
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
		slog.SetLogLoggerLevel(slog.LevelInfo)
	})
	slog.SetDefault(slog.New(SlogHandler(slog.NewTextHandler(io.Discard, nil))))
	slog.SetLogLoggerLevel(slog.LevelError)

	CaptureMessage("first")
	done := make(chan struct{})
	go func() {
		log.Print("after")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("log.Print blocked after a failed send")
	}
	// The worker sends "first" and then "after". A failure line that
	// came back as an event would sit in the queue between them, and
	// Flush would send it too.
	for deadline := time.Now().Add(5 * time.Second); len(srv.requests()) < 2 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	Flush(5 * time.Second)

	var msgs []any
	for _, r := range srv.requests() {
		msgs = append(msgs, r.ev["message"])
	}
	is.Eq(msgs, []any{"first", "after"})
}

func TestSlogHandlerWithoutClientOnlyLogs(t *testing.T) {
	is := is.New(t)
	resetActive(t)
	var buf bytes.Buffer
	log := slog.New(SlogHandler(slog.NewTextHandler(&buf, nil)))

	log.Error("no client", "err", errors.New("x"))

	is.True(strings.Contains(buf.String(), "no client"))
}
