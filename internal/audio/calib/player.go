package calib

import (
	"context"
	"errors"
	"sync"
	"time"

	sink "gitlab.rand0m.me/ruben/go/ensemble/internal/audio/sink"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/group"
)

// player.go is the LOCAL executor: it plays the built-in Signal on THIS node,
// sample-aligned to the group timeline, invoked by the cmd-side fan-out (directly
// on the receiving node, or via the proxied per-node handler on a remote node).
//
// It is NOT the steady-state render/drift loop (that is internal/audio/render,
// P4.9). It borrows only the render-loop drain idiom — "fill frames, then
// sink.Write blocks for backpressure = playout pacing" — but has no resampler and
// no drift PI loop: the signal is emitted at unity directly because the sink runs
// at the canonical rate the Signal was generated for.

var (
	// ErrBusy is returned when Play is called while a previous Play is still running.
	ErrBusy = errors.New("calibrate: already playing")
	// ErrNoSink is returned when the node has no audio sink (Render=false).
	ErrNoSink = errors.New("calibrate: node has no audio sink (Render=false)")
)

// chunkFrames is the per-Write drain size (10 ms @ 48k). Small enough that Stop /
// ctx cancellation is responsive, large enough that the per-call overhead is
// negligible on the Pi-class target. The single fill buffer is reused across the
// whole Play, so steady-state playback is allocation-free.
const chunkFrames = 480

// alignPoll is how often Play re-reads the timeline while waiting for startSample
// to arrive (the priming wait, §5.2 step 4). 1 ms keeps the start tight without
// busy-spinning a Cortex-A53 core.
const alignPoll = time.Millisecond

// CalibratePlayer plays the built-in Signal on this node, sample-aligned to the
// group timeline. Concurrent Play is rejected; Stop ends an in-flight Play early.
type CalibratePlayer struct {
	sig *Signal
	snk sink.AudioSink
	tl  group.Timeline // may be nil for a solo / no-timeline node

	mu      sync.Mutex
	running bool
	stop    chan struct{} // closed by Stop to end the current Play
}

// NewCalibratePlayer binds the (already Start()ed) sink and the group Timeline
// used for alignment. tl may be nil for a solo/no-timeline node, in which case
// Play aligns to the node's own immediate now (still deterministic from the
// period, just not cross-node aligned). snk==nil models a Render=false node:
// Play then returns ErrNoSink.
func NewCalibratePlayer(sig *Signal, snk sink.AudioSink, tl group.Timeline) *CalibratePlayer {
	return &CalibratePlayer{sig: sig, snk: snk, tl: tl}
}

// Play emits the signal for durationSec, beginning at group sample startSample. It
// waits until the local Timeline reaches startSample (the priming lead), then
// busy-fills the sink — sink.Write blocks = playout pacing — for durationSec*Rate
// frames, wrapping the period via Signal.Fill indexed by the GROUP sample so the
// click lands on the same period boundary at the same group instant on every node.
// It returns when the duration elapses, ctx is cancelled, or Stop() is called.
// Concurrent Play => ErrBusy; a nil sink (Render=false) => ErrNoSink.
func (c *CalibratePlayer) Play(ctx context.Context, startSample int64, durationSec int) error {
	if c.snk == nil {
		return ErrNoSink
	}
	if durationSec <= 0 {
		durationSec = 1
	}

	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return ErrBusy
	}
	c.running = true
	stop := make(chan struct{})
	c.stop = stop
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.running = false
		c.stop = nil
		c.mu.Unlock()
	}()

	// Phase 1: wait until the local timeline reaches startSample so playout is
	// primed and cross-node aligned. A nil/unsynced timeline starts immediately
	// from its own now (solo path).
	cur := startSample
	if c.tl != nil {
		if s, ok := c.waitForStart(ctx, stop, startSample); ok {
			cur = s
		} else {
			// ctx cancelled / stopped before start arrived, or never synced. If the
			// context/stop fired, honour it; otherwise (never-synced) start now.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-stop:
				return nil
			default:
				cur = startSample
			}
		}
	}

	// Phase 2: drain durationSec*Rate frames, advancing the GROUP sample by the
	// frames written each chunk so Fill always indexes the group-relative period
	// position (self-correcting to the period the timeline dictates).
	total := int64(durationSec) * int64(c.sig.Rate())
	buf := make([]float32, chunkFrames*c.sig.Channels())
	var done int64
	for done < total {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stop:
			return nil
		default:
		}

		n := total - done
		if n > chunkFrames {
			n = chunkFrames
		}
		out := buf[:n*int64(c.sig.Channels())]
		c.sig.Fill(out, cur)

		if err := writeAll(c.snk, out); err != nil {
			return err
		}
		done += n
		cur += n
	}
	return nil
}

// waitForStart blocks until the local timeline's NowSample reaches startSample,
// returning the (group) sample to begin filling from. ok=false if ctx/stop fired
// or the timeline never reports ok (caller decides the fallback).
func (c *CalibratePlayer) waitForStart(ctx context.Context, stop <-chan struct{}, startSample int64) (int64, bool) {
	if s, playing, ok := c.tl.NowSample(); ok {
		_ = playing
		if s >= startSample {
			return s, true
		}
	}
	t := time.NewTicker(alignPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0, false
		case <-stop:
			return 0, false
		case <-t.C:
			s, _, ok := c.tl.NowSample()
			if ok && s >= startSample {
				return s, true
			}
		}
	}
}

// Stop ends an in-flight Play early (idempotent). It signals the running Play to
// return; a Play that is not running is a no-op.
func (c *CalibratePlayer) Stop() {
	c.mu.Lock()
	if c.stop != nil {
		close(c.stop)
		c.stop = nil
	}
	c.mu.Unlock()
}

// writeAll pushes the whole buffer to the sink, looping on a short write (the
// sink may consume fewer samples per Write under backpressure). The blocking
// Write IS the playout pacing.
func writeAll(snk sink.AudioSink, buf []float32) error {
	for len(buf) > 0 {
		n, err := snk.Write(buf)
		if err != nil {
			return err
		}
		if n <= 0 {
			return errors.New("calibrate: sink made no progress")
		}
		buf = buf[n:]
	}
	return nil
}
