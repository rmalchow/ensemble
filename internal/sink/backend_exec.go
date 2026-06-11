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

// respawnThrottle bounds how often a write failure may respawn the player, so a
// persistently-failing tool (e.g. no audio device) can't fork-bomb at frame
// cadence. The first failure of an episode respawns immediately (lastSpawn zero).
const respawnThrottle = time.Second

// execBackend pipes canonical PCM (s16le 48k stereo) into a player subprocess.
type execBackend struct {
	cmd       *exec.Cmd
	in        io.WriteCloser // stdin pipe
	toolPath  string
	toolName  string
	toolArgs  []string
	log       *slog.Logger
	once      sync.Once
	mu        sync.Mutex
	closed    bool
	lastSpawn time.Time // last (re)spawn, for throttling respawn-on-write-failure
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
	if b.closed {
		return // shutting down: do not respawn a player (avoids the nil
		//        b.in deref Close() left behind, and zombie players)
	}
	if b.in != nil {
		_ = b.in.Close()
	}
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
		_, _ = b.cmd.Process.Wait()
	}
	b.cmd, b.in = nil, nil
	if err := b.spawnLocked(); err != nil {
		b.log.Warn("exec backend respawn after flush failed", "err", err)
		b.cmd, b.in = nil, nil
	}
}

func (b *execBackend) Write(frame []byte) error {
	if len(frame) != stream.FrameBytes {
		return fmt.Errorf("exec: frame %d bytes, want %d", len(frame), stream.FrameBytes)
	}
	// First attempt against the live pipe, unlocked (the player may apply
	// backpressure; holding b.mu across a blocking write would stall Close/Flush).
	b.mu.Lock()
	w := b.in
	b.mu.Unlock()
	if w != nil {
		if _, err := w.Write(frame); err == nil {
			return nil
		}
	}
	// Write failed (broken pipe → the player process died) or no pipe. Respawn the
	// player and retry once so playout self-heals instead of going silent forever.
	w = b.respawn()
	if w == nil {
		return fmt.Errorf("exec: player down")
	}
	if _, err := w.Write(frame); err != nil {
		return err
	}
	return nil
}

// respawn tears down the dead player and starts a fresh one, returning the new
// stdin pipe (or nil when throttled, closing, or the spawn failed). Throttled to
// respawnThrottle so a tool that dies on every start can't spin up processes at
// frame cadence. Also reaps the old (possibly self-exited) process, avoiding a
// zombie.
func (b *execBackend) respawn() io.WriteCloser {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	now := time.Now()
	if !b.lastSpawn.IsZero() && now.Sub(b.lastSpawn) < respawnThrottle {
		return nil // too soon: surface the error this frame, retry the next
	}
	b.lastSpawn = now
	if b.in != nil {
		_ = b.in.Close()
	}
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
		_, _ = b.cmd.Process.Wait()
	}
	b.cmd, b.in = nil, nil
	b.log.Warn("exec backend player died; respawning", "tool", b.toolName)
	if err := b.spawnLocked(); err != nil {
		b.log.Warn("exec backend respawn failed", "err", err)
		b.cmd, b.in = nil, nil
		return nil
	}
	return b.in
}

// Close closes stdin, waits with a short timeout, and kills the process if it
// hangs (D21 — exec backend gets a write deadline via process kill on Close).
func (b *execBackend) Close() error {
	var ret error
	b.once.Do(func() {
		b.mu.Lock()
		b.closed = true // stop any concurrent Flush from respawning
		in := b.in
		cmd := b.cmd
		b.in = nil
		b.cmd = nil
		b.mu.Unlock()
		if in != nil {
			_ = in.Close()
		}
		if cmd == nil {
			return
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			ret = err
		case <-time.After(time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			<-done
		}
	})
	return ret
}
