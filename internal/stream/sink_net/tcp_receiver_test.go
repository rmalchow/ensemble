package sink_net

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestTCPHandleStreamInOrder: feeding deframed packets through handleStream
// pushes one chunk per packet at the right sampleIndex, with no reorder window or
// FEC in the path (05 §5.9 "same pipeline minus FEC and reordering").
func TestTCPHandleStreamInOrder(t *testing.T) {
	r, cap := newTestReceiver(t, allowingSet("127.0.0.1"), canonCfg())
	r.gate.Accept(7) // adopt gen 7 as current so same-gen packets pass
	r.genSnap = 7

	const n = 5
	for i := 0; i < n; i++ {
		idx := int64(i) * tFrames
		pkt := buildPacket(7, uint64(i), idx, tFrames, tChannels, float32(i)/256)
		r.handleStream(pkt)
	}
	if cap.len() != n {
		t.Fatalf("pushes=%d, want %d", cap.len(), n)
	}
	for i := 0; i < n; i++ {
		gotIdx, pcm := cap.at(i)
		if gotIdx != int64(i)*tFrames {
			t.Errorf("push %d idx=%d, want %d", i, gotIdx, int64(i)*tFrames)
		}
		if len(pcm) != tFrames*tChannels {
			t.Errorf("push %d len=%d, want %d", i, len(pcm), tFrames*tChannels)
		}
	}
}

// TestTCPGenChangeFlush: a higher streamGen over TCP flushes + re-primes and only
// the keyframe-first packets of the new gen are delivered (05 §5.9 honors gen).
func TestTCPGenChangeFlush(t *testing.T) {
	r, cap := newTestReceiver(t, allowingSet("127.0.0.1"), canonCfg())
	r.gate.Accept(1)
	r.genSnap = 1

	// Deliver one chunk of gen 1.
	r.handleStream(buildPacket(1, 0, 0, tFrames, tChannels, 0.1))
	before := cap.len()
	if before != 1 {
		t.Fatalf("gen1 pushes=%d, want 1", before)
	}

	// A newer gen arrives: the receiver must adopt it (flush) and deliver the new
	// keyframe chunk. genSnap should advance to 2.
	r.handleStream(buildPacket(2, 0, 5000, tFrames, tChannels, 0.2))
	if r.StreamGen() != 2 {
		t.Errorf("StreamGen=%d after adopt, want 2", r.StreamGen())
	}
	if cap.len() != before+1 {
		t.Errorf("after gen-change pushes=%d, want %d", cap.len(), before+1)
	}
	gotIdx, _ := cap.at(cap.len() - 1)
	if gotIdx != 5000 {
		t.Errorf("new-gen chunk idx=%d, want 5000", gotIdx)
	}

	// A straggler from the OLD gen is dropped (no new push).
	r.handleStream(buildPacket(1, 1, tFrames, tFrames, tChannels, 0.9))
	if cap.len() != before+1 {
		t.Errorf("old-gen straggler pushed: pushes=%d", cap.len())
	}
}

// TestTCPRunLoopback drives the real RunTCP listener with an origin-style writer
// over a loopback TCP connection: length-prefixed packets in, decoded chunks out,
// in order, no loss. Asserts round-trip parity of the deframed pipeline.
func TestTCPRunLoopback(t *testing.T) {
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	r, cap := newTestReceiver(t, allowingSet("127.0.0.1"), canonCfg())
	r.gate.Accept(3)
	r.genSnap = 3

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = r.RunTCP(ctx, ln); close(done) }()

	conn, err := net.DialTCP("tcp", nil, ln.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	for i := 0; i < n; i++ {
		pkt := buildPacket(3, uint64(i), int64(i)*tFrames, tFrames, tChannels, float32(i)/256)
		if err := WriteFrame(conn, pkt); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
	}

	// Wait for all pushes to arrive (the recv goroutine deframes asynchronously).
	deadline := time.Now().Add(2 * time.Second)
	for cap.len() < n && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if cap.len() != n {
		t.Fatalf("loopback pushes=%d, want %d", cap.len(), n)
	}
	for i := 0; i < n; i++ {
		gotIdx, _ := cap.at(i)
		if gotIdx != int64(i)*tFrames {
			t.Errorf("loopback push %d idx=%d want %d", i, gotIdx, int64(i)*tFrames)
		}
	}
	conn.Close()
	cancel()
	<-done
}

// TestTCPAllowlistRejectsPeer: a connection whose peer IP is not allowlisted is
// closed at accept and serves NO frame (03 §6.1 TCP-fallback listener gate).
func TestTCPAllowlistRejectsPeer(t *testing.T) {
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	// Deny all (empty allowlist): the loopback peer is NOT allowed.
	r, cap := newTestReceiver(t, denyingSet(), canonCfg())
	r.gate.Accept(1)
	r.genSnap = 1

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = r.RunTCP(ctx, ln); close(done) }()

	conn, err := net.DialTCP("tcp", nil, ln.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	// The server should close the conn without reading; writes may still buffer,
	// but no push must result.
	_ = WriteFrame(conn, buildPacket(1, 0, 0, tFrames, tChannels, 0.5))
	time.Sleep(100 * time.Millisecond)
	if cap.len() != 0 {
		t.Errorf("non-allowlisted peer produced %d pushes, want 0", cap.len())
	}
	conn.Close()
	cancel()
	<-done
}

// TestPeerIP extracts and unmaps the peer address from a *net.TCPAddr.
func TestPeerIP(t *testing.T) {
	a := &net.TCPAddr{IP: net.IPv4(192, 168, 1, 5), Port: 9100}
	got := peerIP(a)
	if got.String() != "192.168.1.5" {
		t.Errorf("peerIP=%q, want 192.168.1.5", got.String())
	}
	// A non-TCPAddr yields the zero (invalid) Addr.
	if peerIP(&net.UDPAddr{IP: net.IPv4(1, 2, 3, 4)}).IsValid() {
		t.Error("peerIP of non-TCPAddr should be invalid")
	}
}
