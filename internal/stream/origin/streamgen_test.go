package origin

import (
	"context"
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/streamgen"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/wire"
)

// firstHeaderOfGen returns the first captured header carrying gen, or nil.
func firstHeaderOfGen(w *captureWriter, gen uint64) *wire.Header {
	for _, h := range w.headers() {
		if h.StreamGen == gen {
			hh := h
			return &hh
		}
	}
	return nil
}

// TestBumpSeek asserts Bump(ReasonSeek) advances the generation, restarts seq at
// 0, re-anchors SampleIndex at the seek target, and forces a keyframe on the first
// chunk of the new generation (doc 05 §5.8).
func TestBumpSeek(t *testing.T) {
	src := &fakeSource{channels: 2, total: 4800}
	o, _, caps := newTestOrigin(newPCM(t), newNone(t), src, baseCfg())
	if err := o.AddListener("l1", mustAddr(t, "127.0.0.1:19600")); err != nil {
		t.Fatal(err)
	}
	w := caps["127.0.0.1:19600"]

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = o.Run(ctx); close(done) }()

	waitFor(t, func() bool { return w.count() >= 3 })

	const seekTarget = 144000
	newGen := o.Bump(streamgen.ReasonSeek, seekTarget, true)
	if newGen != 8 {
		t.Fatalf("Bump(seek) returned gen %d want 8", newGen)
	}
	waitFor(t, func() bool { return firstHeaderOfGen(w, 8) != nil })
	cancel()
	<-done

	first := firstHeaderOfGen(w, 8)
	if first == nil {
		t.Fatal("no gen-8 chunk observed after seek bump")
	}
	if first.Seq != 0 {
		t.Errorf("first gen-8 Seq=%d want 0", first.Seq)
	}
	if first.SampleIndex != seekTarget {
		t.Errorf("first gen-8 SampleIndex=%d want %d (seek target)", first.SampleIndex, seekTarget)
	}
	if !first.Flags.Keyframe() {
		t.Error("first gen-8 chunk not a keyframe")
	}
}

// TestBumpMediaChangeContinues asserts a media change carries the CONTINUING
// timeline position (not a reset to 0) when so directed (doc 04 §4.3.3 / §5.8).
func TestBumpMediaChangeContinues(t *testing.T) {
	c := streamgen.NewController(2)
	g := c.Bump(streamgen.ReasonMediaChange, 50_000, true)
	if g.Gen != 3 || g.FirstSampleIndex != 50_000 {
		t.Errorf("media-change gen=%d idx=%d want 3/50000", g.Gen, g.FirstSampleIndex)
	}
}

// TestLoopNeverBumps asserts a source loop boundary (EOF→0) neither advances gen
// nor resets SampleIndex: the timeline is continuous (doc 05 §5.2.1). The source
// loops mid-chunk (total < chunk span over the run) so the loop seam is crossed
// repeatedly while no Bump is called.
func TestLoopNeverBumps(t *testing.T) {
	src := &fakeSource{channels: 2, total: 720} // loops every 1.5 chunks
	o, _, caps := newTestOrigin(newPCM(t), newNone(t), src, baseCfg())
	if err := o.AddListener("l1", mustAddr(t, "127.0.0.1:19601")); err != nil {
		t.Fatal(err)
	}
	hs := runForChunks(t, o, caps["127.0.0.1:19601"], 8)
	for i, h := range hs {
		if h.StreamGen != 7 {
			t.Errorf("chunk %d gen=%d, loop must NOT bump (want 7)", i, h.StreamGen)
		}
		if i > 0 && h.SampleIndex != hs[i-1].SampleIndex+480 {
			t.Errorf("chunk %d idx=%d reset on loop (prev %d)", i, h.SampleIndex, hs[i-1].SampleIndex)
		}
	}
	if o.StreamGen() != 7 {
		t.Errorf("StreamGen=%d after loops, want 7 (no bump)", o.StreamGen())
	}
}

// TestResumeAtBumpsMasterChange asserts ResumeAt bumps with ReasonMasterChange,
// preserves the resume sampleIndex on the first new-gen chunk, and preserves the
// caller's gen authority (returns gen+1). Failover ALWAYS bumps even for identical
// media (D22).
func TestResumeAtBumpsMasterChange(t *testing.T) {
	src := &fakeSource{channels: 2, total: 4800}
	o, _, caps := newTestOrigin(newPCM(t), newNone(t), src, baseCfg())
	if err := o.AddListener("l1", mustAddr(t, "127.0.0.1:19602")); err != nil {
		t.Fatal(err)
	}
	w := caps["127.0.0.1:19602"]

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = o.Run(ctx); close(done) }()
	waitFor(t, func() bool { return w.count() >= 2 })

	const resumeAt = 72000
	if g := o.ResumeAt(resumeAt, true); g != 8 {
		t.Fatalf("ResumeAt returned %d want 8", g)
	}
	waitFor(t, func() bool { return firstHeaderOfGen(w, 8) != nil })
	cancel()
	<-done

	first := firstHeaderOfGen(w, 8)
	if first.SampleIndex != resumeAt {
		t.Errorf("resume SampleIndex=%d want %d (continuing/current)", first.SampleIndex, resumeAt)
	}
	if first.Seq != 0 || !first.Flags.Keyframe() {
		t.Errorf("resume first chunk seq=%d keyframe=%v want 0/true", first.Seq, first.Flags.Keyframe())
	}
}

// TestLateJoinKeyframeNoGenBump asserts a late join forces a keyframe for the new
// listener at the next chunk boundary WITHOUT bumping the generation (doc 05
// §5.6.4) — distinct from a Bump. With an inter-frame (Opus-shaped) codec the
// joiner's first chunk is a keyframe; the generation is unchanged.
func TestLateJoinKeyframeNoGenBump(t *testing.T) {
	src := &fakeSource{channels: 2, total: 4800}
	o, _, caps := newTestOrigin(interFrameCodec{Codec: newPCM(t)}, newNone(t), src, baseCfg())

	if err := o.AddListener("late", mustAddr(t, "127.0.0.1:19603")); err != nil {
		t.Fatal(err)
	}
	hs := runForChunks(t, o, caps["127.0.0.1:19603"], 4)

	if !hs[0].Flags.Keyframe() {
		t.Error("late joiner first chunk not a keyframe (05 §5.6.4)")
	}
	// Generation never changed: a per-join keyframe is not a generation change.
	for i, h := range hs {
		if h.StreamGen != 7 {
			t.Errorf("chunk %d gen=%d, late join must NOT bump gen (want 7)", i, h.StreamGen)
		}
	}
	if o.StreamGen() != 7 {
		t.Errorf("StreamGen=%d after late join, want 7", o.StreamGen())
	}
	// A non-keyframe steady-state chunk exists after the join (proves the keyframe
	// was a one-shot for the join, not every chunk).
	sawNonKf := false
	for _, h := range hs[1:] {
		if !h.Flags.Keyframe() {
			sawNonKf = true
		}
	}
	if !sawNonKf {
		t.Error("expected a non-keyframe inter-frame chunk after the join keyframe")
	}
}
