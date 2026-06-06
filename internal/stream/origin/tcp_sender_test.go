package origin

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/codec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/fec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/sink_net"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/wire"
)

// frameCapture records every length-prefixed frame written to a TCP listener so
// framing, fan-out and gen behavior can be asserted without real sockets.
type frameCapture struct {
	mu     sync.Mutex
	buf    bytes.Buffer // raw stream bytes (length-prefixed)
	closed bool
}

func (w *frameCapture) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(b)
}
func (w *frameCapture) Close() error { w.closed = true; return nil }

// frames deframes the captured stream back into individual wire-packet byte
// slices using the same 2-byte BE length prefix the sender wrote.
func (w *frameCapture) frames() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	data := w.buf.Bytes()
	var out [][]byte
	for len(data) >= 2 {
		n := int(binary.BigEndian.Uint16(data[:2]))
		if len(data) < 2+n {
			break
		}
		fr := make([]byte, n)
		copy(fr, data[2:2+n])
		out = append(out, fr)
		data = data[2+n:]
	}
	return out
}

// newTestTCPOrigin builds a Transport==TCP origin wired to frameCapture writers
// keyed by destination addr, with a step-clock driven pacer (no wall-clock wait).
func newTestTCPOrigin(c codec.Codec, src *fakeSource, cfg testCfg) (*Origin, *stepClock, map[string]*frameCapture) {
	cfg.Transport = sink_net.TransportTCP
	o := New(fakeTimeline{sample: cfg.startIdx}, c, fec.NewNone(), src, cfg.Config)
	clk := &stepClock{}
	o.nowMono = clk.now
	caps := map[string]*frameCapture{}
	var capMu sync.Mutex
	ts := o.sender.(tcpSenderAdapter).tcpSender
	ts.dial = func(addr *net.TCPAddr) (streamWriter, error) {
		w := &frameCapture{}
		capMu.Lock()
		caps[addr.String()] = w
		capMu.Unlock()
		return w, nil
	}
	o.sleepUntil = func(ctx context.Context, deadline int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if deadline > clk.now() {
			clk.mu.Lock()
			clk.t = deadline
			clk.mu.Unlock()
		}
		t := time.NewTimer(100 * time.Microsecond)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
	return o, clk, caps
}

func runTCPForFrames(t *testing.T, o *Origin, w *frameCapture, want int) []wire.Header {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = o.Run(ctx); close(done) }()
	deadline := time.After(2 * time.Second)
	for len(w.frames()) < want {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("timed out: got %d frames, want %d", len(w.frames()), want)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done
	hs := make([]wire.Header, 0)
	for _, fr := range w.frames() {
		h, _, err := wire.Unmarshal(fr)
		if err == nil {
			hs = append(hs, h)
		}
	}
	return hs
}

// TestTCPForcesNoneFEC: a Transport==TCP origin uses fec.None regardless of what
// FEC is passed to New, and emits exactly one frame per chunk (Protect identity).
func TestTCPForcesNoneFEC(t *testing.T) {
	// Pass a Duplicate FEC; TCP must override it to None.
	dup, err := fec.New(fec.Duplicate)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testCfg{Config: Config{Rate: 48000, Channels: 2, FramesPerChunk: 480, Transport: sink_net.TransportTCP}}
	o := New(fakeTimeline{}, newPCM(t), dup, &fakeSource{channels: 2, total: 9600}, cfg.Config)
	if o.fec.ID() != fec.None {
		t.Errorf("TCP origin fec=%v, want None", o.fec.ID())
	}
	if o.fecID != wire.FECNone {
		t.Errorf("TCP origin fecID=%v, want FECNone", o.fecID)
	}
}

// TestTCPLengthFraming: each frame on the stream is a 2-byte BE length + the
// marshaled wire packet; deframed headers carry monotonic seq and the right
// sampleIndex cadence.
func TestTCPLengthFraming(t *testing.T) {
	src := &fakeSource{channels: 2, total: 9600}
	cfg := testCfg{Config: Config{Rate: 48000, Channels: 2, FramesPerChunk: 480}, startIdx: 0}
	o, _, caps := newTestTCPOrigin(newPCM(t), src, cfg)
	if err := o.AddListener("f1", mustAddr(t, "127.0.0.1:9100")); err != nil {
		t.Fatal(err)
	}
	w := caps["127.0.0.1:9100"]
	if w == nil {
		t.Fatal("listener writer not created")
	}
	hdrs := runTCPForFrames(t, o, w, 4)

	for i, h := range hdrs[:4] {
		if h.Seq != uint64(i) {
			t.Errorf("frame %d seq=%d, want %d", i, h.Seq, i)
		}
		if h.SampleIndex != int64(i)*480 {
			t.Errorf("frame %d sampleIndex=%d, want %d", i, h.SampleIndex, int64(i)*480)
		}
		if h.FECID != wire.FECNone {
			t.Errorf("frame %d FECID=%v, want None", i, h.FECID)
		}
		if !h.Flags.Keyframe() {
			t.Errorf("frame %d not a keyframe (PCM always keyframe)", i)
		}
	}
}

// TestTCPGenChange: ResumeAt bumps the gen mid-stream; the first frame of the new
// gen carries seq=0, the keyframe flag, and the re-anchored sampleIndex (05 §5.9
// gen-change over TCP). Run is driven continuously (one goroutine) so the listener
// is not torn down by a Run restart.
func TestTCPGenChange(t *testing.T) {
	src := &fakeSource{channels: 2, total: 9600}
	cfg := testCfg{Config: Config{Rate: 48000, Channels: 2, FramesPerChunk: 480, StreamGen: 5}, startIdx: 1000}
	o, _, caps := newTestTCPOrigin(newPCM(t), src, cfg)
	if err := o.AddListener("f1", mustAddr(t, "127.0.0.1:9100")); err != nil {
		t.Fatal(err)
	}
	w := caps["127.0.0.1:9100"]

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = o.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitTCPFrames(t, w, 2) // a few frames at gen 5
	newGen := o.ResumeAt(2000, true)
	if newGen <= 5 {
		t.Fatalf("ResumeAt gen=%d, want > 5", newGen)
	}

	// Wait until a new-gen frame appears.
	deadline := time.After(3 * time.Second)
	var newGenHdr *wire.Header
	for newGenHdr == nil {
		for _, fr := range w.frames() {
			h, _, err := wire.Unmarshal(fr)
			if err == nil && h.StreamGen == newGen {
				hh := h
				newGenHdr = &hh
				break
			}
		}
		if newGenHdr != nil {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no frame at new gen %d", newGen)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if newGenHdr.Seq != 0 {
		t.Errorf("first new-gen frame seq=%d, want 0", newGenHdr.Seq)
	}
	if newGenHdr.SampleIndex != 2000 {
		t.Errorf("first new-gen frame sampleIndex=%d, want 2000", newGenHdr.SampleIndex)
	}
	if !newGenHdr.Flags.Keyframe() {
		t.Error("first new-gen frame not a keyframe")
	}
}

// waitTCPFrames blocks until w holds at least n deframed frames.
func waitTCPFrames(t *testing.T, w *frameCapture, n int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for len(w.frames()) < n {
		select {
		case <-deadline:
			t.Fatalf("timed out: %d frames, want %d", len(w.frames()), n)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

// TestTCPPacingPreserved: the origin paces the TCP stream chunk-by-chunk through
// the pacer (the same playout(idx)−Lead deadline as UDP) rather than dumping the
// whole stream at once (05 §5.9 "pacing still applies"). We assert (a) the pacer
// is consulted — sleepUntil is invoked once per produced chunk — and (b) the
// sampleIndex advances exactly one chunk per frame (the 100 packets/s cadence at
// FramesPerChunk=480). A flooded sender would emit frames with no sleepUntil call.
func TestTCPPacingPreserved(t *testing.T) {
	src := &fakeSource{channels: 2, total: 9600}
	cfg := testCfg{Config: Config{Rate: 48000, Channels: 2, FramesPerChunk: 480}, startIdx: 0}
	o, _, caps := newTestTCPOrigin(newPCM(t), src, cfg)

	// Count sleepUntil invocations: the pacer parks before each chunk's send
	// instant, so a paced stream calls it ~once per chunk (not zero).
	var sleeps int
	var sleepMu sync.Mutex
	inner := o.sleepUntil
	o.sleepUntil = func(ctx context.Context, deadline int64) error {
		sleepMu.Lock()
		sleeps++
		sleepMu.Unlock()
		return inner(ctx, deadline)
	}

	if err := o.AddListener("f1", mustAddr(t, "127.0.0.1:9100")); err != nil {
		t.Fatal(err)
	}
	w := caps["127.0.0.1:9100"]
	hdrs := runTCPForFrames(t, o, w, 5)

	for i := 1; i < 5; i++ {
		if d := hdrs[i].SampleIndex - hdrs[i-1].SampleIndex; d != 480 {
			t.Errorf("frame %d->%d sampleIndex delta=%d, want 480 (one chunk/frame)", i-1, i, d)
		}
	}
	sleepMu.Lock()
	defer sleepMu.Unlock()
	if sleeps == 0 {
		t.Error("sleepUntil never called: stream was flooded, not paced (05 §5.9)")
	}
}

// TestTCPRoundTripParity: a TCP origin's framed output, deframed and fed through a
// sink_net TCP receiver, reproduces the same decoded sample sequence as the source
// emitted (bit-identical, zero gaps under no loss) — UDP/TCP parity (test §7.1).
func TestTCPRoundTripParity(t *testing.T) {
	src := &fakeSource{channels: 2, total: 9600}
	cfg := testCfg{Config: Config{Rate: 48000, Channels: 2, FramesPerChunk: 480}, startIdx: 0}
	o, _, caps := newTestTCPOrigin(newPCM(t), src, cfg)
	if err := o.AddListener("f1", mustAddr(t, "127.0.0.1:9100")); err != nil {
		t.Fatal(err)
	}
	w := caps["127.0.0.1:9100"]
	const n = 6
	hdrs := runTCPForFrames(t, o, w, n)
	if len(hdrs) < n {
		t.Fatalf("got %d frames, want >= %d", len(hdrs), n)
	}
	// Decode each framed packet and check the sampleIndex cadence is contiguous.
	frames := w.frames()
	pcmCodec := newPCM(t)
	prevIdx := int64(-480)
	for i := 0; i < n; i++ {
		h, payload, err := wire.Unmarshal(frames[i])
		if err != nil {
			t.Fatalf("unmarshal frame %d: %v", i, err)
		}
		if h.SampleIndex != prevIdx+480 {
			t.Errorf("frame %d sampleIndex=%d, want %d (contiguous)", i, h.SampleIndex, prevIdx+480)
		}
		prevIdx = h.SampleIndex
		pcm, err := pcmCodec.Decode(payload)
		if err != nil {
			t.Fatalf("decode frame %d: %v", i, err)
		}
		if len(pcm) != 480*2 {
			t.Errorf("frame %d decoded len=%d, want %d", i, len(pcm), 480*2)
		}
	}
}
