// Package sink is the playout pipeline: jitter buffer, pts-driven scheduler,
// continuous rate servo + 4-tap resampler, live volume gain, and the output
// backend registry (alsa/exec/null/file) (§8.5). Piece E.
package sink

import (
	"ensemble/internal/clock"
	"log/slog"
	"sync"
	"time"

	"ensemble/internal/contracts"
	"ensemble/internal/stream"
)

// monotoNow returns the local monotonic clock in nanoseconds — clock.MonoNow,
// the SAME clock the follower measures offsets against. MasterToLocal output
// is only comparable to this clock; wall time (UnixNano) is off by the
// inter-process start-delta and is additionally non-monotonic under NTP steps.
func monotoNow() int64 { return clock.MonoNow() }

// Playout is the per-node sink: jitter buffer → scheduler → resampler → gain →
// backend, with the continuous rate servo and the starvation watchdog.
// Implements contracts.Sink. One scheduler goroutine, one mutex.
type Playout struct {
	mu           sync.Mutex
	jb           *jitterBuffer
	servo        *rateServo
	rs           *resampler
	gen          uint32
	armed        bool
	closed       bool
	toneBusy     bool // a TestTone writer goroutine is active
	stats        contracts.SinkStats
	lastPkt      int64 // local-ns of most recent accepted Push (watchdog)
	lastServoLog int64 // local ns of the last 1Hz servo debug line

	// session servo accounting
	originSeq  uint64
	originPTS  int64
	originSet  bool
	consumed   int64 // cumulative output samples/ch written to the backend
	restartHit bool  // RESTART already fired this starvation episode

	clock         contracts.Clock
	out           contracts.Backend
	delay         contracts.DelayReporter // out asserted to DelayReporter, or nil
	gain          *gainStage
	bufferNs      int64
	delayOffsetNs int64 // output-delay calibration (D36); subtracted from the deadline
	watchdog      time.Duration
	restart       RestartFunc
	now           func() int64
	log           *slog.Logger

	silence []byte
	wake    chan struct{}
	done    chan struct{}
	wg      sync.WaitGroup
}

// New builds a Playout and starts its scheduler goroutine (idle until the first
// Reset). cfg.Backend and cfg.Clock must be non-nil.
func New(cfg Config) *Playout {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("comp", "sink")

	capacity := cfg.Capacity
	if capacity <= 0 {
		capacity = defaultCapacity
	}
	watchdog := cfg.Watchdog
	if watchdog <= 0 {
		watchdog = defaultWatchdog
	}
	bufferMs := cfg.BufferMs
	if bufferMs <= 0 {
		bufferMs = contracts.DefaultBufferMs
	}
	nowFn := cfg.now
	if nowFn == nil {
		nowFn = monotoNow
	}
	sc := cfg.servoCfg
	if sc.Window <= 0 {
		sc = defaultServoConfig()
	}

	p := &Playout{
		jb:            newJitterBuffer(capacity),
		servo:         newRateServo(sc),
		rs:            newResampler(),
		gain:          newGainStage(cfg.Volume),
		clock:         cfg.Clock,
		out:           cfg.Backend,
		bufferNs:      int64(bufferMs) * 1_000_000,
		delayOffsetNs: clampDelayNs(int64(cfg.OutputDelayMs) * 1_000_000),
		watchdog:      watchdog,
		restart:       cfg.Restart,
		now:           nowFn,
		log:           log,
		silence:       make([]byte, stream.FrameBytes),
		wake:          make(chan struct{}, 1),
		done:          make(chan struct{}),
	}
	if dr, ok := cfg.Backend.(contracts.DelayReporter); ok {
		p.delay = dr
	}
	p.wg.Add(1)
	go p.loop()
	return p
}

// Push enqueues a frame for playout (contracts.Sink). Non-blocking; drops+counts
// stale-gen / late frames; copies payload; signals the scheduler.
func (p *Playout) Push(gen uint32, seq uint64, pts int64, payload []byte) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	if gen != p.gen {
		p.stats.StaleGen++
		p.mu.Unlock()
		return
	}
	if !p.armed {
		p.stats.StaleGen++
		p.mu.Unlock()
		return
	}
	if !p.originSet {
		p.jb.setOrigin(seq)
		p.originSeq = seq
		p.originPTS = pts
		p.originSet = true
	}
	if !p.jb.insert(seq, pts, payload) {
		p.stats.LateDrop++
	} else {
		p.lastPkt = p.now()
	}
	p.stats.Buffered = p.jb.len()
	p.mu.Unlock()
	p.signal()
}

// Reset arms the sink for a new generation: discards queued frames, resets servo
// + resampler, sets gen, clears per-session counters, re-establishes the seq/pts
// origin on the next Push.
func (p *Playout) Reset(gen uint32) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.jb.reset()
	p.servo.reset()
	p.rs.reset()
	p.gen = gen
	p.stats.Played = 0
	p.stats.Silence = 0
	p.stats.LateDrop = 0
	p.stats.StaleGen = 0
	p.stats.RatePPM = 0
	p.stats.Buffered = 0
	p.consumed = 0
	p.restartHit = false
	p.originSet = false
	p.armed = true
	p.lastPkt = p.now()
	bufMs := p.bufferNs / 1_000_000
	delayMs := p.delayOffsetNs / 1_000_000
	p.mu.Unlock()
	p.log.Info("session armed", "gen", gen, "bufferMs", bufMs, "delayOffsetMs", delayMs)
	p.signal()
}

// SetBufferMs updates the playout lead live (D21/D23); takes effect on the next
// scheduled slot.
func (p *Playout) SetBufferMs(ms int) {
	if ms <= 0 {
		ms = contracts.DefaultBufferMs
	}
	p.mu.Lock()
	p.bufferNs = int64(ms) * 1_000_000
	p.mu.Unlock()
}

// SetGain sets the live software volume (0.0–1.0, D35; contracts.Sink).
// Lock-free: stores the gain stage's atomic target; the scheduler ramps over the
// next frame. Safe from any goroutine, no restart, applies on every backend.
func (p *Playout) SetGain(g float64) {
	p.gain.setTarget(g)
	p.log.Info("gain changed", "volume", g)
}

// SetDelayOffset sets the node's output-delay calibration in nanoseconds (D36;
// contracts.Sink). Under the mutex it stores the clamped offset, discards
// buffered frames (re-armed on the next Push), and fires the RestartFunc the
// starvation watchdog uses so G's subscriber issues a wire RESTART and the
// source burst re-primes under the new anchor.
func (p *Playout) SetDelayOffset(nanos int64) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.delayOffsetNs = clampDelayNs(nanos)
	p.jb.reset()
	p.originSet = false
	p.stats.Buffered = 0
	restart := p.restart
	offMs := p.delayOffsetNs / 1_000_000
	p.mu.Unlock()
	p.log.Info("delay offset changed; re-anchoring", "delayOffsetMs", offMs, "willRestart", restart != nil)
	if restart != nil {
		restart()
	}
}

// SwapBackend replaces the live output backend (D37, §8.5): used when the node's
// selected output device changes. Under the mutex it closes the old backend, sets
// the new one (re-asserting DelayReporter), and logs. A brief audio blip is
// acceptable; the session/scheduler is NOT restarted. No-op (closing nb) if the
// sink is already closed.
func (p *Playout) SwapBackend(nb contracts.Backend) {
	if nb == nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = nb.Close()
		return
	}
	old := p.out
	p.out = nb
	if dr, ok := nb.(contracts.DelayReporter); ok {
		p.delay = dr
	} else {
		p.delay = nil
	}
	p.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	p.log.Info("output backend swapped")
}

// Disarm cleanly ends the local session (contracts.Sink): group went idle or
// the session stopped. Discards buffered frames and stops the scheduler with
// no starvation warnings. Idempotent; Reset re-arms.
func (p *Playout) Disarm() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || !p.armed {
		return
	}
	p.armed = false
	p.jb.reset()
	p.servo.reset()
	p.rs.reset()
	p.stats.Buffered = 0
	p.log.Info("playout disarmed (session ended)", "gen", p.gen)
	p.signal()
}

// ArmedGen reports the generation the sink is currently armed for and whether it
// is armed at all. The member-side deliver path uses it to decide when to re-arm:
// a frame whose gen differs from the armed gen (or arriving while disarmed) means
// a new/replaced session, so the next frame must Reset the sink to its gen — the
// deliver closure cannot rely on its own cached gen because repointLocked (group
// engine) may have Reset the sink to a guessed gen out from under it on a
// (re)subscribe (the late-join stale-gen drop bug).
func (p *Playout) ArmedGen() (gen uint32, armed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gen, p.armed
}

// Stats snapshots playout counters (contracts.Sink). Synced is read live from
// the clock; RatePPM is the servo's current correction; Buffered is jb.len().
func (p *Playout) Stats() contracts.SinkStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stats
	s.Buffered = p.jb.len()
	s.RatePPM = p.servo.ratePPM()
	_, ok := p.clock.MasterNow()
	s.Synced = ok
	return s
}

// Close stops the scheduler and closes the backend (contracts.Sink). Idempotent.
func (p *Playout) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	close(p.done)
	p.mu.Unlock()
	p.wg.Wait()
	return p.out.Close()
}

// signal pokes the scheduler (non-blocking).
func (p *Playout) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// clampDelayNs clamps an output-delay offset to ±maxDelayMs.
func clampDelayNs(nanos int64) int64 {
	max := int64(maxDelayMs) * 1_000_000
	if nanos > max {
		return max
	}
	if nanos < -max {
		return -max
	}
	return nanos
}

// slotPTS returns the master-clock PTS for a seq, based on the session origin.
func (p *Playout) slotPTS(seq uint64) int64 {
	return p.originPTS + int64(seq-p.originSeq)*stream.FrameNanos
}

// loop is the single scheduler goroutine. It plays one output frame per device
// write, in seq order, sleeping until each slot's deadline.
func (p *Playout) loop() {
	defer p.wg.Done()
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return
		}
		// Starvation watchdog (§3.7): fires whether the loop is idle OR busy
		// emitting silence into a drained buffer. fireRestart is called after
		// the unlock; once disarmed the loop falls into waitIdle below.
		fireRestart := p.checkStarvationLocked()
		if !p.armed || !p.jb.hasPending() {
			p.mu.Unlock()
			if fireRestart != nil {
				fireRestart()
			}
			if p.waitIdle() {
				return
			}
			continue
		}
		if fireRestart != nil {
			p.mu.Unlock()
			fireRestart()
			continue
		}

		seq := p.jb.nextSeq
		pts := p.slotPTS(seq)
		target := pts + p.bufferNs - p.delayOffsetNs
		local, ok := p.clock.MasterToLocal(target)
		if !ok {
			// unsynced gate (§7)
			p.mu.Unlock()
			if p.sleepInterruptible(5 * time.Millisecond) {
				return
			}
			continue
		}

		// servo update (every slot)
		if mNow, mok := p.clock.MasterNow(); mok {
			var dDelay int64
			dok := false
			if p.delay != nil {
				dDelay, dok = p.delay.DeviceDelay()
			}
			ppm := p.servo.observe(p.consumed, mNow, dDelay, dok)
			p.rs.setRate(ppm)
			p.stats.RatePPM = ppm
			if now := p.now(); now-p.lastServoLog > 1_000_000_000 {
				p.lastServoLog = now
				p.log.Debug("servo",
					"skewPPM", int64(p.servo.lastSkew), "integ", int64(p.servo.integ),
					"outPPM", int64(ppm), "got", int64(p.servo.lastGot),
					"want", int64(p.servo.lastWant), "consumed", p.consumed,
					"deviceDelayNs", dDelay, "delayOK", dok, "buffered", p.stats.Buffered)
			}
		}

		s := p.jb.pop(seq)
		curGen := p.gen
		p.mu.Unlock()

		// sleep until the coarse deadline
		if d := local - p.now(); d > 0 {
			if p.sleepInterruptible(time.Duration(d)) {
				return
			}
		}

		// Re-acquire the lock: a Reset/SetDelayOffset/disarm may have fired during
		// the sleep, resetting the resampler and buffer. If so, abandon this slot
		// (its session is gone) and re-enter the loop. The resampler + gain run
		// UNDER the lock (pure CPU, fast) so they never race rs.reset(); only the
		// blocking backend Write happens unlocked (D21).
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return
		}
		if !p.armed || p.gen != curGen {
			p.mu.Unlock()
			continue // session changed under us; drop this stale slot
		}

		late := p.now() > local+stream.FrameNanos
		if late {
			// A late slot must NOT consume device time: writing silence for it
			// would delay every later frame by one frame duration, so a backlog
			// (e.g. the unsynced hold at session start) could never drain and
			// playout would be late FOREVER, dropping every live frame. Count
			// and skip instantly; the loop re-evaluates the next slot at once.
			if s != nil {
				p.stats.LateDrop++
			}
			p.jb.advance()
			p.stats.Buffered = p.jb.len()
			p.mu.Unlock()
			continue
		}

		var in []byte
		played := false
		if s != nil {
			in = s.payload
			played = true
		} else {
			// Gap at its proper deadline: real silence keeps device cadence.
			p.stats.Silence++
			in = p.silence
		}

		out := p.rs.process(in)
		p.gain.apply(out)
		p.consumed += stream.FrameSamples
		p.jb.advance()
		p.stats.Buffered = p.jb.len()
		p.mu.Unlock()

		// blocking backend write happens unlocked (it may block on the device)
		if err := p.out.Write(out); err != nil {
			p.log.Warn("backend write failed", "err", err)
		} else if played {
			p.mu.Lock()
			p.stats.Played++
			p.mu.Unlock()
		}
	}
}

// waitIdle blocks while disarmed/empty until a wake, done, or a watchdog tick.
// On the tick it returns so the loop re-runs its inline starvation check
// (§3.7). Returns true if Close was signaled.
func (p *Playout) waitIdle() (done bool) {
	timer := time.NewTimer(p.watchdog)
	defer timer.Stop()
	select {
	case <-p.wake:
		return false
	case <-p.done:
		return true
	case <-timer.C:
		return false
	}
}

// sleepInterruptible sleeps for d unless Close fires first. Returns true on Close.
func (p *Playout) sleepInterruptible(d time.Duration) (done bool) {
	if d <= 0 {
		select {
		case <-p.done:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-p.done:
		return true
	case <-timer.C:
		return false
	}
}

// checkStarvationLocked implements the 2 s watchdog (§3.7). The caller holds the
// mutex. It returns a non-nil func to invoke (after unlocking) when a RESTART
// must be fired; nil otherwise. On the second starvation interval it disarms.
func (p *Playout) checkStarvationLocked() (fireRestart func()) {
	if !p.armed || p.now()-p.lastPkt < int64(p.watchdog) {
		return nil
	}
	if !p.restartHit {
		p.restartHit = true
		// Advance lastPkt so the next watchdog interval is measured from now;
		// the second interval of continued silence triggers disarm.
		p.lastPkt = p.now()
		p.log.Warn("playout starved, requesting RESTART")
		restart := p.restart
		if restart == nil {
			return nil
		}
		return restart
	}
	// Second starvation interval after RESTART with no fresh frames: disarm.
	p.armed = false
	p.jb.reset()
	p.servo.reset()
	p.rs.reset()
	p.stats.Buffered = 0
	p.log.Warn("playout still starved after RESTART, disarming")
	return nil
}
