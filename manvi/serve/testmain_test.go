package serve

import (
	"os"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/internal/storeguard"
)

// TestMain fails the package when a test leaves a dcstore child process behind.
func TestMain(m *testing.M) { os.Exit(storeguard.Run(m)) }
