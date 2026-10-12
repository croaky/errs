package errs

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	zeroRetryDelays()
	os.Exit(m.Run())
}
