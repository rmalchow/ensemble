package clock

import (
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"ensemble/internal/stream"
)

// fakeClock is a monotonic-ish test clock. base is the "master" reference; each
// tick of advance moves it forward. skew is added on reads through skewedNow to
// model a node whose local clock is offset from the master by a fixed amount.
type fakeClock struct {
	ns atomic.Int64
}

func (c *fakeClock) now() int64      { return c.ns.Add(1) } // advance on every read
func (c *fakeClock) set(v int64)     { c.ns.Store(v) }
func (c *fakeClock) advance(d int64) { c.ns.Add(d) }
func (c *fakeClock) read() int64     { return c.ns.Load() }
func skewed(c *fakeClock, sk int64) nowFunc {
	return func() int64 { return c.ns.Add(1) + sk }
}

// testMux binds a loopback UDP socket and wraps it in a stream.Mux (running).
func testMux(t *testing.T) *stream.Mux {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	m := stream.NewMux(conn, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.Run()
	t.Cleanup(func() { m.Close() })
	return m
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// waitSync polls until the follower reports synced or the deadline passes.
func waitSync(t *testing.T, f *Follower, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, ok := f.MasterNow(); ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("follower did not sync within %v (stats=%+v)", d, f.Stats())
}

func TestServerReplyEchoesAndStamps(t *testing.T) {
	srvMux := testMux(t)
	clk := &fakeClock{}
	clk.set(1000)
	s := &Server{mux: srvMux, now: clk.now, log: discardLog()}
	s.Start()

	// A raw client socket to send a request and read the reply.
	cli, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("client ListenUDP: %v", err)
	}
	defer cli.Close()

	var req [packetSize]byte
	encodeClock(req[:], stream.TypeClockReq, 5, 77, 424242, 0, 0)
	if _, err := cli.WriteToUDPAddrPort(req[:], srvMux.LocalAddr()); err != nil {
		t.Fatalf("write request: %v", err)
	}

	cli.SetReadDeadline(time.Now().Add(2 * time.Second))
	var rsp [packetSize]byte
	n, _, err := cli.ReadFromUDPAddrPort(rsp[:])
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	h, t1, t2, t3, err := decodeClock(rsp[:n])
	if err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if h.Type != stream.TypeClockRsp {
		t.Errorf("Type = %#x, want reply", h.Type)
	}
	if h.Gen != 5 || h.Seq != 77 {
		t.Errorf("Gen/Seq = %d/%d, want 5/77", h.Gen, h.Seq)
	}
	if t1 != 424242 {
		t.Errorf("echoed t1 = %d, want 424242", t1)
	}
	if !(t2 <= t3) {
		t.Errorf("t2=%d t3=%d, want t2<=t3", t2, t3)
	}
	if t2 < 1000 {
		t.Errorf("t2=%d, want >= fake clock base 1000", t2)
	}
}

func TestServerDropsMalformed(t *testing.T) {
	srvMux := testMux(t)
	clk := &fakeClock{}
	s := &Server{mux: srvMux, now: clk.now, log: discardLog()}
	s.Start()

	cli, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	defer cli.Close()

	// Send a too-short clock-typed datagram directly (the mux drops <HeaderSize,
	// so craft a valid header but truncated payload to exercise server-side drop).
	var short [stream.HeaderSize + 4]byte
	stream.Header{Magic: stream.Magic, Type: stream.TypeClockReq, PayloadLen: clockPayloadSize}.Encode(short[:])
	cli.WriteToUDPAddrPort(short[:], srvMux.LocalAddr())

	cli.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	var buf [packetSize]byte
	if _, _, err := cli.ReadFromUDPAddrPort(buf[:]); err == nil {
		t.Fatal("expected no reply for malformed request, got one")
	}
}

func TestFollowerSyncsOverLoopback(t *testing.T) {
	mux := testMux(t)
	// Master clock = base; follower (local) clock = base + skew. The server uses
	// the master clock; the follower uses skewed reads. The estimator should
	// recover offset == master - local == -skew.
	const skew = 5_000_000 // local is 5ms ahead of master
	master := &fakeClock{}
	master.set(1_000_000_000)

	s := &Server{mux: mux, now: master.now, log: discardLog()}
	s.Start()

	f := newFollowerInterval(mux, discardLog(), 2*time.Millisecond)
	f.now = skewed(master, skew) // follower local time = master + skew
	f.Start()
	defer f.Close()
	f.SetMaster(mux.LocalAddr(), 1)

	waitSync(t, f, 2*time.Second)

	st := f.Stats()
	// offset = master - local ~= -skew (plus tiny per-read increments). Allow slack.
	if st.OffsetNs > -skew+1_000_000 || st.OffsetNs < -skew-1_000_000 {
		t.Fatalf("offset = %d, want ~ %d", st.OffsetNs, -skew)
	}
}

func TestMasterFollowsSelfZeroOffset(t *testing.T) {
	mux := testMux(t)
	clk := &fakeClock{}
	clk.set(1_000_000_000)
	s := &Server{mux: mux, now: clk.now, log: discardLog()}
	s.Start()

	f := newFollowerInterval(mux, discardLog(), 2*time.Millisecond)
	f.now = clk.now // SAME clock as the server
	f.Start()
	defer f.Close()
	f.SetMaster(mux.LocalAddr(), 1)

	waitSync(t, f, 2*time.Second)
	st := f.Stats()
	// Single monotonic clock: |offset| is just the tiny localhost RTT / read
	// granularity, well under 1ms.
	if st.OffsetNs > 1_000_000 || st.OffsetNs < -1_000_000 {
		t.Fatalf("self-follow offset = %d, want ~0", st.OffsetNs)
	}
	if !st.Synced {
		t.Fatal("not synced")
	}
}

func TestFollowerUnspecifiedAddrRewritten(t *testing.T) {
	mux := testMux(t)
	clk := &fakeClock{}
	clk.set(1_000_000_000)
	s := &Server{mux: mux, now: clk.now, log: discardLog()}
	s.Start()

	f := newFollowerInterval(mux, discardLog(), 2*time.Millisecond)
	f.now = clk.now
	f.Start()
	defer f.Close()

	// Point at 0.0.0.0:<port> — SetMaster must rewrite to loopback and still sync.
	port := mux.LocalAddr().Port()
	wildcard := netip.AddrPortFrom(netip.IPv4Unspecified(), port)
	f.SetMaster(wildcard, 1)

	waitSync(t, f, 2*time.Second)
}

func TestResyncOnGenerationChange(t *testing.T) {
	mux := testMux(t)
	clk := &fakeClock{}
	clk.set(1_000_000_000)
	s := &Server{mux: mux, now: clk.now, log: discardLog()}
	s.Start()

	f := newFollowerInterval(mux, discardLog(), 2*time.Millisecond)
	f.now = clk.now
	f.Start()
	defer f.Close()
	f.SetMaster(mux.LocalAddr(), 1)
	waitSync(t, f, 2*time.Second)

	// Bump generation: immediately unsynced (samples cleared).
	f.SetMaster(mux.LocalAddr(), 2)
	if _, ok := f.MasterNow(); ok {
		t.Fatal("expected unsynced immediately after gen bump")
	}

	// A reply with the OLD gen must be ignored.
	f.mu.Lock()
	f.pending[9999] = clk.read()
	f.mu.Unlock()
	var old [packetSize]byte
	encodeClock(old[:], stream.TypeClockRsp, 1 /*old gen*/, 9999, 0, 5, 6)
	f.handleReply(old[:], mux.LocalAddr())
	if _, ok := f.MasterNow(); ok {
		t.Fatal("old-gen reply should not sync")
	}

	// New-gen probes re-sync.
	waitSync(t, f, 2*time.Second)
}

func TestResyncOnEndpointChange(t *testing.T) {
	mux := testMux(t)
	clk := &fakeClock{}
	clk.set(1_000_000_000)
	s := &Server{mux: mux, now: clk.now, log: discardLog()}
	s.Start()

	f := newFollowerInterval(mux, discardLog(), 2*time.Millisecond)
	f.now = clk.now
	f.Start()
	defer f.Close()
	f.SetMaster(mux.LocalAddr(), 1)
	waitSync(t, f, 2*time.Second)

	// Same gen, different (still self) addr via explicit loopback host: because
	// dst differs from the unspecified->loopback rewrite is identical, use a
	// bogus-then-real swap to assert the reset path. Here we change to a clearly
	// different endpoint and confirm the estimator resets.
	other := netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 2}), mux.LocalAddr().Port())
	f.SetMaster(other, 1)
	if _, ok := f.MasterNow(); ok {
		t.Fatal("expected unsynced after endpoint change")
	}
	if f.Stats().Samples != 0 {
		t.Fatalf("samples = %d after endpoint change, want 0", f.Stats().Samples)
	}
}

func TestSetMasterNoOpSameTarget(t *testing.T) {
	mux := testMux(t)
	clk := &fakeClock{}
	clk.set(1_000_000_000)
	s := &Server{mux: mux, now: clk.now, log: discardLog()}
	s.Start()

	f := newFollowerInterval(mux, discardLog(), 2*time.Millisecond)
	f.now = clk.now
	f.Start()
	defer f.Close()
	f.SetMaster(mux.LocalAddr(), 1)
	waitSync(t, f, 2*time.Second)
	before := f.Stats().Samples

	// Same (dst,gen) — note SetMaster applies the same loopback rewrite, so the
	// stored dst matches. Must NOT reset.
	f.SetMaster(mux.LocalAddr(), 1)
	if _, ok := f.MasterNow(); !ok {
		t.Fatal("no-op SetMaster wiped the estimate")
	}
	if got := f.Stats().Samples; got < before {
		t.Fatalf("samples dropped from %d to %d on no-op SetMaster", before, got)
	}
}

func TestStaleGenReplyDropped(t *testing.T) {
	mux := testMux(t)
	f := newFollowerInterval(mux, discardLog(), time.Hour) // no auto probes
	f.now = func() int64 { return 1000 }
	f.SetMaster(mux.LocalAddr(), 3)

	f.mu.Lock()
	f.pending[1] = 1000
	f.mu.Unlock()
	var pkt [packetSize]byte
	encodeClock(pkt[:], stream.TypeClockRsp, 2 /*wrong gen*/, 1, 0, 10, 20)
	f.handleReply(pkt[:], mux.LocalAddr())
	if f.Stats().Samples != 0 {
		t.Fatal("stale-gen reply was added")
	}
}

func TestUnknownSeqReplyDropped(t *testing.T) {
	mux := testMux(t)
	f := newFollowerInterval(mux, discardLog(), time.Hour)
	f.now = func() int64 { return 1000 }
	f.SetMaster(mux.LocalAddr(), 1)

	var pkt [packetSize]byte
	encodeClock(pkt[:], stream.TypeClockRsp, 1, 12345 /*never sent*/, 0, 10, 20)
	f.handleReply(pkt[:], mux.LocalAddr()) // must not panic
	if f.Stats().Samples != 0 {
		t.Fatal("unknown-seq reply was added")
	}
}

func TestPendingPrunedOnLoss(t *testing.T) {
	mux := testMux(t)
	// No server: every probe's reply is lost. Drive the clock forward fast so
	// pending entries age past pendingTTL and get pruned.
	var nowNs atomic.Int64
	nowNs.Store(0)
	f := newFollowerInterval(mux, discardLog(), 2*time.Millisecond)
	f.now = func() int64 { return nowNs.Add(int64(time.Second)) } // +1s per read
	f.Start()
	defer f.Close()
	f.SetMaster(mux.LocalAddr(), 1)

	// Let many probes fire; each read advances the clock by 1s so old pending
	// entries exceed the 5s TTL and are pruned.
	time.Sleep(300 * time.Millisecond)
	f.mu.Lock()
	n := len(f.pending)
	f.mu.Unlock()
	if n > 10 {
		t.Fatalf("pending map grew to %d, want bounded", n)
	}
}

func TestMasterToLocalRoundTrip(t *testing.T) {
	mux := testMux(t)
	clk := &fakeClock{}
	clk.set(1_000_000_000)
	s := &Server{mux: mux, now: clk.now, log: discardLog()}
	s.Start()

	f := newFollowerInterval(mux, discardLog(), 2*time.Millisecond)
	f.now = clk.now
	f.Start()
	defer f.Close()
	f.SetMaster(mux.LocalAddr(), 1)
	waitSync(t, f, 2*time.Second)

	const x = int64(123_456_789)
	m, ok := f.LocalToMaster(x)
	if !ok {
		t.Fatal("LocalToMaster not ok")
	}
	back, ok := f.MasterToLocal(m)
	if !ok {
		t.Fatal("MasterToLocal not ok")
	}
	if back != x {
		t.Fatalf("round-trip: got %d, want %d", back, x)
	}
}

func TestCloseStopsProbeLoop(t *testing.T) {
	mux := testMux(t)
	f := newFollowerInterval(mux, discardLog(), 2*time.Millisecond)
	f.now = func() int64 { return 1 }
	f.Start()
	f.SetMaster(mux.LocalAddr(), 1)
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := f.Close(); err != nil { // idempotent
		t.Fatalf("second Close: %v", err)
	}
}

func TestUnsyncedBeforeFirstReply(t *testing.T) {
	mux := testMux(t)
	f := newFollowerInterval(mux, discardLog(), time.Hour)
	f.now = func() int64 { return 1 }
	f.SetMaster(mux.LocalAddr(), 1)
	if _, ok := f.MasterNow(); ok {
		t.Fatal("fresh follower reports synced before any reply")
	}
}
