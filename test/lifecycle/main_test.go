package lifecycle_test

import (
	"os"
	"testing"
	"time"
)

// TestMain fixes the local zone to UTC, so no test depends on the machine zone.
func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}
