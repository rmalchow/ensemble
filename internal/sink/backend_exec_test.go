package sink

import (
	"log/slog"
	"testing"
)

// TestExecBackendFlushAfterCloseNoPanic pins the crash the user hit: Close()
// nils the stdin pipe but left b.cmd set, so a Disarm()→Flush() arriving
// during/after shutdown dereferenced a nil pipe (and respawned a zombie).
// Flush must be a safe no-op once the backend is closed.
func TestExecBackendFlushAfterCloseNoPanic(t *testing.T) {
	if _, _, ok := lookExecTool(); !ok {
		t.Skip("no exec player tool on PATH")
	}
	b, err := newExecBackend(slog.Default())
	if err != nil {
		t.Skipf("exec backend unavailable: %v", err)
	}
	_ = b.Close()
	// These must not panic and must not spawn a new player.
	b.Flush()
	b.Flush()
	if b.cmd != nil || b.in != nil {
		t.Fatalf("closed backend should hold no process: cmd=%v in=%v", b.cmd, b.in)
	}
}
