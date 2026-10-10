package speedtest

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package when a test leaves goroutines behind: a Run
// that returned must have stopped every stream, prober and queue.
func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }
