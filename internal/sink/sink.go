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

	clock           contracts.Clock
	out             contracts.Backend
	delay           contracts.DelayReporter // out asserted to DelayReporter, or nil
	flush           contracts.Flusher       // out asserted to Flusher, or nil
	gain            *gainStage
	bufferNs        int64
	delayOffsetNs   int64 // output-delay calibration (D36, acoustic/room); subtracted from the deadline
	equalizeDelayNs int64 // master-driven cross-room device-buffer equalization (D65); ADDED to the deadline (delays a faster room to match the slowest)
	deviceLatencyNs int64 // backend's configured output latency (D63); subtracted from the deadline
	watchdog        time.Duration
	restart         RestartFunc
	now             func() int64
	log             *slog.Logger

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
	if sc.QueueTau <= 0 {
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
	if fl, ok := cfg.Backend.(contracts.Flusher); ok {
		p.flush = fl
	}
	if dr, ok := cfg.Backend.(contracts.DelayReporter); ok {
		p.delay = dr
	}
	// The backend's configured output latency (ALSA) is recorded for telemetry /
	// the upcoming per-node device-latency compensation (D63/D64), but is NOT yet
	// subtracted from the deadline (see the loop comment).
	if lr, ok := cfg.Backend.(contracts.LatencyReporter); ok {
		p.deviceLatencyNs = lr.ConfiguredLatencyNs()
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
	if p.gain.currentTarget() == g {
		return // unchanged: silence the per-PATCH flood from a dragging slider
	}
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

// SetEqualizeDelay sets the master-driven cross-room equalization delay in
// nanoseconds (D65). This is a SEPARATE component from the D36 acoustic offset
// (SetDelayOffset): the deadline adds it, so the master can delay a faster room to
// match the slowest WITHOUT clobbering the node's own acoustic calibration. Clamped
// to ≥0 (it only ever delays). Re-anchors exactly like SetDelayOffset; a no-op when
// the value is unchanged so the master's 1 Hz re-assert never re-anchors (an audible
// glitch) — only a real change does.
func (p *Playout) SetEqualizeDelay(nanos int64) {
	if nanos < 0 {
		nanos = 0
	}
	nanos = clampDelayNs(nanos)
	p.mu.Lock()
	if p.closed || p.equalizeDelayNs == nanos {
		p.mu.Unlock()
		return
	}
	p.equalizeDelayNs = nanos
	p.jb.reset()
	p.originSet = false
	p.stats.Buffered = 0
	restart := p.restart
	eqMs := p.equalizeDelayNs / 1_000_000
	p.mu.Unlock()
	p.log.Info("equalize delay changed; re-anchoring", "equalizeMs", eqMs, "willRestart", restart != nil)
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
	// D63: the new backend may have a different configured device latency (e.g.
	// swapping an alsa device for the null backend) — re-read it so the deadline
	// pre-roll tracks the live backend.
	if lr, ok := nb.(contracts.LatencyReporter); ok {
		p.deviceLatencyNs = lr.ConfiguredLatencyNs()
	} else {
		p.deviceLatencyNs = 0
	}
	p.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	p.log.Info("output backend swapped")
}

// PreferOutputDevice forwards a device override to the LIVE output backend when
// it supports re-ordering its failover chain (the resilient backend) — the UI
// device selection (D37). Always targets the current backend, so it is correct
// across a SwapBackend (e.g. a playback disable/enable cycle). Returns false when
// the live backend ignores devices (nothing to do; persist+replicate still apply).
func (p *Playout) PreferOutputDevice(device string) bool {
	p.mu.Lock()
	out := p.out
	p.mu.Unlock()
	if sel, ok := out.(interface{ SetPreferred(string) }); ok {
		sel.SetPreferred(device)
		return true
	}
	return false
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
	if p.flush != nil {
		// Session over: drop whatever the device/player retains, or it
		// audibly replays at the next session's start (user-reported on
		// leave+rejoin).
		p.flush.Flush()
	}
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
	s.SamplesInjected, s.SamplesDropped = p.rs.sampleStats() // grounded resample counts
	s.Calibrated = p.servo.setpointNs() > 0                  // D65: setpoint frozen → stable device-queue depth
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
		// Deadline: pts + buffer − acoustic calibration − configured device latency.
		// We subtract the backend's CONFIGURED device latency (a known constant), NOT
		// the LIVE device queue: subtracting the live queue is the unstable feedback
		// loop the old design avoided (writing earlier grows the queue, which moves
		// deadlines earlier…). Subtracting the constant pre-rolls the device to its
		// full buffer (xrun cushion) and makes speaker_time = MasterToLocal(pts+buffer)
		// on EVERY node regardless of its device latency — so heterogeneous DACs and
		// Deadline = pts + buffer − acoustic calibration. Deliberately NOT minus the
		// device queue: subtracting the live queue is a positive-feedback loop (write
		// earlier → queue grows → write earlier…), and subtracting a CALIBRATED
		// constant that switches in mid-session jolts the schedule. Compensating the
		// per-node device-buffer DIFFERENCE for tight cross-node sync is the next,
		// data-driven step (D64); the room-sync telemetry below measures it first.
		// + equalizeDelayNs: master-driven cross-room equalization (D65). The master
		// computes maxSetpoint−ownSetpoint across rooms and pushes it here; ADDING it
		// delays a faster room (smaller device buffer) so its speaker_time matches the
		// slowest room. Always ≥0, so it only ever adds latency — never pre-rolls
		// harder than the device buffer allows (the failure mode D63's revert avoided).
		target := pts + p.bufferNs - p.delayOffsetNs + p.equalizeDelayNs
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
			if dok {
				p.stats.DeviceDelayNs = dDelay // D63 telemetry: surfaced via Stats()→STATUS
			}
			ppm := p.servo.observe(p.consumed, mNow, dDelay, dok)
			p.rs.setRate(ppm)
			p.stats.RatePPM = ppm
			// phaseErr: the device queue's deviation from its calibrated setpoint
			// (dDelay − setpoint), in ns — the per-node playout phase the master diffs
			// across rooms to see skew. Small in lock; a sustained value is drift the
			// gentle servo is correcting. (D64 telemetry.)
			if sp := p.servo.setpointNs(); sp > 0 {
				p.stats.PhaseErrNs = dDelay - sp
			}
			if now := p.now(); now-p.lastServoLog > 1_000_000_000 {
				p.lastServoLog = now
				p.log.Debug("stats",
					"outPPM", int64(ppm),
					"phaseErrMs", p.stats.PhaseErrNs/1_000_000,
					"deviceDelayMs", dDelay/1_000_000, "delayOK", dok,
					"setpointMs", p.servo.setpointNs()/1_000_000,
					"queueErrSamples", int64(p.servo.queueErr),
					"buffered", p.stats.Buffered,
					"played", p.stats.Played, "silence", p.stats.Silence, "late", p.stats.LateDrop)
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
	if p.flush != nil {
		p.flush.Flush()
	}
	p.log.Warn("playout still starved after RESTART, disarming")
	return nil
}
