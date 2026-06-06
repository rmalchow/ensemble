package sink_net

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/allowlist"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/audio/ring"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/codec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/fec"
)

const (
	tFrames   = 480
	tChannels = 2
)

func newPCM(t *testing.T) codec.Codec {
	t.Helper()
	c, err := codec.New(codec.PCM)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newNone(t *testing.T) fec.FEC {
	t.Helper()
	f, err := fec.New(fec.None)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// allowingSet returns a *allowlist.Set that permits the given IPs (via the live
// member path, which is the realtime-plane gate, 07 §3.1).
func allowingSet(ips ...string) *allowlist.Set {
	s := allowlist.New()
	live := make([]allowlist.MemberAddr, 0, len(ips))
	for _, ip := range ips {
		live = append(live, allowlist.MemberAddr{Addr: netip.MustParseAddr(ip)})
	}
	s.Update(state.ConfigDoc{}, live)
	return s
}

func denyingSet() *allowlist.Set { return allowlist.New() } // denies all until Update

// newTestReceiver wires a receiver with a capturePush instead of the ring adapter
// so PushAt calls are observable; cfg defaults to the canonical profile.
func newTestReceiver(t *testing.T, allow *allowlist.Set, cfg Config) (*Receiver, *capturePush) {
	t.Helper()
	r := New(newPCM(t), newNone(t), ring.NewRing(48000*tChannels), allow, cfg)
	cap := &capturePush{}
	r.push = cap
	return r, cap
}

func canonCfg() Config {
	return Config{Rate: 48000, Channels: tChannels, FramesPerChunk: tFrames, WindowPackets: 32}
}

// TestInOrderHappyPath: a stream of source packets → one PushAt per chunk with the
// right sampleIndex and decoded length = 480×channels.
func TestInOrderHappyPath(t *testing.T) {
	r, cap := newTestReceiver(t, allowingSet("127.0.0.1"), canonCfg())
	for s := uint64(0); s < 5; s++ {
		buf := buildPacket(0, s, int64(s)*tFrames, tFrames, tChannels, 0.5)
		r.handle(buf, loopbackAddr)
	}
	if len(cap.idx) != 5 {
		t.Fatalf("pushes=%d want 5", len(cap.idx))
	}
	for i := range cap.idx {
		if cap.idx[i] != int64(i)*tFrames {
			t.Errorf("push %d sampleIndex=%d want %d", i, cap.idx[i], int64(i)*tFrames)
		}
		if len(cap.pcm[i]) != tFrames*tChannels {
			t.Errorf("push %d len=%d want %d", i, len(cap.pcm[i]), tFrames*tChannels)
		}
	}
}

// TestReceiverReorder: packets fed out of seq order are pushed in seq order.
func TestReceiverReorder(t *testing.T) {
	r, cap := newTestReceiver(t, allowingSet("127.0.0.1"), canonCfg())
	order := []uint64{0, 2, 1, 3}
	for _, s := range order {
		r.handle(buildPacket(0, s, int64(s)*tFrames, tFrames, tChannels, 0.25), loopbackAddr)
	}
	want := []int64{0, tFrames, 2 * tFrames, 3 * tFrames}
	if len(cap.idx) != len(want) {
		t.Fatalf("pushes=%v want indices %v", cap.idx, want)
	}
	for i := range want {
		if cap.idx[i] != want[i] {
			t.Errorf("push %d idx=%d want %d (reorder)", i, cap.idx[i], want[i])
		}
	}
}

// TestReceiverDedupe: a seq delivered twice pushes once.
func TestReceiverDedupe(t *testing.T) {
	r, cap := newTestReceiver(t, allowingSet("127.0.0.1"), canonCfg())
	r.handle(buildPacket(0, 0, 0, tFrames, tChannels, 0.1), loopbackAddr)
	r.handle(buildPacket(0, 0, 0, tFrames, tChannels, 0.1), loopbackAddr) // dup
	if len(cap.idx) != 1 {
		t.Errorf("dedupe failed: pushes=%d want 1", len(cap.idx))
	}
}

// TestLateDrop: a packet behind the frontier is dropped, never pushed.
func TestLateDrop(t *testing.T) {
	r, cap := newTestReceiver(t, allowingSet("127.0.0.1"), canonCfg())
	r.handle(buildPacket(0, 5, 5*tFrames, tFrames, tChannels, 0.1), loopbackAddr) // primes frontier 5
	before := len(cap.idx)
	r.handle(buildPacket(0, 3, 3*tFrames, tFrames, tChannels, 0.1), loopbackAddr) // late
	if len(cap.idx) != before {
		t.Errorf("late packet was pushed: pushes grew from %d to %d", before, len(cap.idx))
	}
}

// TestUnrecoverableGapConceal: a gap that ages out → exactly one concealment chunk
// at that sampleIndex; subsequent audio is not shifted.
func TestUnrecoverableGapConceal(t *testing.T) {
	cfg := canonCfg()
	cfg.WindowPackets = 4 // small window to force the slide
	r, cap := newTestReceiver(t, allowingSet("127.0.0.1"), cfg)

	r.handle(buildPacket(0, 0, 0, tFrames, tChannels, 0.5), loopbackAddr) // releases
	for s := uint64(2); s <= 6; s++ {                                     // seq 1 missing
		r.handle(buildPacket(0, s, int64(s)*tFrames, tFrames, tChannels, 0.5), loopbackAddr)
	}
	// Expected pushes: 0 (real), 1 (conceal), 2..6 (real) = 7, all aligned.
	wantIdx := []int64{0, 1 * tFrames, 2 * tFrames, 3 * tFrames, 4 * tFrames, 5 * tFrames, 6 * tFrames}
	if len(cap.idx) != len(wantIdx) {
		t.Fatalf("pushes=%v want %v", cap.idx, wantIdx)
	}
	for i := range wantIdx {
		if cap.idx[i] != wantIdx[i] {
			t.Errorf("push %d idx=%d want %d", i, cap.idx[i], wantIdx[i])
		}
	}
	// The concealed chunk (index 1) is silence.
	for _, v := range cap.pcm[1] {
		if v != 0 {
			t.Errorf("conceal chunk not silence: %v", v)
			break
		}
	}
}

// TestGenChangeUp: hdr.StreamGen > r.gen flushes and adopts; subsequent same-gen
// packets flow.
func TestGenChangeUp(t *testing.T) {
	r, cap := newTestReceiver(t, allowingSet("127.0.0.1"), canonCfg())
	r.handle(buildPacket(3, 0, 0, tFrames, tChannels, 0.5), loopbackAddr)
	r.handle(buildPacket(3, 1, tFrames, tFrames, tChannels, 0.5), loopbackAddr)
	if r.StreamGen() != 3 {
		t.Fatalf("gen=%d want 3", r.StreamGen())
	}
	// New, higher generation restarting at seq 0, sampleIndex 96000.
	r.handle(buildPacket(5, 0, 96000, tFrames, tChannels, 0.5), loopbackAddr)
	if r.StreamGen() != 5 {
		t.Errorf("gen=%d want 5 after up-change", r.StreamGen())
	}
	last := cap.idx[len(cap.idx)-1]
	if last != 96000 {
		t.Errorf("first new-gen push idx=%d want 96000", last)
	}
}

// TestGenChangeDown: hdr.StreamGen < r.gen packets are dropped.
func TestGenChangeDown(t *testing.T) {
	r, cap := newTestReceiver(t, allowingSet("127.0.0.1"), canonCfg())
	r.handle(buildPacket(5, 0, 0, tFrames, tChannels, 0.5), loopbackAddr)
	pushesAfterGen5 := len(cap.idx)
	r.handle(buildPacket(2, 9, 9*tFrames, tFrames, tChannels, 0.5), loopbackAddr) // stale
	if len(cap.idx) != pushesAfterGen5 {
		t.Errorf("stale prior-gen packet was pushed (pushes %d→%d)", pushesAfterGen5, len(cap.idx))
	}
	if r.StreamGen() != 5 {
		t.Errorf("gen=%d want 5 (unchanged by stale)", r.StreamGen())
	}
}

// TestAllowlistGate: a datagram from a disallowed source is dropped before decode.
func TestAllowlistGate(t *testing.T) {
	r, cap := newTestReceiver(t, denyingSet(), canonCfg())
	r.handle(buildPacket(0, 0, 0, tFrames, tChannels, 0.5), loopbackAddr)
	if len(cap.idx) != 0 {
		t.Errorf("disallowed source produced %d pushes, want 0", len(cap.idx))
	}
}

// TestBadPacket: wrong magic / version / truncated → dropped without panic.
func TestBadPacket(t *testing.T) {
	r, cap := newTestReceiver(t, allowingSet("127.0.0.1"), canonCfg())
	cases := [][]byte{
		nil,
		{0x00},           // too short
		make([]byte, 44), // zero header: bad magic
		append([]byte("XXXX"), make([]byte, 60)...), // bad magic
	}
	for i, buf := range cases {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Errorf("case %d panicked: %v", i, rec)
				}
			}()
			r.handle(buf, loopbackAddr)
		}()
	}
	if len(cap.idx) != 0 {
		t.Errorf("bad packets produced %d pushes, want 0", len(cap.idx))
	}
}

// TestFlushAndReprime: drops window+FEC+ring tail and re-primes cleanly.
func TestFlushAndReprime(t *testing.T) {
	r, cap := newTestReceiver(t, allowingSet("127.0.0.1"), canonCfg())
	r.handle(buildPacket(0, 0, 0, tFrames, tChannels, 0.5), loopbackAddr)
	r.handle(buildPacket(0, 1, tFrames, tFrames, tChannels, 0.5), loopbackAddr)

	r.FlushAndReprime()
	if _, _, _, _, ok := r.LatestChunkMeta(); ok {
		t.Error("LatestChunkMeta ok after flush, want false")
	}

	// A fresh stream at a high seq re-primes without late-dropping.
	before := len(cap.idx)
	r.handle(buildPacket(0, 100, 100*tFrames, tFrames, tChannels, 0.5), loopbackAddr)
	if len(cap.idx) != before+1 {
		t.Errorf("post-flush re-prime pushed %d, want 1 new", len(cap.idx)-before)
	}
}

// TestLatestChunkMeta: the receiver exposes the newest accepted chunk's anchor for
// the FollowerTimeline.
func TestLatestChunkMeta(t *testing.T) {
	// LeadMs=0→default 300; one chunk is below the prime lead, so to exercise the
	// meta-anchor projection (not the prime gate) use a 1-frame lead so the first
	// chunk both anchors and primes. The prime gate itself is covered in
	// reprime_test.go.
	cfg := canonCfg()
	cfg.LeadMs = 0 // defaults to 300 ms; override the prime target after construction
	r, _ := newTestReceiver(t, allowingSet("127.0.0.1"), cfg)
	r.primeTarget = 1 // one chunk fills the lead so playout enables immediately
	r.handle(buildPacket(4, 0, 1000, tFrames, tChannels, 0.5), loopbackAddr)
	idx, mono, gen, playing, ok := r.LatestChunkMeta()
	if !ok {
		t.Fatal("LatestChunkMeta not ok after a chunk + prime")
	}
	if idx != 1000 || gen != 4 || !playing {
		t.Errorf("meta=(idx %d, gen %d, playing %v) want (1000, 4, true)", idx, gen, playing)
	}
	_ = mono
}

// TestLoopbackRoundTrip wires a real origin-shaped UDP send → real Receiver.Run →
// captured pushes, asserting gap-free, correctly-indexed delivery over a real
// socket (the unit-level P4 acceptance proxy). It uses real codec.PCM + fec.None.
func TestLoopbackRoundTrip(t *testing.T) {
	// Bind the receiver socket.
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	r, cap := newTestReceiver(t, allowingSet("127.0.0.1"), canonCfg())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx, conn); close(done) }()

	// Send N in-order packets to the receiver's bound port from 127.0.0.1.
	dst := conn.LocalAddr().(*net.UDPAddr)
	send, err := net.DialUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, dst)
	if err != nil {
		t.Fatal(err)
	}
	defer send.Close()

	const n = 10
	for s := uint64(0); s < n; s++ {
		buf := buildPacket(0, s, int64(s)*tFrames, tFrames, tChannels, float32(s)/256)
		if _, err := send.Write(buf); err != nil {
			t.Fatal(err)
		}
	}

	// Wait for all pushes (UDP loopback is reliable + ordered in practice).
	deadline := time.After(2 * time.Second)
	for cap.len() < n {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("round-trip: got %d pushes, want %d", cap.len(), n)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done

	for i := 0; i < n; i++ {
		idx, pcm := cap.at(i)
		if idx != int64(i)*tFrames {
			t.Errorf("round-trip push %d idx=%d want %d", i, idx, int64(i)*tFrames)
		}
		if len(pcm) != tFrames*tChannels {
			t.Errorf("round-trip push %d len=%d want %d", i, len(pcm), tFrames*tChannels)
		}
	}
}
