package render

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/audio/drift"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/audio/resampler"
	sink "gitlab.rand0m.me/ruben/go/ensemble/internal/audio/sink"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// ─── test doubles ────────────────────────────────────────────────────────────

// fakeSink records writes, walks a scripted Delay() table, and can inject
// backpressure or a write error. Single shared double; mutex-guarded because the
// renderer's consumer goroutine writes while a test inspects from outside.
type fakeSink struct {
	mu sync.Mutex

	started       bool
	startRate     int
	startChannels int

	written    int // total float32 samples consumed across all Writes
	writeCalls int
	writeWidth int
	writeErr   error // when set, Write returns (0, writeErr)

	delays     []delayStep
	delayCalls int
	delayFn    func() (int, bool) // when set, overrides the scripted table
}

type delayStep struct {
	samples int
	ok      bool
}

func (f *fakeSink) Start(rate, channels int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
	f.startRate = rate
	f.startChannels = channels
	f.writeWidth = channels
	return nil
}

func (f *fakeSink) Write(frames []float32) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeCalls++
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.writeWidth > 0 && len(frames)%f.writeWidth != 0 {
		return 0, errors.New("fakeSink: frames not a multiple of channels")
	}
	f.written += len(frames)
	return len(frames), nil
}

func (f *fakeSink) Delay() (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delayFn != nil {
		fn := f.delayFn
		f.mu.Unlock()
		s, ok := fn()
		f.mu.Lock()
		return s, ok
	}
	f.delayCalls++
	if len(f.delays) == 0 {
		return 0, false
	}
	i := f.delayCalls - 1
	if i >= len(f.delays) {
		i = len(f.delays) - 1
	}
	return f.delays[i].samples, f.delays[i].ok
}

func (f *fakeSink) Close() error { return nil }

func (f *fakeSink) totalWritten() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written
}

var _ sink.AudioSink = (*fakeSink)(nil)

// fakeTimeline is a scriptable group.Timeline.
type fakeTimeline struct {
	mu      sync.Mutex
	sample  int64
	playing bool
	ok      bool
}

func (t *fakeTimeline) NowSample() (int64, bool, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sample, t.playing, t.ok
}

func (t *fakeTimeline) set(sample int64, playing, ok bool) {
	t.mu.Lock()
	t.sample, t.playing, t.ok = sample, playing, ok
	t.mu.Unlock()
}

// fakeReader is a FrameReader that emits a constant stereo frame and reports a
// settable streamGen. resolve counts are not needed; it never blocks.
type fakeReader struct {
	mu    sync.Mutex
	l, r  float32
	gen   uint64
	reads int
}

func (s *fakeReader) Read(p []float32) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	n := len(p) - len(p)%2
	for i := 0; i < n; i += 2 {
		p[i] = s.l
		p[i+1] = s.r
	}
	return n, nil
}

func (s *fakeReader) StreamGen() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

func (s *fakeReader) setGen(g uint64) {
	s.mu.Lock()
	s.gen = g
	s.mu.Unlock()
}

func (s *fakeReader) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func docWithNode(n state.NodeRecord, mediaFile string) func() state.ConfigDoc {
	doc := state.ConfigDoc{
		Nodes:  []state.NodeRecord{n},
		Groups: []state.GroupRecord{{ID: "g", Media: state.MediaSelection{File: mediaFile}}},
	}
	return func() state.ConfigDoc { return doc }
}

// ─── pure unit tests ─────────────────────────────────────────────────────────

func TestWantContentMapping(t *testing.T) {
	const rate = 48000
	tests := []struct {
		name      string
		sample    int64
		hwDelayUs int
		want      int64
	}{
		{"zero", 0, 0, 0},
		{"sample only", 100000, 0, 100000},
		{"2ms@48k=+96", 0, 2000, 96},
		{"sample+2ms", 48000, 2000, 48096},
		{"negative trim", 48000, -1000, 48000 - 48},
		{"round half", 100, 10, 100}, // 10us*48000 = 0.48 -> 0
		{"round up", 100, 11, 101},   // 11us*48000 = 0.528 -> 1
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := wantContent(tc.sample, tc.hwDelayUs, rate)
			if got != tc.want {
				t.Fatalf("wantContent(%d,%dus)=%d, want %d", tc.sample, tc.hwDelayUs, got, tc.want)
			}
		})
	}
}

func TestResampleSelectGain(t *testing.T) {
	r := newTestRenderer(&fakeSink{}, &fakeTimeline{}, &fakeReader{}, state.NodeRecord{
		ID: "n1", Channel: "left", GainDB: -6,
	})
	r.applyLane(state.NodeRecord{Channel: "left", GainDB: -6})

	// Constant stereo input: L=0.5, R=-0.5. "left" picks ch0 (0.5) to both outputs,
	// then -6 dB (~0.501x) => ~0.2506 on both, after the resampler primes its taps.
	in := make([]float32, 2*2000)
	for i := 0; i < len(in); i += 2 {
		in[i] = 0.5
		in[i+1] = -0.5
	}
	out := r.resampleSelect(in, 2)
	if len(out) == 0 {
		t.Fatalf("no output produced")
	}
	// Inspect the tail (priming transients are at the head).
	last := out[len(out)-2:]
	wantV := float32(0.5 * 0.5012)
	if math.Abs(float64(last[0]-wantV)) > 5e-3 || math.Abs(float64(last[1]-wantV)) > 5e-3 {
		t.Fatalf("left+(-6dB) out tail=%v, want both ~%v", last, wantV)
	}
	if last[0] != last[1] {
		t.Fatalf("left fan-out must put the same sample on both outputs: %v", last)
	}
	// sourceConsumed advanced by consumed/srcCh.
	r.rmu.Lock()
	sc := r.sourceConsumed
	r.rmu.Unlock()
	if sc <= 0 {
		t.Fatalf("sourceConsumed not advanced: %d", sc)
	}
	if sc > 2000 {
		t.Fatalf("sourceConsumed=%d exceeds frames fed (2000)", sc)
	}
}

func TestGrossErrorReseek(t *testing.T) {
	d := drift.NewDriftLoop(drift.DefaultDriftParams())
	action, ratio := d.Update(100000)
	if action != drift.DriftReseek || ratio != 1.0 {
		t.Fatalf("gross error: got (%v,%v), want (reseek,1.0)", action, ratio)
	}
	d.Reset()
	if action, _ := d.Update(10); action != drift.DriftHold {
		t.Fatalf("small error after reset: got %v, want hold", action)
	}

	// Renderer routes DriftReseek -> reseek + settle, observed via baseline reset.
	rd := newTestRenderer(&fakeSink{}, &fakeTimeline{sample: 5000, playing: true, ok: true},
		&fakeReader{}, state.NodeRecord{ID: "n1", Channel: "stereo"})
	rd.configDoc = docWithNode(state.NodeRecord{ID: "n1", Channel: "stereo"}, "song.mp3")
	rd.hadGen = true // skip the startup reseek so we exercise the gross-error path
	rd.loadedGen = 0
	// Force a large content error: pretend a lot consumed past want.
	rd.baseSourceConsumed = 0
	rd.rmu.Lock()
	rd.sourceConsumed = 1_000_000
	rd.rmu.Unlock()
	// Put some backlog in the ring so a reseek visibly resets it.
	rd.ring.Write(make([]float32, 480*2))
	rd.tick()
	if rd.settle != settleTicks {
		t.Fatalf("after gross error settle=%d, want %d", rd.settle, settleTicks)
	}
	if rd.ring.Len() != 0 {
		t.Fatalf("reseek did not Reset the ring: len=%d", rd.ring.Len())
	}
	rd.rmu.Lock()
	sc := rd.sourceConsumed
	rd.rmu.Unlock()
	if sc != 0 {
		t.Fatalf("reseek did not zero sourceConsumed: %d", sc)
	}
}

func TestHaveSyncFalseHolds(t *testing.T) {
	snk := &fakeSink{}
	rdr := &fakeReader{l: 0.5, r: 0.5}
	r := newTestRenderer(snk, &fakeTimeline{ok: false}, rdr,
		state.NodeRecord{ID: "n1", Channel: "stereo"})
	r.configDoc = docWithNode(state.NodeRecord{ID: "n1", Channel: "stereo"}, "song.mp3")

	var lastTick RenderTick
	r.SetOnTick(func(rt RenderTick) { lastTick = rt })
	r.tick()
	if lastTick.HaveSync {
		t.Fatalf("ok=false must yield HaveSync=false")
	}
	if rdr.readCount() != 0 {
		t.Fatalf("reader consulted while not synced: reads=%d", rdr.readCount())
	}
	if snk.totalWritten() != 0 {
		t.Fatalf("sink written while not synced: %d", snk.totalWritten())
	}
}

func TestClipChangeReseeks(t *testing.T) {
	// streamGen change is the ensemble analog of a clip/media change. After a
	// streamGen bump the renderer reseeks (baseline reset + ring reset).
	rdr := &fakeReader{l: 0.1, r: 0.1, gen: 7}
	r := newTestRenderer(&fakeSink{}, &fakeTimeline{sample: 1000, playing: true, ok: true}, rdr,
		state.NodeRecord{ID: "n1", Channel: "stereo"})
	r.configDoc = docWithNode(state.NodeRecord{ID: "n1", Channel: "stereo"}, "song.mp3")

	// First ok tick: startup reseek primes loadedGen=7.
	r.tick()
	if !r.hadGen || r.loadedGen != 7 {
		t.Fatalf("startup did not prime gen: hadGen=%v gen=%d", r.hadGen, r.loadedGen)
	}
	// Seed some backlog + content, then bump streamGen.
	r.ring.Write(make([]float32, 960))
	r.rmu.Lock()
	r.sourceConsumed = 12345
	r.rmu.Unlock()
	rdr.setGen(8)
	r.settle = 0
	r.tick()
	if r.loadedGen != 8 {
		t.Fatalf("streamGen change not picked up: gen=%d", r.loadedGen)
	}
	if r.ring.Len() != 0 {
		t.Fatalf("streamGen change did not reseek (ring not reset): len=%d", r.ring.Len())
	}
	// reseek arms settle=settleTicks, then this same tick consumes one settle slot.
	if r.settle != settleTicks-1 {
		t.Fatalf("streamGen change did not arm settle: %d, want %d", r.settle, settleTicks-1)
	}
}

// TestCoarseDelayFallback drives the full closed loop with a coarse (ok=false) sink
// against the real plant and asserts the error stays bounded well under HardErrSamp
// (the integrator absorbs the near-constant coarse bias, doc 06 §4.2).
func TestCoarseDelayFallback(t *testing.T) {
	res := simulatePlant(t, plantConfig{
		crystalPPM:   25,
		preciseDelay: false,
		ticks:        4000,
	})
	if res.reseeksAfterSettle > 0 {
		t.Fatalf("coarse path reseeked %d times after settling", res.reseeksAfterSettle)
	}
	if res.maxAbsErrAfterSettle >= drift.DefaultDriftParams().HardErrSamp {
		t.Fatalf("coarse path error %d not bounded under HardErrSamp %d",
			res.maxAbsErrAfterSettle, drift.DefaultDriftParams().HardErrSamp)
	}
	// Coarse model still produces DriftHold ticks.
	if res.holdTicks == 0 {
		t.Fatalf("no DriftHold ticks observed on coarse path")
	}
}

// ─── real-plant convergence harness (doc 06 §3.6 / A.4) ──────────────────────
//
// This is the regression guard for the mpvsync output-domain bug. It does NOT run
// the renderer goroutines (which need wall-clock real time); instead it steps the
// SAME control-law math (the renderer's wantContent / playedContent error
// definition and drift.DriftLoop) against a faithful plant model:
//
//   - The DAC drains output frames at f_out = Rate·(1 + crystalPPM·1e-6),
//     INDEPENDENT of the commanded ratio.
//   - The resampler consumes `ratio` content frames per output frame produced;
//     output is produced/drained at f_out, so sourceConsumed advances at ratio·f_out.
//   - The ring + device backlog EVOLVE from that drain and are fed back into
//     playedContent — they are not free inputs.
//
// A test that nulls against an output-domain model, or only checks "ratio<1 when
// error>0", does NOT catch the bug and is rejected in review (doc 06 §3.6).

var traceConv = false

type plantConfig struct {
	crystalPPM   float64
	preciseDelay bool
	ticks        int
}

type plantResult struct {
	finalRatioPPM        float64
	finalErr             int
	maxAbsErrAfterSettle int
	reseeksAfterSettle   int
	holdTicks            int
	finalIntegralPPM     float64

	// Back-half (post-transient) averages. The integer-sample quantization of the
	// error creates a slow, bounded limit cycle around the true operating point, so
	// convergence is asserted on these means (which settle to the steady values),
	// not on a single instantaneous final tick.
	avgRatioPPM  float64
	avgErr       float64
	maxAbsIntPPM float64
}

func simulatePlant(t *testing.T, cfg plantConfig) plantResult {
	t.Helper()
	const rate = 48000
	const leadFrames = rate * 300 / 1000 // 300 ms playout target (per-channel frames)
	tickSec := 0.020                     // 20 ms control tick
	fOut := float64(rate) * (1 + cfg.crystalPPM*1e-6)
	d := drift.NewDriftLoop(drift.DefaultDriftParams())

	// The downstream output-domain backlog target (ring lead + a little device
	// buffer). The producer keeps total backlog B = ring + device at this target;
	// the device share is held at devTarget and the rest sits in the ring.
	const devTarget = rate * 10 / 1000 // ~10 ms device buffer at steady state
	const targetB = leadFrames + devTarget

	// Plant state, all in per-channel frames since the reseek baseline.
	ratio := 1.0
	sourceConsumed := 0.0 // SOURCE frames the resampler has consumed
	backlog := 0.0        // output-domain frames buffered downstream (ring + device)
	devFrames := 0.0      // device share of the backlog

	// The timeline advances at exactly Rate (the master crystal == nominal Rate; the
	// LOCAL DAC crystal is offset by crystalPPM — that offset is the disturbance).
	wantContent := 0.0
	baseSourceConsumed := 0.0

	var res plantResult
	var backHalf int
	settle := settleTicks

	for i := 0; i < cfg.ticks; i++ {
		dt := tickSec
		// (a) DAC drains fOut*dt output frames this tick, INDEPENDENT of ratio.
		outDrained := fOut * dt
		backlog -= outDrained
		if backlog < 0 {
			backlog = 0 // underrun floor (would trip a reseek via the error below)
		}

		// (b) The producer refills the backlog toward targetB, producing `produced`
		//     output frames (and topping the device share toward devTarget).
		produced := targetB - backlog
		if produced < 0 {
			produced = 0
		}
		backlog += produced
		devFrames = devTarget // device share held at target by the drain/refill

		// (c) Producing `produced` output frames consumes ratio*produced SOURCE
		//     frames — the actuator sits HERE, in source consumption, not in the
		//     DAC drain (doc 06 §3.2). This is what makes the loop controllable.
		sourceConsumed += ratio * produced

		// (e) playedContent EXACTLY as the renderer computes it (doc 06 §3.3), using
		//     the modeled backlog (precise vs coarse Delay()).
		ringFrames := backlog - devFrames
		var dev float64
		if cfg.preciseDelay {
			dev = devFrames
		} else {
			// Coarse model: a near-constant wall-time estimate (the renderer's
			// coarseDeviceDelay). The PI integrator absorbs the constant bias.
			dev = float64(rate) * (dt * 0.5)
		}
		buffered := math.Round((ringFrames + dev) * ratio)
		played := baseSourceConsumed + sourceConsumed - buffered
		errSamples := int(math.Round(played - wantContent))

		// (d) The group timeline advances at the nominal Rate for the NEXT tick.
		//     Advanced AFTER the error is taken so want/played share the same instant
		//     (the reseek baseline is captured at this same want), giving a clean ~0
		//     startup error — the buffer holds FUTURE content, already netted out of
		//     played by the `buffered` subtraction.
		wantContent += float64(rate) * dt

		// (f) settle window (no control), like the renderer.
		if settle > 0 {
			settle--
			continue
		}

		action, r2 := d.Update(errSamples)
		switch action {
		case drift.DriftHold:
			ratio = clampRatio(r2) // belt-and-braces re-clamp, as Resampler.SetRatio does
			res.holdTicks++
		case drift.DriftReseek:
			// Reseek: re-baseline like the renderer (doc 06 §6.1).
			baseSourceConsumed = wantContent
			sourceConsumed = 0
			ratio = 1.0
			backlog = 0
			devFrames = 0
			d.Reset()
			settle = settleTicks
			if i > cfg.ticks/4 { // after the initial transient
				res.reseeksAfterSettle++
			}
			continue
		}

		// Record convergence stats over the back half (post-transient).
		if i > cfg.ticks/2 {
			if a := abs(errSamples); a > res.maxAbsErrAfterSettle {
				res.maxAbsErrAfterSettle = a
			}
			res.avgRatioPPM += (ratio - 1) * 1e6
			res.avgErr += float64(errSamples)
			if ip := math.Abs(d.IntegralPPM()); ip > res.maxAbsIntPPM {
				res.maxAbsIntPPM = ip
			}
			backHalf++
		}
		if traceConv && i%2000 == 0 {
			println("i", i, "err", errSamples, "ratioPPM", int(math.Round((ratio-1)*1e6)), "intPPM", int(math.Round(d.IntegralPPM())))
		}
		res.finalErr = errSamples
		res.finalRatioPPM = (ratio - 1) * 1e6
		res.finalIntegralPPM = d.IntegralPPM()
	}
	if backHalf > 0 {
		res.avgRatioPPM /= float64(backHalf)
		res.avgErr /= float64(backHalf)
	}
	return res
}

func clampRatio(ratio float64) float64 {
	lo := 1.0 - float64(resampler.MaxPPM)*1e-6
	hi := 1.0 + float64(resampler.MaxPPM)*1e-6
	if ratio < lo {
		return lo
	}
	if ratio > hi {
		return hi
	}
	return ratio
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func TestRealPlantConvergence(t *testing.T) {
	tests := []struct {
		name       string
		crystalPPM float64
	}{
		{"+40ppm", +40},
		{"-40ppm", -40},
		{"+15ppm", +15},
		{"zero", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := simulatePlant(t, plantConfig{
				crystalPPM:   tc.crystalPPM,
				preciseDelay: true,
				ticks:        40000,
			})

			// (1) errSamples -> 0. The integer-sample quantization leaves a small,
			//     bounded limit cycle (sub-millisecond), so assert the mean error is
			//     ~0 and the worst-case stays well under 1 ms (48 samples @ 48k).
			if math.Abs(res.avgErr) > 5 {
				t.Fatalf("mean error %.2f samples not converged to ~0", res.avgErr)
			}
			if res.maxAbsErrAfterSettle >= 48 {
				t.Fatalf("worst-case error %d samples not sub-ms post-settle", res.maxAbsErrAfterSettle)
			}

			// (2) The commanded ratio settles to 1 - crystalPPM*1e-6 (NOT the clamp).
			//     A +crystal DAC drains fast => node runs ahead => ratio<1 to slow.
			//     Asserted on the back-half mean (the operating point of the limit cycle).
			wantRatioPPM := -tc.crystalPPM
			if math.Abs(res.avgRatioPPM-wantRatioPPM) > 3.0 {
				t.Fatalf("mean ratio %.2f ppm, want ~%.2f ppm (1 - crystalPPM)",
					res.avgRatioPPM, wantRatioPPM)
			}

			// (3) NOT riding the ±MaxPPM clamp.
			if math.Abs(res.avgRatioPPM) >= float64(resampler.MaxPPM)-1 {
				t.Fatalf("ratio rode the clamp (%.2f ppm) — output-domain regression?",
					res.avgRatioPPM)
			}

			// (4) The integrator holds a finite, non-clamped value throughout.
			ic := drift.DefaultDriftParams().IntegralClamp
			if res.maxAbsIntPPM >= ic-1 {
				t.Fatalf("integrator pinned at clamp (%.2f ppm) — windup regression",
					res.maxAbsIntPPM)
			}

			// (5) No reseek after settling.
			if res.reseeksAfterSettle > 0 {
				t.Fatalf("%d reseeks after settling — steady-state reseek regression",
					res.reseeksAfterSettle)
			}
		})
	}
}

// ─── integration smoke test: the real goroutines run + drain ─────────────────

func TestRunDrainsToSink(t *testing.T) {
	snk := &fakeSink{delays: []delayStep{{480, true}}}
	tl := &fakeTimeline{sample: 0, playing: true, ok: true}
	rdr := &fakeReader{l: 0.2, r: -0.2, gen: 1}
	r := NewRenderer(snk, tl, docWithNode(state.NodeRecord{ID: "n1", Channel: "stereo"}, "s.mp3"),
		rdr, "n1", DefaultRendererParams())

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Advance the timeline like a playing master while the renderer chases it.
	deadline := time.Now().Add(180 * time.Millisecond)
	for time.Now().Before(deadline) {
		s, _, _ := tl.NowSample()
		tl.set(s+960, true, true) // ~20 ms @ 48k per step
		time.Sleep(5 * time.Millisecond)
	}
	<-done

	if !snk.started || snk.startRate != 48000 || snk.startChannels != 2 {
		t.Fatalf("sink not started canonically: started=%v %d/%d", snk.started, snk.startRate, snk.startChannels)
	}
	if snk.totalWritten() == 0 {
		t.Fatalf("renderer produced no output to the sink")
	}
	if rdr.readCount() == 0 {
		t.Fatalf("producer never read from the FrameReader")
	}
}

// newTestRenderer builds a renderer with the given doubles and the node record's
// lane pre-applied, for the off-goroutine unit tests that call tick()/resampleSelect
// directly.
func newTestRenderer(snk sink.AudioSink, tl *fakeTimeline, src FrameReader, n state.NodeRecord) *Renderer {
	r := NewRenderer(snk, tl, docWithNode(n, "song.mp3"), src, n.ID, DefaultRendererParams())
	r.applyLane(n)
	return r
}
