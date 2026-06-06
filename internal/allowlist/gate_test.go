package allowlist

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// newLoopbackGate binds a UDP socket on 127.0.0.1:0 and runs GateUDP over it in
// a goroutine. It returns the bound addr, the set, a stop func that cancels ctx,
// closes the conn and joins the goroutine, plus channels reporting delivered
// payloads and srcs. The deliver callback copies the buffer (it aliases the read
// buffer per the GateUDP contract).
func newLoopbackGate(t *testing.T, set *Set) (addr netip.AddrPort, delivered chan []byte, srcs chan netip.AddrPort, stop func()) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	la := conn.LocalAddr().(*net.UDPAddr)
	addr = la.AddrPort()

	delivered = make(chan []byte, 16)
	srcs = make(chan netip.AddrPort, 16)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- GateUDP(ctx, conn, set, func(src netip.AddrPort, b []byte) {
			cp := make([]byte, len(b))
			copy(cp, b)
			delivered <- cp
			srcs <- src
		})
	}()

	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			_ = conn.Close()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("GateUDP did not return after stop")
			}
		})
	}
	return addr, delivered, srcs, stop
}

// sendUDP dials dst from a fresh socket and sends b, returning the local addr.
func sendUDP(t *testing.T, dst netip.AddrPort, b []byte) netip.AddrPort {
	t.Helper()
	c, err := net.DialUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}, net.UDPAddrFromAddrPort(dst))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if _, err := c.Write(b); err != nil {
		t.Fatalf("write: %v", err)
	}
	return c.LocalAddr().(*net.UDPAddr).AddrPort()
}

func allowLoopback() *Set {
	s := New()
	s.cur.Store(&map[netip.Addr]struct{}{netip.MustParseAddr("127.0.0.1"): {}})
	return s
}

func TestGateUDPAllowedDelivered(t *testing.T) {
	addr, delivered, srcs, stop := newLoopbackGate(t, allowLoopback())
	defer stop()

	payload := []byte("hello clock")
	srcAddr := sendUDP(t, addr, payload)

	select {
	case got := <-delivered:
		if !bytes.Equal(got, payload) {
			t.Errorf("delivered %q, want %q", got, payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("allowed datagram was not delivered")
	}
	gotSrc := <-srcs
	if gotSrc.Addr() != srcAddr.Addr() {
		t.Errorf("src addr = %v, want %v", gotSrc.Addr(), srcAddr.Addr())
	}
	if gotSrc.Port() != srcAddr.Port() {
		t.Errorf("src port = %d, want %d", gotSrc.Port(), srcAddr.Port())
	}
}

func TestGateUDPDisallowedDropped(t *testing.T) {
	// Empty set: 127.0.0.1 is NOT allowed, so the datagram must be dropped.
	addr, delivered, _, stop := newLoopbackGate(t, New())
	defer stop()

	sendUDP(t, addr, []byte("intruder"))

	select {
	case got := <-delivered:
		t.Fatalf("disallowed datagram was delivered: %q", got)
	case <-time.After(300 * time.Millisecond):
		// Expected: silent drop, deliver never called.
	}
}

func TestGateUDPNoReplyOnDrop(t *testing.T) {
	addr, _, _, stop := newLoopbackGate(t, New()) // deny all
	defer stop()

	// Use a connected socket as a passive sink: if the gate replied, a Read
	// would return bytes. We expect a timeout (silence).
	c, err := net.DialUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}, net.UDPAddrFromAddrPort(addr))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err == nil {
		t.Fatalf("gate replied with %d bytes; want silence", n)
	}
	if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("read error = %v, want timeout", err)
	}
}

func TestGateUDPBufferNotTruncated(t *testing.T) {
	// Max canonical audio datagram: 44 B header + 480*2*2 S16LE = 1964 B (§5.3).
	const wantLen = 44 + 480*2*2
	addr, delivered, _, stop := newLoopbackGate(t, allowLoopback())
	defer stop()

	payload := bytes.Repeat([]byte{0xAB}, wantLen)
	sendUDP(t, addr, payload)

	select {
	case got := <-delivered:
		if len(got) != wantLen {
			t.Errorf("delivered len = %d, want %d (truncated)", len(got), wantLen)
		}
		if !bytes.Equal(got, payload) {
			t.Error("delivered bytes differ from sent (corruption/truncation)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("max-size datagram not delivered")
	}
}

func TestGateUDPLiveRederive(t *testing.T) {
	// Start denying 127.0.0.1, then Update to allow it mid-stream — no socket
	// restart (03 §6.3). Subsequent packets must be delivered.
	set := New()
	addr, delivered, _, stop := newLoopbackGate(t, set)
	defer stop()

	// First packet dropped.
	sendUDP(t, addr, []byte("before"))
	select {
	case got := <-delivered:
		t.Fatalf("delivered before allow: %q", got)
	case <-time.After(200 * time.Millisecond):
	}

	// Flip the set to allow loopback.
	set.cur.Store(&map[netip.Addr]struct{}{netip.MustParseAddr("127.0.0.1"): {}})

	// Retry until a packet lands (the sender uses ephemeral ports; the gate now
	// allows any 127.0.0.1 source).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sendUDP(t, addr, []byte("after"))
		select {
		case got := <-delivered:
			if !bytes.Equal(got, []byte("after")) {
				t.Errorf("delivered %q, want %q", got, "after")
			}
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("packet not delivered after live re-derive")
}

func TestGateUDPConnCloseReturns(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- GateUDP(context.Background(), conn, allowLoopback(), func(netip.AddrPort, []byte) {})
	}()
	_ = conn.Close()
	select {
	case <-done: // returns without panic on closed conn
	case <-time.After(2 * time.Second):
		t.Fatal("GateUDP did not return after conn close")
	}
}

func TestGateUDPCtxCancelReturns(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- GateUDP(ctx, conn, allowLoopback(), func(netip.AddrPort, []byte) {})
	}()
	cancel()
	_ = conn.Close() // unblock the read so the ctx check runs
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("GateUDP returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GateUDP did not return after ctx cancel")
	}
}
