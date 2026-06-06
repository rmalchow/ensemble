// Package render is the output-half render loop of an Ensemble player node: the
// two goroutines under Renderer.Run(ctx) that map the group's Timeline.NowSample()
// to this node's physical playout, drive the Resampler against an AudioSink, and
// hold sub-millisecond inter-node sync with the CORRECTED CONTENT-DOMAIN drift PI
// loop (doc 06 §2,§3). It owns the 20 ms control tick, the wantContent/playedContent
// computation (the HWDelayUs sample bias and the Delay()-aware backlog subtraction),
// per-node channel-select / GainDB, and the reseek primitive that handles startup,
// streamGen change, and underrun.
//
// Pipeline at a glance (doc 06 §0):
//
//	(from 05: udp → fec → decode) ─▶ recv-ring ─┐  producer goroutine
//	                                            ▼
//	            ┌────────────────────────────────────────────────┐
//	            │ Resampler (near-unity, ratio = content per      │
//	            │   output frame; §3 actuator)                    │
//	            │      │                                          │
//	            │      ▼                                          │
//	            │ channel-select + GainDB (§5)                    │
//	            │      │                                          │
//	            │      ▼                                          │
//	            │ playout Ring (~LeadMs jitter buffer)            │
//	            └────────────────────────────────────────────────┘
//	                            │  consumer/control goroutine
//	                            ▼
//	            AudioSink.Write (blocks = playout pacing)
//	                            │
//	   control tick (20 ms) ────┤  reads NowSample()+Delay()
//	                            ▼
//	                       DAC ─▶ speaker
//
// ─────────────────────────────────────────────────────────────────────────────
// REGULATED VARIABLE IS CONTENT-DOMAIN — NEVER framesWritten − Delay() (doc 06 §3).
// ─────────────────────────────────────────────────────────────────────────────
//
// The mpvsync renderer regulated an OUTPUT-domain error
//
//	played_output = framesWritten(handed to sink) − Delay()      // THE BUG
//
// The actuator (the resample ratio) is content-per-output-frame: it changes how
// fast the resampler CONSUMES SOURCE, not how fast the DAC drains output (the
// crystal does that, fixed). So the loop's gain on played_output was effectively
// zero — it rode the ±MaxPPM clamp and relied on periodic reseeks. THE FIX is to
// regulate cumulative source frames consumed minus the source-equivalent still
// buffered downstream:
//
//	playedContent = baseSourceConsumed + sourceConsumed
//	              − round((ringFrames + devFrames) × ratio)
//	wantContent   = NowSample().sample + round(HWDelayUs·1e-6·Rate)
//	errSamples    = int(playedContent − wantContent)   // + ⇒ ahead ⇒ slow (ratio<1)
//
// The ratio now sits directly in d(playedContent)/dt ≈ ratio·f_out, so the PI loop
// has nonzero loop gain and converges to errSamples → 0 with the integrator holding
// the steady crystal drift (a NON-clamped value). A future maintainer must NOT
// silently reintroduce the output-domain formula: see the regression-guard
// convergence harness in renderer_test.go (doc 06 §3.6 / A.4).
package render
