package sink

import (
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"ensemble/internal/stream"
)

// fakeClock is a wall-clock-backed master/local clock: master == local ==
// nanoseconds since `base`, gated by `synced`. Tying it to the wall clock means
// the scheduler's real time.Sleep deadline-waits align with master time, so
// frames play at their deadlines rather than all arriving "late".
type fakeClock struct {
	mu     sync.Mutex
	base   time.Time
	synced bool
}

func newFakeClock(synced bool) *fakeClock {
	return &fakeClock{base: time.Now(), synced: synced}
}

func (c *fakeClock) setSynced(s bool) {
	c.mu.Lock()
	c.synced = s
	c.mu.Unlock()
}

func (c *fakeClock) nowNs() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return int64(time.Since(c.base))
}

func (c *fakeClock) MasterNow() (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return int64(time.Since(c.base)), c.synced
}

func (c *fakeClock) MasterToLocal(m int64) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return m, c.synced // local == master
}

func (c *fakeClock) LocalToMaster(l int64) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return l, c.synced
}

// recBackend records every written frame. Optionally a DelayReporter.
type recBackend struct {
	mu       sync.Mutex
	frames   [][]byte
	delayNs  int64
	hasDelay bool
}

func (b *recBackend) Write(f []byte) error {
	b.mu.Lock()
	cp := make([]byte, len(f))
	copy(cp, f)
	b.frames = append(b.frames, cp)
	b.mu.Unlock()
	return nil
}

func (b *recBackend) Close() error { return nil }

func (b *recBackend) DeviceDelay() (int64, bool) {
	if !b.hasDelay {
		return 0, false
	}
	return b.delayNs, true
}

func (b *recBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.frames)
}

func (b *recBackend) at(i int) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.frames[i]
}

// audioFrame builds a frame whose every sample equals v.
func audioFrame(v int16) []byte {
	f := make([]byte, stream.FrameBytes)
	for i := 0; i < stream.FrameSamples*stream.Channels; i++ {
		binary.LittleEndian.PutUint16(f[i*2:i*2+2], uint16(v))
	}
	return f
}

// newTestPlayout wires a Playout against the wall-clock fake clock and a backend.
// `now` is the same wall-clock source as the master clock, so deadlines line up.
func newTestPlayout(t *testing.T, clk *fakeClock, be interface {
	Write([]byte) error
	Close() error
}, restart RestartFunc) *Playout {
	t.Helper()
	p := New(Config{
		Backend:  be,
		Clock:    clk,
		BufferMs: 150,
		Restart:  restart,
		Volume:   1.0,
		Watchdog: 2 * time.Second,
		now:      clk.nowNs,
		servoCfg: fastServoCfg(),
	})
	t.Cleanup(func() { p.Close() })
	return p
}

// drainUntil polls cond up to ~3 s of wall time, yielding to the scheduler.
func drainUntil(t *testing.T, cond func() bool) bool {
	t.Helper()
	for i := 0; i < 3000; i++ {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

// pushRun pushes frames seq..seq+count-1 with pts anchored a little ahead of the
// current master time so their deadlines (pts + bufferMs) fall in the near
// future and play at cadence (not late).
func pushRun(p *Playout, clk *fakeClock, gen uint32, seqs []uint64) {
	base := clk.nowNs() + int64(10*time.Millisecond)
	for _, s := range seqs {
		pts := base + int64(s)*stream.FrameNanos
		p.Push(gen, s, pts, audioFrame(int16(1000+s)))
	}
}

func TestPlayoutBasicInOrder(t *testing.T) {
	clk := newFakeClock(true)
	be := &recBackend{}
	p := newTestPlayout(t, clk, be, nil)
	p.Reset(1)
	pushRun(p, clk, 1, []uint64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9})
	if !drainUntil(t, func() bool { return p.Stats().Played >= 10 }) {
		st := p.Stats()
		t.Fatalf("Played=%d Silence=%d count=%d", st.Played, st.Silence, be.count())
	}
	if p.Stats().Silence != 0 {
		t.Fatalf("Silence=%d, want 0", p.Stats().Silence)
	}
}

func TestPlayoutInsertsSilenceForGap(t *testing.T) {
	clk := newFakeClock(true)
	be := &recBackend{}
	p := newTestPlayout(t, clk, be, nil)
	p.Reset(1)
	pushRun(p, clk, 1, []uint64{0, 1, 3, 4}) // miss 2
	if !drainUntil(t, func() bool { return p.Stats().Played >= 4 && p.Stats().Silence >= 1 }) {
		st := p.Stats()
		t.Fatalf("Played=%d Silence=%d", st.Played, st.Silence)
	}
	st := p.Stats()
	if st.Silence != 1 {
		t.Fatalf("Silence=%d, want 1", st.Silence)
	}
	if st.Played != 4 {
		t.Fatalf("Played=%d, want 4", st.Played)
	}
}

func TestPlayoutReorderWithinBuffer(t *testing.T) {
	clk := newFakeClock(true)
	be := &recBackend{}
	p := newTestPlayout(t, clk, be, nil)
	p.Reset(1)
	pushRun(p, clk, 1, []uint64{0, 2, 1, 3})
	if !drainUntil(t, func() bool { return p.Stats().Played >= 4 }) {
		t.Fatalf("Played=%d", p.Stats().Played)
	}
	if p.Stats().Silence != 0 {
		t.Fatalf("Silence=%d, want 0", p.Stats().Silence)
	}
}

func TestPlayoutStaleGenDropped(t *testing.T) {
	clk := newFakeClock(true)
	be := &recBackend{}
	p := newTestPlayout(t, clk, be, nil)
	p.Reset(2)
	p.Push(1, 0, clk.nowNs(), audioFrame(1)) // stale gen
	if !drainUntil(t, func() bool { return p.Stats().StaleGen >= 1 }) {
		t.Fatalf("StaleGen=%d", p.Stats().StaleGen)
	}
	if be.count() != 0 {
		t.Fatalf("stale frame written, count=%d", be.count())
	}
}

func TestPlayoutLateFrameDropped(t *testing.T) {
	clk := newFakeClock(true)
	be := &recBackend{}
	p := newTestPlayout(t, clk, be, nil)
	p.Reset(1)
	// pts far in the PAST so deadline (pts+150ms) is already > one frame past.
	pastPts := clk.nowNs() - int64(500*time.Millisecond)
	p.Push(1, 0, pastPts, audioFrame(123))
	if !drainUntil(t, func() bool { return p.Stats().LateDrop >= 1 }) {
		t.Fatalf("LateDrop=%d", p.Stats().LateDrop)
	}
}

func TestPlayoutUnsyncedHolds(t *testing.T) {
	clk := newFakeClock(false) // unsynced
	be := &recBackend{}
	p := newTestPlayout(t, clk, be, nil)
	p.Reset(1)
	pushRun(p, clk, 1, []uint64{0})
	time.Sleep(40 * time.Millisecond)
	if be.count() != 0 {
		t.Fatalf("unsynced: wrote %d frames", be.count())
	}
	if p.Stats().Synced {
		t.Fatal("Synced should be false")
	}
	clk.setSynced(true)
	// Re-push with fresh near-future pts (the first push may now be late).
	pushRun(p, clk, 1, []uint64{1, 2, 3})
	if !drainUntil(t, func() bool { return be.count() >= 1 }) {
		t.Fatal("frames should flush after sync")
	}
	if !p.Stats().Synced {
		t.Fatal("Synced should be true after sync")
	}
}

func TestPlayoutBufferStat(t *testing.T) {
	clk := newFakeClock(false) // unsynced so frames accumulate
	be := &recBackend{}
	p := newTestPlayout(t, clk, be, nil)
	p.Reset(1)
	for s := uint64(0); s < 5; s++ {
		p.Push(1, s, int64(s)*stream.FrameNanos, audioFrame(1))
	}
	if !drainUntil(t, func() bool { return p.Stats().Buffered == 5 }) {
		t.Fatalf("Buffered=%d, want 5", p.Stats().Buffered)
	}
}

func TestPlayoutBufferMsLead(t *testing.T) {
	clk := newFakeClock(true)
	be := &recBackend{}
	p := newTestPlayout(t, clk, be, nil)
	p.Reset(1)
	// pts = now + 100ms; deadline = pts + 150ms = now + 250ms. Nothing should
	// write for at least ~200ms.
	pts := clk.nowNs() + int64(100*time.Millisecond)
	p.Push(1, 0, pts, audioFrame(42))
	time.Sleep(120 * time.Millisecond)
	if be.count() != 0 {
		t.Fatalf("wrote before deadline (lead not honored), count=%d", be.count())
	}
	if !drainUntil(t, func() bool { return be.count() >= 1 }) {
		t.Fatal("frame not written after deadline")
	}
}

func TestPlayoutSetGainHalves(t *testing.T) {
	clk := newFakeClock(true)
	be := &recBackend{}
	p := newTestPlayout(t, clk, be, nil)
	p.Reset(1)
	p.SetGain(0.5)
	const v = 8000
	base := clk.nowNs() + int64(10*time.Millisecond)
	for s := uint64(0); s < 8; s++ {
		pts := base + int64(s)*stream.FrameNanos
		p.Push(1, s, pts, audioFrame(v))
	}
	if !drainUntil(t, func() bool { return p.Stats().Played >= 8 }) {
		t.Fatalf("Played=%d", p.Stats().Played)
	}
	// A late frame (ramp settled) must be ~halved.
	last := be.at(be.count() - 1)
	mid := int16(binary.LittleEndian.Uint16(last[100:102]))
	if mid < v/2-50 || mid > v/2+50 {
		t.Fatalf("settled gain not ~0.5: sample=%d want ~%d", mid, v/2)
	}
}

func TestPlayoutResetZeroesCounters(t *testing.T) {
	clk := newFakeClock(true)
	be := &recBackend{}
	p := newTestPlayout(t, clk, be, nil)
	p.Reset(1)
	pushRun(p, clk, 1, []uint64{0})
	drainUntil(t, func() bool { return p.Stats().Played >= 1 })
	p.Reset(2)
	st := p.Stats()
	if st.Played != 0 || st.Silence != 0 || st.LateDrop != 0 || st.StaleGen != 0 || st.RatePPM != 0 {
		t.Fatalf("Reset did not zero counters: %+v", st)
	}
}

func TestPlayoutPushAfterCloseNoop(t *testing.T) {
	clk := newFakeClock(true)
	be := &recBackend{}
	p := New(Config{Backend: be, Clock: clk, BufferMs: 150, Volume: 1, now: clk.nowNs, servoCfg: fastServoCfg()})
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	p.Push(1, 0, 0, audioFrame(1)) // no panic
	p.Reset(1)                     // no panic
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestPlayoutCloseNoLeak(t *testing.T) {
	clk := newFakeClock(true)
	be := &recBackend{}
	p := newTestPlayout(t, clk, be, nil)
	p.Reset(1)
	pushRun(p, clk, 1, []uint64{0, 1, 2})
	time.Sleep(20 * time.Millisecond)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil { // idempotent
		t.Fatalf("second Close: %v", err)
	}
}

func TestPlayoutStatsConcurrent(t *testing.T) {
	clk := newFakeClock(true)
	be := &recBackend{}
	p := newTestPlayout(t, clk, be, nil)
	p.Reset(1)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			pushRun(p, clk, 1, []uint64{uint64(i)})
			time.Sleep(time.Millisecond)
		}
		close(done)
	}()
	for {
		select {
		case <-done:
			return
		default:
			_ = p.Stats()
			p.SetGain(0.8)
		}
	}
}

// --- servo / watchdog / delay-offset integration ---------------------------

// skewDAC is a fake backend modeling a crystal running dacPPM off nominal. The
// model is DETERMINISTIC and tied to the write count (not the wall clock) so the
// servo sees a clean, noise-free skew signal: each frame written adds
// FrameSamples to the device queue, while the DAC consumes
// FrameSamples·(1+dacPPM/1e6) — i.e. a fast DAC drains the queue by
// FrameSamples·dacPPM/1e6 per write. The reported DeviceDelay is that queue atop
// a generous pre-fill (so it never underruns). The servo's emitted = written −
// queue then equals the DAC-consumed count (minus the constant pre-fill it
// baselines away), which tracks the crystal: emitted grows dacPPM faster than
// master → skew > 0 → correction → −dacPPM, cancelling the drift (§3.5).
type skewDAC struct {
	mu      sync.Mutex
	written int64
	dacPPM  float64
	prefill float64
}

func newSkewDAC(dacPPM float64) *skewDAC {
	return &skewDAC{dacPPM: dacPPM, prefill: 100 * float64(stream.SampleRate)}
}

func (d *skewDAC) Write(f []byte) error {
	d.mu.Lock()
	d.written++
	d.mu.Unlock()
	return nil
}

func (d *skewDAC) Close() error { return nil }

func (d *skewDAC) DeviceDelay() (int64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.written == 0 {
		return 0, false
	}
	writtenSamples := float64(d.written) * float64(stream.FrameSamples)
	dacConsumed := writtenSamples * (1 + d.dacPPM/1e6)
	queue := d.prefill + writtenSamples - dacConsumed
	if queue < 0 {
		queue = 0
	}
	ns := int64(queue / float64(stream.SampleRate) * 1e9)
	return ns, true
}

// feedFor continuously pushes frames (one per ~1 ms real time) to keep the sink
// fed for the test, stopping when stop is closed.
func feedFor(p *Playout, clk *fakeClock, gen uint32, stop <-chan struct{}) {
	go func() {
		seq := uint64(0)
		base := clk.nowNs() + int64(10*time.Millisecond)
		for {
			select {
			case <-stop:
				return
			default:
			}
			pts := base + int64(seq)*stream.FrameNanos
			p.Push(gen, seq, pts, audioFrame(500))
			seq++
			time.Sleep(time.Millisecond)
		}
	}()
}

// TestPlayoutServoDrivesRate ties the servo into the live scheduler: with a
// skewed fake DAC behind the backend the servo must ENGAGE (drive a non-zero,
// clamped correction) and the jitter buffer must stay bounded (no drift-driven
// runaway). The exact ±dacPPM magnitude AND sign are proven deterministically in
// servo_test.go's closed-loop model; here the wall-clock scheduler's catch-up
// dynamics make the live absolute value scheduler-dependent, so the live test
// asserts engagement + clamp + buffer stability (the servo is wired in and runs
// continuously, not an underrun reaction).
func TestPlayoutServoDrivesRate(t *testing.T) {
	clk := newFakeClock(true)
	dac := newSkewDAC(200) // DAC runs +200 ppm fast (implements DelayReporter)
	cfg := fastServoCfg()
	cfg.Window = 600_000_000 // one PI step per window; short window so the
	// live test sees engagement within its few-second budget (the first
	// window is measure-and-discard by design).
	p := New(Config{
		Backend:  dac,
		Clock:    clk,
		BufferMs: 150,
		Volume:   1,
		now:      clk.nowNs,
		servoCfg: cfg,
	})
	t.Cleanup(func() { p.Close() })
	p.Reset(1)

	stop := make(chan struct{})
	defer close(stop)
	feedFor(p, clk, 1, stop)

	// The servo must engage (non-zero correction) and stay within the ±500 clamp;
	// the jitter buffer must stay bounded.
	engaged := drainUntil(t, func() bool {
		ppm := p.Stats().RatePPM
		return ppm > 5 || ppm < -5
	})
	if !engaged {
		t.Fatalf("servo did not engage (RatePPM=%.1f) with a skewed DAC", p.Stats().RatePPM)
	}
	st := p.Stats()
	if st.RatePPM < -500.001 || st.RatePPM > 500.001 {
		t.Fatalf("RatePPM %.1f outside ±500 clamp", st.RatePPM)
	}
	if st.Buffered > 256 {
		t.Fatalf("jitter buffer unbounded: %d", st.Buffered)
	}
}

func TestPlayoutRatePPMClamps(t *testing.T) {
	clk := newFakeClock(true)
	dac := newSkewDAC(20000) // absurd skew
	p := New(Config{
		Backend:  dac,
		Clock:    clk,
		BufferMs: 150,
		Volume:   1,
		now:      clk.nowNs,
		servoCfg: fastServoCfg(),
	})
	t.Cleanup(func() { p.Close() })
	p.Reset(1)
	stop := make(chan struct{})
	defer close(stop)
	feedFor(p, clk, 1, stop)
	// An absurd skew must saturate the servo at the clamp, never beyond.
	drainUntil(t, func() bool {
		ppm := p.Stats().RatePPM
		return ppm >= 499 || ppm <= -499
	})
	ppm := p.Stats().RatePPM
	if ppm < -500.001 || ppm > 500.001 {
		t.Fatalf("RatePPM %.1f outside ±500 clamp", ppm)
	}
}

func TestPlayoutWatchdogFiresRestart(t *testing.T) {
	clk := newFakeClock(true)
	be := &recBackend{}
	var restarts int
	var rmu sync.Mutex
	restart := func() {
		rmu.Lock()
		restarts++
		rmu.Unlock()
	}
	// Short watchdog for a fast test.
	p := New(Config{
		Backend:  be,
		Clock:    clk,
		BufferMs: 150,
		Restart:  restart,
		Volume:   1,
		Watchdog: 80 * time.Millisecond,
		now:      clk.nowNs,
		servoCfg: fastServoCfg(),
	})
	t.Cleanup(func() { p.Close() })
	p.Reset(1)
	pushRun(p, clk, 1, []uint64{0})
	// Wait one watchdog interval with no further pushes → RESTART once.
	if !drainUntil(t, func() bool {
		rmu.Lock()
		defer rmu.Unlock()
		return restarts == 1
	}) {
		rmu.Lock()
		n := restarts
		rmu.Unlock()
		t.Fatalf("restarts=%d, want 1", n)
	}
	// Sink should stay armed: resumed pushes play.
	before := be.count()
	pushRun(p, clk, 1, []uint64{1, 2, 3})
	if !drainUntil(t, func() bool { return be.count() > before }) {
		t.Fatal("sink did not resume after RESTART")
	}
}

func TestPlayoutWatchdogDisarmsAfterRestart(t *testing.T) {
	clk := newFakeClock(true)
	be := &recBackend{}
	var restarts int
	var rmu sync.Mutex
	restart := func() { rmu.Lock(); restarts++; rmu.Unlock() }
	p := New(Config{
		Backend:  be,
		Clock:    clk,
		BufferMs: 150,
		Restart:  restart,
		Volume:   1,
		Watchdog: 60 * time.Millisecond,
		now:      clk.nowNs,
		servoCfg: fastServoCfg(),
	})
	t.Cleanup(func() { p.Close() })
	p.Reset(1)
	pushRun(p, clk, 1, []uint64{0})
	// Stay silent for two watchdog intervals → RESTART then disarm.
	drainUntil(t, func() bool {
		rmu.Lock()
		defer rmu.Unlock()
		return restarts >= 1
	})
	time.Sleep(150 * time.Millisecond) // second interval → disarm
	// After disarm, a stale-gen-free push to the SAME gen is dropped because the
	// sink disarmed; Buffered stays 0 and nothing new plays until Reset.
	countAtDisarm := be.count()
	pushRun(p, clk, 1, []uint64{1, 2})
	time.Sleep(60 * time.Millisecond)
	if be.count() != countAtDisarm {
		t.Fatalf("disarmed sink still played: %d -> %d", countAtDisarm, be.count())
	}
	// Re-arm via Reset and confirm playout resumes.
	p.Reset(2)
	pushRun(p, clk, 2, []uint64{0, 1})
	if !drainUntil(t, func() bool { return be.count() > countAtDisarm }) {
		t.Fatal("sink did not re-arm after Reset")
	}
}

func TestPlayoutWatchdogNilRestart(t *testing.T) {
	clk := newFakeClock(true)
	be := &recBackend{}
	p := New(Config{
		Backend:  be,
		Clock:    clk,
		BufferMs: 150,
		Restart:  nil,
		Volume:   1,
		Watchdog: 50 * time.Millisecond,
		now:      clk.nowNs,
		servoCfg: fastServoCfg(),
	})
	t.Cleanup(func() { p.Close() })
	p.Reset(1)
	pushRun(p, clk, 1, []uint64{0})
	time.Sleep(160 * time.Millisecond) // two intervals: no panic, disarms
	// Re-arm works.
	p.Reset(2)
	pushRun(p, clk, 2, []uint64{0})
	if !drainUntil(t, func() bool { return be.count() >= 1 }) {
		t.Fatal("did not resume after nil-restart disarm + Reset")
	}
}

func TestPlayoutSetDelayOffsetReanchors(t *testing.T) {
	clk := newFakeClock(true)
	be := &recBackend{}
	var restarts int
	var rmu sync.Mutex
	restart := func() { rmu.Lock(); restarts++; rmu.Unlock() }
	p := newTestPlayout(t, clk, be, restart)
	p.Reset(1)
	pushRun(p, clk, 1, []uint64{0, 1, 2, 3, 4})
	// Drain ALL initial frames so no stale write races the probe measurement.
	drainUntil(t, func() bool { return p.Stats().Played >= 5 })

	p.SetDelayOffset(int64(50 * time.Millisecond))
	rmu.Lock()
	r := restarts
	rmu.Unlock()
	if r != 1 {
		t.Fatalf("SetDelayOffset should fire restart once, got %d", r)
	}
	if p.Stats().Buffered != 0 {
		t.Fatalf("buffer not discarded on delay change: %d", p.Stats().Buffered)
	}
	// After re-prime, deadlines shift earlier by 50 ms. Push a single probe frame
	// far in the future and confirm the NEW write lands near pts+150ms−50ms, i.e.
	// before the no-offset deadline pts+150ms.
	before := be.count()
	pts := clk.nowNs() + int64(300*time.Millisecond)
	p.Push(1, 10, pts, audioFrame(99)) // re-arms origin at seq 10
	wantDeadline := pts + int64(150*time.Millisecond) - int64(50*time.Millisecond)
	noOffsetDeadline := pts + int64(150*time.Millisecond)
	if !drainUntil(t, func() bool { return be.count() > before }) {
		t.Fatal("probe frame not played after re-anchor")
	}
	playedAt := clk.nowNs()
	if playedAt >= noOffsetDeadline {
		t.Fatalf("played at %d ns, expected before no-offset deadline %d (offset not applied)", playedAt, noOffsetDeadline)
	}
	if playedAt < wantDeadline-int64(50*time.Millisecond) {
		t.Fatalf("played at %d ns, well before shifted deadline %d", playedAt, wantDeadline)
	}
}

func TestPlayoutSetDelayOffsetNilRestart(t *testing.T) {
	clk := newFakeClock(true)
	be := &recBackend{}
	p := newTestPlayout(t, clk, be, nil) // nil restart
	p.Reset(1)
	pushRun(p, clk, 1, []uint64{0, 1, 2})
	drainUntil(t, func() bool { return p.Stats().Played >= 1 })
	p.SetDelayOffset(int64(30 * time.Millisecond)) // no panic
	if p.Stats().Buffered != 0 {
		t.Fatalf("buffer not discarded: %d", p.Stats().Buffered)
	}
	// Re-primes on the next Push.
	pushRun(p, clk, 1, []uint64{5, 6})
	if !drainUntil(t, func() bool { return be.count() >= 2 }) {
		t.Fatal("did not re-prime after delay change")
	}
}
