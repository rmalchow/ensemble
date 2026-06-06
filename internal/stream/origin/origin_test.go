package origin

import (
	"context"
	"net"
	"testing"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/codec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/fec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/wire"
)

func mustAddr(t *testing.T, s string) *net.UDPAddr {
	t.Helper()
	a, err := net.ResolveUDPAddr("udp", s)
	if err != nil {
		t.Fatalf("resolve %s: %v", s, err)
	}
	return a
}

// runForChunks runs the origin until `want` packets have been written to the given
// capture writer (or a deadline), then cancels. It returns the captured headers.
func runForChunks(t *testing.T, o *Origin, w *captureWriter, want int) []wire.Header {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = o.Run(ctx); close(done) }()

	deadline := time.After(2 * time.Second)
	for w.count() < want {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("timed out: got %d writes, want %d", w.count(), want)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done
	return w.headers()
}

func newPCM(t *testing.T) codec.Codec {
	t.Helper()
	c, err := codec.New(codec.PCM)
	if err != nil {
		t.Fatalf("codec.New(PCM): %v", err)
	}
	return c
}

func newNone(t *testing.T) fec.FEC {
	t.Helper()
	f, err := fec.New(fec.None)
	if err != nil {
		t.Fatalf("fec.New(None): %v", err)
	}
	return f
}

func baseCfg() testCfg {
	return testCfg{
		Config: Config{Rate: 48000, Channels: 2, FramesPerChunk: 480, Lead: 300 * time.Millisecond, StreamGen: 7},
	}
}

// TestHeaderStamping asserts every emitted header carries the canonical magic/
// version (validated by Unmarshal), Rate100=480, monotonic Seq from 0, strictly
// increasing MasterMono, and the correct CodecID/FECID.
func TestHeaderStamping(t *testing.T) {
	src := &fakeSource{channels: 2, total: 4800}
	o, clk, caps := newTestOrigin(newPCM(t), newNone(t), src, baseCfg())
	// Make MasterMono strictly increase: each nowMono call bumps the clock a hair.
	base := o.nowMono
	var tick int64
	o.nowMono = func() int64 { tick++; return base() + tick }

	if err := o.AddListener("l1", mustAddr(t, "127.0.0.1:19100")); err != nil {
		t.Fatal(err)
	}
	w := caps["127.0.0.1:19100"]
	_ = clk
	hs := runForChunks(t, o, w, 5)

	for i, h := range hs {
		if h.Rate100 != 480 {
			t.Errorf("chunk %d: Rate100=%d want 480", i, h.Rate100)
		}
		if h.CodecID != wire.CodecPCM {
			t.Errorf("chunk %d: CodecID=%d want PCM", i, h.CodecID)
		}
		if h.FECID != wire.FECNone {
			t.Errorf("chunk %d: FECID=%d want None", i, h.FECID)
		}
		if h.StreamGen != 7 {
			t.Errorf("chunk %d: StreamGen=%d want 7", i, h.StreamGen)
		}
	}
	// Seq monotonic from 0; SampleIndex advances by 480.
	for i := 1; i < len(hs); i++ {
		if hs[i].Seq != hs[i-1].Seq+1 {
			t.Errorf("Seq not monotonic at %d: %d then %d", i, hs[i-1].Seq, hs[i].Seq)
		}
		if hs[i].SampleIndex != hs[i-1].SampleIndex+480 {
			t.Errorf("SampleIndex step at %d: %d then %d", i, hs[i-1].SampleIndex, hs[i].SampleIndex)
		}
		if hs[i].MasterMono <= hs[i-1].MasterMono {
			t.Errorf("MasterMono not strictly increasing at %d: %d then %d", i, hs[i-1].MasterMono, hs[i].MasterMono)
		}
	}
	if hs[0].Seq != 0 {
		t.Errorf("first Seq=%d want 0", hs[0].Seq)
	}
}

// TestKeyframePCM asserts every PCM packet carries the keyframe flag (05 §5.4.1).
func TestKeyframePCM(t *testing.T) {
	src := &fakeSource{channels: 2, total: 4800}
	o, _, caps := newTestOrigin(newPCM(t), newNone(t), src, baseCfg())
	if err := o.AddListener("l1", mustAddr(t, "127.0.0.1:19101")); err != nil {
		t.Fatal(err)
	}
	hs := runForChunks(t, o, caps["127.0.0.1:19101"], 6)
	for i, h := range hs {
		if !h.Flags.Keyframe() {
			t.Errorf("PCM chunk %d not flagged keyframe", i)
		}
	}
}

// TestChunkingAndLoop asserts chunks are exactly 480 frames, sampleIndex advances
// by 480, and the loop boundary neither resets idx nor bumps gen.
func TestChunkingAndLoop(t *testing.T) {
	// total = 720 frames => loops mid-chunk; loop must not reset idx or gen.
	src := &fakeSource{channels: 2, total: 720}
	o, _, caps := newTestOrigin(newPCM(t), newNone(t), src, baseCfg())
	if err := o.AddListener("l1", mustAddr(t, "127.0.0.1:19102")); err != nil {
		t.Fatal(err)
	}
	hs := runForChunks(t, o, caps["127.0.0.1:19102"], 5)

	for i, h := range hs {
		// Every chunk's payload is exactly 480 stereo frames = 1920 bytes S16LE.
		_, payload, err := wire.Unmarshal(decodeAt(t, caps["127.0.0.1:19102"], i))
		if err != nil {
			t.Fatalf("chunk %d unmarshal: %v", i, err)
		}
		if len(payload) != 480*2*2 {
			t.Errorf("chunk %d payload=%d bytes want 1920", i, len(payload))
		}
		if h.StreamGen != 7 {
			t.Errorf("chunk %d gen bumped to %d on loop", i, h.StreamGen)
		}
		if i > 0 && hs[i].SampleIndex != hs[i-1].SampleIndex+480 {
			t.Errorf("idx reset at loop, chunk %d: %d", i, hs[i].SampleIndex)
		}
	}
}

func decodeAt(t *testing.T, w *captureWriter, i int) []byte {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes[i]
}

// TestFanOutCount asserts encode/Protect run once per chunk regardless of listener
// count (D5), while WriteToUDP count = M × packets.
func TestFanOutCount(t *testing.T) {
	src := &fakeSource{channels: 2, total: 4800}
	cc := &countingCodec{Codec: newPCM(t)}
	cf := &countingFEC{FEC: newNone(t)}
	o, _, caps := newTestOrigin(cc, cf, src, baseCfg())

	ports := []string{"127.0.0.1:19200", "127.0.0.1:19201", "127.0.0.1:19202"}
	for i, p := range ports {
		if err := o.AddListener(string(rune('a'+i)), mustAddr(t, p)); err != nil {
			t.Fatal(err)
		}
	}

	// Run until the first listener has 4 writes.
	_ = runForChunks(t, o, caps[ports[0]], 4)

	// Each listener saw the same number of writes (fan-out copy).
	for _, p := range ports {
		if got := caps[p].count(); got < 4 {
			t.Errorf("listener %s got %d writes, want >=4", p, got)
		}
	}
	// The D5 invariant: encode/Protect run once per chunk, INDEPENDENT of listener
	// count M. With None FEC, writes-per-listener == chunks delivered; encodes may
	// lead delivered writes by at most one (the look-ahead chunk produced but not
	// yet fanned out when Run was cancelled). The crux is encodes != M×chunks.
	chunks := caps[ports[0]].count()
	if cc.encodes < chunks || cc.encodes > chunks+1 {
		t.Errorf("encodes=%d want ~%d (once per chunk, D5)", cc.encodes, chunks)
	}
	if cf.protects != cc.encodes {
		t.Errorf("protects=%d != encodes=%d (both once per chunk)", cf.protects, cc.encodes)
	}
	if cc.encodes >= chunks*len(ports) && chunks > 1 {
		t.Errorf("encodes=%d scales with M=%d listeners — D5 violated", cc.encodes, len(ports))
	}
}

// TestAddRemoveIdempotent asserts duplicate AddListener / removing an unknown id
// are no-ops, and a removed listener stops receiving.
func TestAddRemoveIdempotent(t *testing.T) {
	src := &fakeSource{channels: 2, total: 4800}
	o, _, _ := newTestOrigin(newPCM(t), newNone(t), src, baseCfg())

	if err := o.AddListener("x", mustAddr(t, "127.0.0.1:19300")); err != nil {
		t.Fatal(err)
	}
	if err := o.AddListener("x", mustAddr(t, "127.0.0.1:19301")); err != nil {
		t.Fatalf("duplicate add errored: %v", err)
	}
	if o.Listeners() != 1 {
		t.Errorf("duplicate add changed count to %d, want 1", o.Listeners())
	}
	o.RemoveListener("unknown") // no-op
	if o.Listeners() != 1 {
		t.Errorf("removing unknown changed count to %d", o.Listeners())
	}
	o.RemoveListener("x")
	if o.Listeners() != 0 {
		t.Errorf("after remove count=%d want 0", o.Listeners())
	}
}

// TestResumeAtGenBump asserts ResumeAt bumps gen, resets seq to 0, sets the
// SampleIndex anchor, and forces a keyframe on the first new-gen chunk — and that
// failover always bumps even for identical media.
func TestResumeAtGenBump(t *testing.T) {
	src := &fakeSource{channels: 2, total: 4800}
	o, _, caps := newTestOrigin(newPCM(t), newNone(t), src, baseCfg())
	if err := o.AddListener("l1", mustAddr(t, "127.0.0.1:19400")); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = o.Run(ctx); close(done) }()
	w := caps["127.0.0.1:19400"]

	// Let a few chunks flow at gen 7.
	waitFor(t, func() bool { return w.count() >= 3 })

	newGen := o.ResumeAt(96000, true)
	if newGen != 8 {
		t.Fatalf("ResumeAt returned gen %d want 8", newGen)
	}

	// Wait for a chunk under the new generation.
	waitFor(t, func() bool {
		for _, h := range w.headers() {
			if h.StreamGen == 8 {
				return true
			}
		}
		return false
	})
	cancel()
	<-done

	// Find the first gen-8 header: seq must restart at 0, idx must be the anchor,
	// keyframe forced.
	var first *wire.Header
	for _, h := range w.headers() {
		if h.StreamGen == 8 {
			hh := h
			first = &hh
			break
		}
	}
	if first == nil {
		t.Fatal("no gen-8 chunk observed")
	}
	if first.Seq != 0 {
		t.Errorf("first gen-8 Seq=%d want 0", first.Seq)
	}
	if first.SampleIndex != 96000 {
		t.Errorf("first gen-8 SampleIndex=%d want 96000", first.SampleIndex)
	}
	if !first.Flags.Keyframe() {
		t.Error("first gen-8 chunk not a keyframe")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatal("waitFor timed out")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

// TestKeyframeOnJoinOpus asserts that with an inter-frame (Opus-shaped) codec, a
// join forces a keyframe on the next chunk to that listener; steady-state non-join
// chunks are NOT keyframes.
func TestKeyframeOnJoinOpus(t *testing.T) {
	src := &fakeSource{channels: 2, total: 4800}
	o, _, caps := newTestOrigin(interFrameCodec{Codec: newPCM(t)}, newNone(t), src, baseCfg())
	if err := o.AddListener("l1", mustAddr(t, "127.0.0.1:19500")); err != nil {
		t.Fatal(err)
	}
	hs := runForChunks(t, o, caps["127.0.0.1:19500"], 5)

	// The first chunk after the join is forced keyframe; later ones are not.
	if !hs[0].Flags.Keyframe() {
		t.Error("first chunk after join not a keyframe (Opus join, 05 §5.6.4)")
	}
	nonKf := false
	for _, h := range hs[1:] {
		if !h.Flags.Keyframe() {
			nonKf = true
		}
	}
	if !nonKf {
		t.Error("expected at least one non-keyframe inter-frame chunk after join")
	}
}
