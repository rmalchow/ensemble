package sink

import (
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"ensemble/internal/stream"
)

// execTool is one player command in the auto-pick order (§8.5).
type execTool struct {
	name string
	args []string
}

// execTools is the auto-pick order. First found on $PATH wins.
var execTools = []execTool{
	{"pw-play", []string{"--raw", "--rate", "48000", "--channels", "2", "--format", "s16", "-"}},
	{"pw-cat", []string{"-p", "--raw", "--rate", "48000", "--channels", "2", "--format", "s16", "-"}},
	{"aplay", []string{"-q", "-f", "S16_LE", "-r", "48000", "-c", "2", "-t", "raw", "-"}},
	{"paplay", []string{"--raw", "--rate=48000", "--channels=2", "--format=s16le"}},
}

// lookExecTool returns the first execTool whose binary is on $PATH, or ok=false.
func lookExecTool() (execTool, string, bool) {
	for _, t := range execTools {
		if p, err := exec.LookPath(t.name); err == nil {
			return t, p, true
		}
	}
	return execTool{}, "", false
}

// execBackend pipes canonical PCM (s16le 48k stereo) into a player subprocess.
type execBackend struct {
	cmd      *exec.Cmd
	in       io.WriteCloser // stdin pipe
	toolPath string
	toolName string
	toolArgs []string
	log      *slog.Logger
	once     sync.Once
	mu       sync.Mutex
}

func newExecBackend(log *slog.Logger) (*execBackend, error) {
	tool, path, ok := lookExecTool()
	if !ok {
		return nil, fmt.Errorf("exec backend: no player tool on $PATH")
	}
	b := &execBackend{toolPath: path, toolName: tool.name, toolArgs: tool.args, log: log}
	if err := b.spawnLocked(); err != nil {
		return nil, err
	}
	return b, nil
}

// spawnLocked starts (or restarts) the player process. Caller holds b.mu (or
// is the constructor).
func (b *execBackend) spawnLocked() error {
	cmd := exec.Command(b.toolPath, b.toolArgs...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("exec backend: stdin pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("exec backend: start %s: %w", b.toolName, err)
	}
	b.cmd = cmd
	b.in = stdin
	b.log.Info("exec backend started", "tool", b.toolName)
	return nil
}

// Flush discards whatever the player buffered (contracts.Flusher): pipe
// players retain queued audio across a write stall and replay it when writes
// resume — stale audio at the next session's start. The only reliable flush
// for an external process is a respawn.
func (b *execBackend) Flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd != nil {
		_ = b.in.Close()
		_ = b.cmd.Process.Kill()
		_, _ = b.cmd.Process.Wait()
	}
	if err := b.spawnLocked(); err != nil {
		b.log.Warn("exec backend respawn after flush failed", "err", err)
		b.cmd = nil
		b.in = nil
	}
}

func (b *execBackend) Write(frame []byte) error {
	if len(frame) != stream.FrameBytes {
		return fmt.Errorf("exec: frame %d bytes, want %d", len(frame), stream.FrameBytes)
	}
	b.mu.Lock()
	w := b.in
	b.mu.Unlock()
	if w == nil {
		return fmt.Errorf("exec: closed")
	}
	_, err := w.Write(frame)
	return err
}

// Close closes stdin, waits with a short timeout, and kills the process if it
// hangs (D21 — exec backend gets a write deadline via process kill on Close).
func (b *execBackend) Close() error {
	var ret error
	b.once.Do(func() {
		b.mu.Lock()
		in := b.in
		b.in = nil
		b.mu.Unlock()
		if in != nil {
			_ = in.Close()
		}
		done := make(chan error, 1)
		go func() { done <- b.cmd.Wait() }()
		select {
		case err := <-done:
			ret = err
		case <-time.After(time.Second):
			_ = b.cmd.Process.Kill()
			<-done
		}
	})
	return ret
}
