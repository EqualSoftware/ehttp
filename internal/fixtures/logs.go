package fixtures

import (
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// CaptureLogs swaps the global zap logger. Tests that call it are serial.
func CaptureLogs(t *testing.T) *observer.ObservedLogs {
	t.Helper()

	core, logs := observer.New(zapcore.DebugLevel)
	restore := zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(restore)

	return logs
}
