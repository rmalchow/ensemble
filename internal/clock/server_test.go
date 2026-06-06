package clock

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

func allowLoopback(netip.Addr) bool { return true }

// waitFor polls cond until it is true or the deadline passes.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

func TestServerFollowerEndToEnd(t *testing.T) {
	srv, err := ListenGated("127.0.0.1:0", allowLoopback)
	if err != nil {
		t.Fatalf("ListenGated: %v", err)
	}
	defer srv.Close()

	f := NewFollower(
		WithInterval(5*time.Millisecond),
		WithTimeout(200*time.Millisecond),
		WithEstimator(8, 0.15),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx, srv.Addr().String()) }()

	if !waitFor(2*time.Second, func() bool {
		_, ok := f.Offset()
		return ok && f.SamplesSeen() > 0
	}) {
		t.Fatalf("follower never locked: samples=%d", f.SamplesSeen())
	}

	// Sub-ms convergence on loopback (A.13 P3 clock slice): true offset is ~0.
	off, ok := f.Offset()
	if !ok {
		t.Fatal("expected an offset")
	}
	if off < -time.Millisecond || off > time.Millisecond {
		t.Errorf("loopback offset = %v, want |offset| < 1ms", off)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
}

func TestServerAllowlistDrop(t *testing.T) {
	// Gate denies everything: the follower must never get a reply.
	deny := func(netip.Addr) bool { return false }
	srv, err := ListenGated("127.0.0.1:0", deny)
	if err != nil {
		t.Fatalf("ListenGated: %v", err)
	}
	defer srv.Close()

	f := NewFollower(WithInterval(5*time.Millisecond), WithTimeout(50*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.Run(ctx, srv.Addr().String())

	// Give it ample time to send several pings; none should be answered.
	time.Sleep(300 * time.Millisecond)
	if _, ok := f.Offset(); ok {
		t.Error("follower locked despite deny-all gate")
	}
	if n := f.SamplesSeen(); n != 0 {
		t.Errorf("SamplesSeen = %d, want 0 (all dropped, no replies)", n)
	}
}

func TestServerAllowlistLiveFlip(t *testing.T) {
	// Start denied, then flip the predicate to allow mid-run with no socket
	// restart (P2.4 §5.2 / doc 04 §4.1.2 live update).
	var allow atomic.Bool
	srv, err := ListenGated("127.0.0.1:0", func(netip.Addr) bool { return allow.Load() })
	if err != nil {
		t.Fatalf("ListenGated: %v", err)
	}
	defer srv.Close()

	f := NewFollower(WithInterval(5*time.Millisecond), WithTimeout(50*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.Run(ctx, srv.Addr().String())

	time.Sleep(100 * time.Millisecond)
	if _, ok := f.Offset(); ok {
		t.Fatal("follower locked while gate denied")
	}

	allow.Store(true) // open the gate live
	if !waitFor(2*time.Second, func() bool { _, ok := f.Offset(); return ok }) {
		t.Fatal("follower did not lock after the gate opened live")
	}
}

func TestFollowerRejectsStaleSeq(t *testing.T) {
	// A reply with a wrong seq must be skipped by exchange: no sample recorded.
	// We act as a malicious "server" that replies with a bogus seq.
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	defer conn.Close()

	go func() {
		buf := make([]byte, PacketSize)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			req, err := unmarshal(buf[:n])
			if err != nil {
				continue
			}
			// Reply with a deliberately wrong seq.
			bad := packet{kind: kindReply, seq: req.seq + 9999, t1: req.t1, t2: nowMono(), t3: nowMono()}
			_, _ = conn.WriteToUDP(bad.marshal(), from)
		}
	}()

	f := NewFollower(WithInterval(5*time.Millisecond), WithTimeout(40*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.Run(ctx, conn.LocalAddr().String())

	time.Sleep(250 * time.Millisecond)
	if _, ok := f.Offset(); ok {
		t.Error("follower accepted a stale-seq reply")
	}
	if n := f.SamplesSeen(); n != 0 {
		t.Errorf("SamplesSeen = %d, want 0 (stale seq rejected)", n)
	}
}

func TestServerCloseStopsServe(t *testing.T) {
	srv, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// A second close is harmless from the test's view (already-closed error).
	_ = srv.Close()
}

func TestFollowerCtxCancelReturns(t *testing.T) {
	srv, err := ListenGated("127.0.0.1:0", allowLoopback)
	if err != nil {
		t.Fatalf("ListenGated: %v", err)
	}
	defer srv.Close()

	f := NewFollower(WithInterval(10 * time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx, srv.Addr().String()) }()

	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx cancel (goroutine leak)")
	}
}
