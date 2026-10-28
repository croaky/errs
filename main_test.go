package errs

import (
	"os"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	zeroRetryDelays()
	perfEvery = 50 * time.Millisecond
	os.Exit(m.Run())
}
