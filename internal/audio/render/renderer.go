package render

// The renderer is the audio sibling of the master-side origin: both chase the same
// group Timeline, but where the origin emits the stream, the renderer actuates a
// resample ratio against an AudioSink so this node's playout tracks the group
// timeline to sub-millisecond accuracy. It runs two goroutines under Run(ctx):
//
//   - producer: recv-ring (FrameReader) → Resampler.Process(ratio) → channel-select
//     + GainDB → playout Ring.Write. Holds the resampler lock across Process so a
//     concurrent SetRatio from the control goroutine cannot race interpolation, and
//     accumulates sourceConsumed (the regulated content-domain quantity, doc 06 §3.3).
//   - consumer/control: drains the Ring into the sink (blocking Write = playout
//     pacing) and, every Tick, maps Timeline.NowSample() → wantContent, computes the
//     content-domain playedContent from sourceConsumed and the Delay()-aware backlog,
//     and runs the drift PI loop to trim the ratio (or reseek on a gross error /
//     underrun).
//
// This is the corrected content-domain loop (doc 06 §3): see doc.go for why the
// mpvsync output-domain formula is the bug. Ensemble plays ONE continuous stream per
// group (no clip lanes) — the timeline sample IS the content position directly, so
// the media renderer's clip/AssetResolver machinery collapses to a FrameReader seam
// plus a streamGen change trigger.

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/audio/drift"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/audio/resampler"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/audio/ring"
	sink "gitlab.rand0m.me/ruben/go/ensemble/internal/audio/sink"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/group"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// canonicalSourceChannels is the source-side channel count the resampler is built
// for: the canonical group stream is stereo (A.12). The node's sink may have a
// different channel count; channel-select fans the chosen source channel(s) out to
// the sink's Channels.
const canonicalSourceChannels = 2

// settleTicks is the number of control ticks the drift loop is held off after a
// reseek/load while the new buffer primes (doc 06 §6.1).
const settleTicks = 2

// RendererParams configures one node's audio renderer (doc 06 §2.1, A.12).
type RendererParams struct {
	Rate     int               // canonical rate, 48000
	Channels int               // sink output channels, 2
	LeadMs   int               // playout buffer ahead, default 300
	Tick     time.Duration     // control tick, default 20ms
	Drift    drift.DriftParams // PI loop tunables (A.12)
}

// DefaultRendererParams returns the canonical configuration: 48000 Hz / stereo, a
// 300 ms playout buffer, a 20 ms control tick, and the default drift PI gains
// (Appendix A.12 — the single source of truth).
func DefaultRendererParams() RendererParams {
	return RendererParams{
		Rate:     48000,
		Channels: 2,
		LeadMs:   300,
		Tick:     20 * time.Millisecond,
		Drift:    drift.DefaultDriftParams(),
	}
}

// FrameReader is the producer's input seam: decoded canonical-rate interleaved
// stereo frames from the receive side (05 recv → FEC → decode → recv-ring). Kept
// as an interface so render does not hard-import stream/sink_net; cmd/node.Run
// wires the concrete reader (P4.7 risk 2). Read returns the number of float32
// samples copied (a multiple of canonicalSourceChannels) and may return a short
// count (including 0) when the recv-ring is momentarily empty — the producer then
// backs off and retries. StreamGen reports the current group stream generation; a
// change signals a media/seek discontinuity and triggers a reseek (doc 06 §6.3).
type FrameReader interface {
	Read(p []float32) (n int, err error)
	StreamGen() uint64
}

// RenderTick reports one control tick for UI/logging (doc 06 §2.4). Audio analog of
// the video TickInfo.
type RenderTick struct {
	HaveSync     bool
	Playing      bool
	WantSample   int64
	PlayedSample int64
	ErrorSamples int
	RatioPPM     float64
	Action       drift.DriftAction
	Underruns    int64
	Clip         string // active media id ("" => gap/silence); the group's Media.File
}

// Renderer chases group.Timeline and plays this node's lane through sink.AudioSink,
// holding sync with the content-domain drift PI loop (doc 06 §2,§3). Its lifecycle
// is driven by P4.9 via the group.Hooks StartRender/StopRender seam (Run under a
// cancelable ctx).
type Renderer struct {
	sink      sink.AudioSink
	timeline  group.Timeline
	configDoc func() state.ConfigDoc
	src       FrameReader
	nodeID    string

	// pMu guards p (live params) + onTick; a tick snapshots them once at its top.
	pMu    sync.Mutex
	p      RendererParams
	onTick func(RenderTick)

	// pipeline state owned by the control goroutine (and the producer it spawns).
	resampler *resampler.Resampler
	drift     *drift.DriftLoop
	ring      *ring.Ring

	// rmu guards the producer-shared pipeline fields: the resampler (ratio +
	// interpolation state) and the content-domain sourceConsumed counter. The
	// producer advances sourceConsumed under rmu; the control goroutine reads it and
	// re-baselines it on reseek, and sets the ratio each tick.
	rmu            sync.Mutex
	sourceConsumed int64 // cumulative SOURCE frames consumed since the reseek baseline

	// chMu guards the channel-select + gain snapshot, refreshed each tick from the
	// node's ConfigDoc record (cheap; the lane rarely changes).
	chMu sync.Mutex
	role Channel // channel role this node emits (doc 06 §5.1); zero => stereo
	gain float32 // linear gain from NodeRecord.GainDB

	// baseSourceConsumed is the wantContent captured at the last reseek so
	// playedContent and wantContent share an origin (doc 06 §3.3). framesWritten /
	// lastWriteAt are advanced by the drain and feed ONLY the coarse Delay() model
	// (doc 06 §4.2) — they are NOT the regulated quantity (that is sourceConsumed).
	baseSourceConsumed int64
	framesWritten      int64
	lastWriteAt        time.Time

	settle    int
	ratioSet  bool
	lastRatio float64
	loadedGen uint64 // streamGen currently primed; reseek on change (doc 06 §6.3)
	hadGen    bool   // whether loadedGen has been initialized (first ok tick)
	underruns int64
}

// NewRenderer wires a renderer. timeline supplies the group sample clock; configDoc
// snapshots the replicated doc so the renderer can look up its own NodeRecord
// (Channel/GainDB/HWDelayUs); src is the producer's decoded-frame input; nodeID is
// this node's id.
func NewRenderer(
	snk sink.AudioSink,
	timeline group.Timeline,
	configDoc func() state.ConfigDoc,
	src FrameReader,
	nodeID string,
	p RendererParams,
) *Renderer {
	p = sanitizeParams(p, DefaultRendererParams())
	return &Renderer{
		sink:      snk,
		timeline:  timeline,
		configDoc: configDoc,
		src:       src,
		nodeID:    nodeID,
		p:         p,
		resampler: resampler.NewResampler(canonicalSourceChannels),
		drift:     drift.NewDriftLoop(p.Drift),
		ring:      ring.NewRing(ringCap(p)),
		gain:      1.0,
		lastRatio: 1.0,
	}
}

// sanitizeParams fills any non-positive field from def so a partial struct stays sane.
func sanitizeParams(p, def RendererParams) RendererParams {
	if p.Rate <= 0 {
		p.Rate = def.Rate
	}
	if p.Channels <= 0 {
		p.Channels = def.Channels
	}
	if p.LeadMs <= 0 {
		p.LeadMs = def.LeadMs
	}
	if p.Tick <= 0 {
		p.Tick = def.Tick
	}
	return p
}

// ringCap sizes the jitter buffer to hold ~2× LeadMs of interleaved audio so the
// producer has slack above the LeadMs target it fills to.
func ringCap(p RendererParams) int {
	lead := p.LeadMs
	if lead <= 0 {
		lead = 300
	}
	return 2 * p.Rate * p.Channels * lead / 1000
}

// leadSamples is the fill target (interleaved samples) the producer keeps the
// ring at: ONE THIRD of LeadMs (~100 ms at the canonical 300), floored at two
// drain chunks. The full LeadMs is the TOTAL ahead-supply in the pipeline
// (origin lead), shared between the receive ring, this playout ring and the
// device buffer — a producer that targets the whole lead here starves the
// upstream share, the ring oscillates near empty, and every drain pass logs an
// underrun (with audible gaps when the device buffer drains during the empty
// windows). A third keeps the device fed between control passes while leaving
// the rest of the lead as upstream slack.
func leadSamples(p RendererParams) int {
	lead := p.LeadMs
	if lead <= 0 {
		lead = 300
	}
	target := p.Rate * p.Channels * lead / 3 / 1000
	if min := 2 * drainFrames * p.Channels; target < min {
		target = min
	}
	return target
}

// SetOnTick registers a per-tick status callback (UI/logging).
func (r *Renderer) SetOnTick(fn func(RenderTick)) {
	r.pMu.Lock()
	r.onTick = fn
	r.pMu.Unlock()
}

// SetParams re-tunes the live control parameters; it takes effect on the next tick.
// LeadMs and the drift gains re-tune live; Rate/Channels changes require a restart
// (the running pipeline is not resized). A zero Tick/Rate/Channels/LeadMs is kept
// at its current value so a partial update cannot stall the loop.
func (r *Renderer) SetParams(p RendererParams) {
	r.pMu.Lock()
	p = sanitizeParams(p, r.p)
	r.p = p
	r.pMu.Unlock()
	// Re-tune the drift loop's gains live (it is owned by the control goroutine, but
	// NewDriftLoop is a cheap value swap; the integrator is rebuilt only on a real
	// gain change, which a reseek would reset anyway).
	r.drift = drift.NewDriftLoop(p.Drift)
}

func (r *Renderer) params() RendererParams {
	r.pMu.Lock()
	defer r.pMu.Unlock()
	return r.p
}

func (r *Renderer) tickCallback() func(RenderTick) {
	r.pMu.Lock()
	defer r.pMu.Unlock()
	return r.onTick
}

// Run starts the producer + consumer/control goroutines and blocks until ctx is
// cancelled. It opens the sink at the configured rate/channels and closes it on exit.
func (r *Renderer) Run(ctx context.Context) error {
	p := r.params()
	if err := r.sink.Start(p.Rate, p.Channels); err != nil {
		return err
	}
	defer r.sink.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.produce(ctx)
	}()

	err := r.control(ctx)
	wg.Wait()
	return err
}

// produce keeps the ring filled to ~LeadMs from the FrameReader, resampling at the
// current ratio and applying the node's channel-select + gain. It backs off with a
// short sleep when the ring is near full or the reader has no frames. Single
// producer of the Ring.
func (r *Renderer) produce(ctx context.Context) {
	in := make([]float32, 4096)
	var pending []float32 // resampled-but-not-yet-written tail

	backoff := time.NewTimer(0)
	if !backoff.Stop() {
		<-backoff.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		p := r.params()
		target := leadSamples(p)

		// Flush any pending resampled output that did not fit last round.
		if len(pending) > 0 {
			w := r.ring.Write(pending)
			pending = pending[w:]
			if len(pending) > 0 {
				r.sleep(ctx, backoff, time.Millisecond)
				continue
			}
		}

		if r.ring.Len() >= target {
			r.sleep(ctx, backoff, 2*time.Millisecond)
			continue
		}

		if r.src == nil {
			r.sleep(ctx, backoff, 5*time.Millisecond)
			continue
		}

		n, err := r.src.Read(in)
		if n > 0 {
			out := r.resampleSelect(in[:n], p.Channels)
			if len(out) > 0 {
				w := r.ring.Write(out)
				if w < len(out) {
					pending = append(pending[:0], out[w:]...)
				}
			}
		}
		if n == 0 && err == nil {
			// Recv-ring momentarily empty: back off and retry (the producer never
			// pushes silence — an underrun is observed by the control tick).
			r.sleep(ctx, backoff, 2*time.Millisecond)
			continue
		}
		if err != nil {
			// Reader closed / fatal: stop producing; the control loop holds and the
			// next reseek re-primes once the reader recovers.
			r.sleep(ctx, backoff, 5*time.Millisecond)
		}
	}
}

// resampleSelect runs interleaved source samples through the resampler at the
// current ratio (under rmu, accumulating sourceConsumed), then channel-selects to
// the sink's output channels and applies gain. The source is canonical stereo; the
// sink has outCh channels.
func (r *Renderer) resampleSelect(in []float32, outCh int) []float32 {
	r.rmu.Lock()
	resampled, consumed := r.resampler.Process(nil, in)
	// consumed is in interleaved source samples; /srcCh gives per-channel frames
	// (the content-domain quantity the loop regulates, doc 06 §3.3 step 4).
	r.sourceConsumed += int64(consumed / canonicalSourceChannels)
	r.rmu.Unlock()

	r.chMu.Lock()
	role := r.role
	g := r.gain
	r.chMu.Unlock()

	srcCh := canonicalSourceChannels
	frames := len(resampled) / srcCh
	if frames == 0 {
		return nil
	}
	out := make([]float32, frames*outCh)
	for f := 0; f < frames; f++ {
		base := f * srcCh
		frame := resampled[base : base+srcCh]
		for oc := 0; oc < outCh; oc++ {
			out[f*outCh+oc] = g * SelectChannel(frame, role, outCh, oc)
		}
	}
	return out
}

func (r *Renderer) sleep(ctx context.Context, t *time.Timer, d time.Duration) {
	t.Reset(d)
	select {
	case <-ctx.Done():
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
	case <-t.C:
	}
}

// control is the consumer/control goroutine: it drains the ring into the sink and,
// every Tick, runs the timeline→sample mapping + drift loop.
func (r *Renderer) control(ctx context.Context) error {
	p := r.params()
	r.lastWriteAt = time.Now()

	ticker := time.NewTicker(p.Tick)
	defer ticker.Stop()
	lastTick := p.Tick

	for {
		// Drain whatever is buffered into the sink between ticks; the sink's Write
		// blocks for backpressure so this paces playout.
		r.drain(ctx)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			r.tick()
			if cur := r.params().Tick; cur > 0 && cur != lastTick {
				lastTick = cur
				ticker.Reset(cur)
			}
		}
	}
}

// drainFrames is the per-channel frame count pulled per drain pass.
const drainFrames = 480 // 10 ms @ 48k (FramesPerChunk, A.12)

// drain moves the ring's buffered audio into the sink in chunk-sized writes
// until the ring is empty, bumping framesWritten (the coarse-Delay() counter
// only). The sink's blocking Write provides the playout pacing, so draining
// everything available never runs ahead of the device — while a single
// chunk-per-tick drain would cap throughput at half real time (480 frames per
// 20 ms tick). On a ring underrun it does NOT push silence (that would advance
// counters and fight the upcoming reseek) — it bumps underruns and lets the next
// tick observe the gross error (doc 06 §2.3, §6.4). On an ALSA -EPIPE
// (sink.ErrUnderrun) it reseeks immediately (doc 06 §6.4).
func (r *Renderer) drain(ctx context.Context) {
	p := r.params()
	outCh := p.Channels
	buf := make([]float32, drainFrames*outCh)
	// Per-pass cap: ~100 ms of audio. Plenty of throughput headroom against the
	// 20 ms tick cadence — while guaranteeing the CONTROL LAW keeps running: a
	// follower's producer is fed by the live receiver and never lets the ring
	// empty, so an unbounded drain-until-empty loop would never return and
	// tick() (sync detection, drift, reseek) would never run — the renderer
	// then plays uncontrolled forever with HaveSync stuck false.
	maxPass := p.Rate / 10 * outCh
	wrote := 0
	for wrote < maxPass {
		select {
		case <-ctx.Done():
			return
		default:
		}
		got := r.ring.Read(buf)
		if got == 0 {
			if wrote == 0 {
				// Nothing buffered at all this pass: the producer fell behind. The
				// next tick sees playedContent lag past HardErrSamp and reseeks.
				r.underruns++
			}
			return
		}
		got -= got % outCh
		if got == 0 {
			return
		}
		n, err := r.sink.Write(buf[:got])
		if err != nil {
			if errors.Is(err, sink.ErrUnderrun) {
				// Device underrun: gross error. The sink already re-PREPAREd the PCM;
				// reseek to the current want so the loop re-converges from neutral.
				r.underruns++
				r.reseekToNow()
			}
			return
		}
		wrote += n
		r.framesWritten += int64(n / outCh)
		r.lastWriteAt = time.Now()
	}
}

// reseekToNow reseeks to the current wantContent (used by the device-underrun path
// in the drain, which runs off-tick). It is a no-op when there is no sync yet.
func (r *Renderer) reseekToNow() {
	sample, _, ok := r.timeline.NowSample()
	if !ok {
		return
	}
	node := r.nodeRecord()
	p := r.params()
	r.reseek(wantContent(sample, node.HWDelayUs, p.Rate))
	r.settle = settleTicks
}

// tick is the control law, run every p.Tick (doc 06 §7).
func (r *Renderer) tick() {
	p := r.params()
	info := RenderTick{Underruns: r.underruns}
	if cb := r.tickCallback(); cb != nil {
		defer func() { cb(info) }()
	}

	// (1) timeline gate.
	sample, playing, ok := r.timeline.NowSample()
	if !ok {
		info.HaveSync = false
		return // hold: no advance, no reseek.
	}
	info.HaveSync = true
	info.Playing = playing

	// (2) locate the active media for this node. Ensemble plays one continuous
	// stream per group; "active" means there is a media file selected and a reader.
	doc := r.docSnapshot()
	node := nodeRecordIn(doc, r.nodeID)
	media := activeMedia(doc)
	if media == "" || r.src == nil || !playing {
		// Gap / nothing selected / paused: feed silence (drop buffered backlog).
		r.feedSilence()
		info.Clip = media
		return
	}
	info.Clip = media

	// Refresh the channel-select + gain snapshot from the node record (cheap).
	r.applyLane(node)

	// (4) content-domain WANT (includes the HWDelayUs trim, doc 06 §3.2 / §5.3).
	want := wantContent(sample, node.HWDelayUs, p.Rate)

	// (3) stream discontinuity (streamGen change) → reseek. Also the startup case:
	// the first ok tick primes the baseline.
	gen := r.src.StreamGen()
	if !r.hadGen || gen != r.loadedGen {
		r.loadedGen = gen
		r.hadGen = true
		r.reseek(want)
		r.settle = settleTicks
		// fall through into the steady-state computation this same tick.
	}

	info.WantSample = want

	// (5) content-domain PLAYED (the corrected variable, doc 06 §3.3).
	played := r.playedContent(p)
	info.PlayedSample = played

	// (6) settle window after a reseek/load: hold the law while the buffer primes.
	if r.settle > 0 {
		r.settle--
		info.Action = drift.DriftHold
		info.RatioPPM = (r.lastRatio - 1) * 1e6
		return
	}

	// (7) error + PI law.
	errSamples := int(played - want)
	info.ErrorSamples = errSamples
	action, ratio := r.drift.Update(errSamples)
	info.Action = action
	info.RatioPPM = (ratio - 1) * 1e6

	switch action {
	case drift.DriftHold:
		r.applyRatio(ratio)
	case drift.DriftReseek:
		r.reseek(want)
		r.settle = settleTicks
	}
}

// wantContent maps the group sample index to this node's content-domain target,
// applying the HWDelayUs fixed sample offset (doc 06 §3.2 / §5.3). A positive
// HWDelayUs advances the want target so the node plays earlier in content to
// compensate a later acoustic arrival.
func wantContent(sample int64, hwDelayUs, rate int) int64 {
	hwOff := int64(math.Round(float64(hwDelayUs) * 1e-6 * float64(rate)))
	return sample + hwOff
}

// playedContent computes the content-domain progress (doc 06 §3.3): cumulative
// source frames consumed minus the source-equivalent still buffered downstream of
// the resampler (the playout ring + the device delay). Uses sink.Delay() when
// precise, else the coarse wall-time model (doc 06 §4.2).
func (r *Renderer) playedContent(p RendererParams) int64 {
	r.rmu.Lock()
	ratio := r.lastRatio
	srcConsumed := r.sourceConsumed
	r.rmu.Unlock()

	ringFrames := r.ring.Len() / p.Channels
	devFrames, ok := r.sink.Delay()
	if !ok {
		devFrames = r.coarseDeviceDelay()
	}
	bufferedContent := int64(math.Round(float64(ringFrames+devFrames) * ratio))
	return r.baseSourceConsumed + srcConsumed - bufferedContent
}

// coarseDeviceDelay estimates the device's outstanding per-channel frames when the
// sink gives no precise Delay() readback (exec sink): outstanding ≈ wall time since
// the last Write × rate. The ring backlog is subtracted SEPARATELY in playedContent,
// so this returns the DEVICE estimate only (do not double-count the ring). The bias
// is near-constant, so the PI integrator absorbs it (doc 06 §4.2).
func (r *Renderer) coarseDeviceDelay() int {
	elapsed := time.Since(r.lastWriteAt).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	return int(float64(r.params().Rate) * elapsed)
}

// applyRatio sets the resampler ratio, skipping the call when the change is below a
// small epsilon (anti-redundant write).
func (r *Renderer) applyRatio(ratio float64) {
	if r.ratioSet && math.Abs(ratio-r.lastRatio) < 1e-7 {
		return
	}
	r.rmu.Lock()
	r.resampler.SetRatio(ratio)
	r.lastRatio = ratio
	r.rmu.Unlock()
	r.ratioSet = true
}

// reseek performs a hard refill / re-baseline (doc 06 §6.1): reset the resampler +
// ring + drift loop, re-baseline the content counters to want, and start at ratio
// 1.0. Used by startup, streamGen change, and underrun. The source/origin owns the
// actual stream seek; the receiver re-primes the recv-ring (R11), so this renderer
// only re-baselines its own pipeline.
func (r *Renderer) reseek(want int64) {
	r.rmu.Lock()
	r.resampler.Reset()
	r.resampler.SetRatio(1.0)
	r.baseSourceConsumed = want
	r.sourceConsumed = 0
	r.lastRatio = 1.0
	r.rmu.Unlock()

	r.ring.Reset()
	r.drift.Reset()
	r.framesWritten = 0
	r.lastWriteAt = time.Now()
	r.ratioSet = true
}

// feedSilence is the gap/paused/not-ready hold: drop any buffered audio so the sink
// drains to silence. It does not advance the content baseline.
func (r *Renderer) feedSilence() {
	r.ring.Reset()
}

// docSnapshot returns the current ConfigDoc (empty if no accessor is wired).
func (r *Renderer) docSnapshot() state.ConfigDoc {
	if r.configDoc == nil {
		return state.ConfigDoc{}
	}
	return r.configDoc()
}

// nodeRecord returns this node's record from the current ConfigDoc snapshot.
func (r *Renderer) nodeRecord() state.NodeRecord {
	return nodeRecordIn(r.docSnapshot(), r.nodeID)
}

// applyLane snapshots the node's channel-select + gain for the producer (doc 06
// §5). An unknown Channel role degrades to stereo passthrough (ParseChannel
// returns ChannelStereo on error).
func (r *Renderer) applyLane(node state.NodeRecord) {
	role, _ := ParseChannel(node.Channel)
	g := GainLinear(node.GainDB)
	r.chMu.Lock()
	r.role = role
	r.gain = g
	r.chMu.Unlock()
}

// nodeRecordIn finds the node record for id in doc, or a zero record (which yields
// stereo passthrough at 0 dB / 0 µs — a safe default for an unknown node).
func nodeRecordIn(doc state.ConfigDoc, id string) state.NodeRecord {
	for i := range doc.Nodes {
		if doc.Nodes[i].ID == id {
			return doc.Nodes[i]
		}
	}
	return state.NodeRecord{}
}

// activeMedia returns the group media file this node should be playing, or "" when
// nothing is selected. Ensemble runs one group per node; the first group with a
// non-empty Media.File wins (the node's group membership is resolved upstream by
// P4.9, which only starts the renderer for the node's playing group).
func activeMedia(doc state.ConfigDoc) string {
	for i := range doc.Groups {
		if f := doc.Groups[i].Media.File; f != "" {
			return f
		}
	}
	return ""
}

