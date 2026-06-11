package sink

import (
	"log/slog"
	"testing"
	"time"

	"ensemble/internal/stream"
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

// TestExecBackendRespawnsOnWriteFailure pins the recovery for the reported
// "backend write failed: write |1: broken pipe" case: the player subprocess dies
// and the backend must respawn it on the next write rather than going silent
// forever.
func TestExecBackendRespawnsOnWriteFailure(t *testing.T) {
	if _, _, ok := lookExecTool(); !ok {
		t.Skip("no exec player tool on PATH")
	}
	b, err := newExecBackend(slog.Default())
	if err != nil {
		t.Skipf("exec backend unavailable: %v", err)
	}
	defer b.Close()

	// Kill the player out from under the backend and reap it so its pipe read end
	// is closed — the next write hits a broken pipe, exactly like the field log.
	b.mu.Lock()
	proc := b.cmd.Process
	b.mu.Unlock()
	_ = proc.Kill()
	_, _ = proc.Wait()

	frame := make([]byte, stream.FrameBytes)
	var lastErr error
	for i := 0; i < 20; i++ {
		if lastErr = b.Write(frame); lastErr == nil {
			return // recovered: the backend respawned the player
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("backend never recovered after player death: %v", lastErr)
}
