package source

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"ensemble/internal/id"
	"ensemble/internal/stream"
)

// newTestServer builds a Server bound to loopback SOURCE_PORT sockets and runs
// it. Returns the server and its UDP/TCP addresses.
func newTestServer(t *testing.T) (*Server, netip.AddrPort, netip.AddrPort) {
	t.Helper()
	uc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{Self: id.New(), UDP: uc, TCP: ln})
	s.Run()
	uap, _ := netip.ParseAddrPort(uc.LocalAddr().String())
	tap, _ := netip.ParseAddrPort(ln.Addr().String())
	t.Cleanup(func() {
		s.Close()
		uc.Close()
		ln.Close()
	})
	return s, uap, tap
}

// udpSub is a raw subscriber socket that HELLOs a source and reads audio.
type udpSub struct {
	conn *net.UDPConn
	src  netip.AddrPort
}

func newUDPSub(t *testing.T, src netip.AddrPort) *udpSub {
	t.Helper()
	uc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { uc.Close() })
	return &udpSub{conn: uc, src: src}
}

func (u *udpSub) hello(primeMe bool) {
	var flag byte
	if primeMe {
		flag = stream.FlagPrimeMe
	}
	h := stream.Header{Magic: stream.Magic, Type: stream.TypeHello, PayloadLen: 1}
	u.conn.WriteToUDPAddrPort(h.AppendFrame(nil, []byte{flag}), u.src)
}

func (u *udpSub) restart() {
	h := stream.Header{Magic: stream.Magic, Type: stream.TypeRestart, PayloadLen: 1}
	u.conn.WriteToUDPAddrPort(h.AppendFrame(nil, []byte{stream.FlagPrimeMe}), u.src)
}

func (u *udpSub) bye() {
	h := stream.Header{Magic: stream.Magic, Type: stream.TypeBye, PayloadLen: 1}
	u.conn.WriteToUDPAddrPort(h.AppendFrame(nil, []byte{0}), u.src)
}

// recv reads packets until timeout, returning all decoded headers (and their
// payloads).
func (u *udpSub) recvAll(t *testing.T, d time.Duration) []stream.Header {
	t.Helper()
	var out []stream.Header
	deadline := time.Now().Add(d)
	buf := make([]byte, 64*1024)
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			break
		}
		u.conn.SetReadDeadline(time.Now().Add(remain))
		n, _, err := u.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			break
		}
		h, _, derr := stream.DecodeFrame(buf[:n])
		if derr == nil {
			out = append(out, h)
		}
	}
	return out
}

func countType(hs []stream.Header, typ byte) int {
	n := 0
	for _, h := range hs {
		if h.Type == typ {
			n++
		}
	}
	return n
}

func TestServerSubscribeUDPPrimeThenLive(t *testing.T) {
	s, uap, _ := newTestServer(t)
	s.StartSession(1, stream.TransportUDP, 150)

	// release 10 frames
	for i := 0; i < 10; i++ {
		s.ReleaseFrame(int64(i)*stream.FrameNanos, pcm(byte(i)))
	}
	sub := newUDPSub(t, uap)
	sub.hello(true) // prime-me

	// release more live frames
	go func() {
		for i := 10; i < 20; i++ {
			s.ReleaseFrame(int64(i)*stream.FrameNanos, pcm(byte(i)))
			time.Sleep(2 * time.Millisecond)
		}
	}()

	hs := sub.recvAll(t, 800*time.Millisecond)
	audio := countType(hs, stream.TypeAudio)
	if audio == 0 {
		t.Fatal("subscriber received no audio")
	}
	st := s.Stats()
	if st.Connects != 1 {
		t.Fatalf("Connects=%d want 1", st.Connects)
	}
	if st.Primes != 1 {
		t.Fatalf("Primes=%d want 1", st.Primes)
	}
}

func TestServerFanoutAllSubscribers(t *testing.T) {
	s, uap, _ := newTestServer(t)
	s.StartSession(1, stream.TransportUDP, 150)

	s1 := newUDPSub(t, uap)
	s2 := newUDPSub(t, uap)
	s1.hello(false)
	s2.hello(false)
	// allow registry to record both
	if !waitForN(t, func() int { return s.Stats().Clients }, 2, time.Second) {
		t.Fatalf("clients=%d want 2", s.Stats().Clients)
	}
	s.ReleaseFrame(0, pcm(0x77))

	h1 := s1.recvAll(t, 300*time.Millisecond)
	h2 := s2.recvAll(t, 300*time.Millisecond)
	if countType(h1, stream.TypeAudio) == 0 || countType(h2, stream.TypeAudio) == 0 {
		t.Fatal("not all subscribers received the frame")
	}
}

func TestServerFECCadence(t *testing.T) {
	s, uap, _ := newTestServer(t)
	s.StartSession(1, stream.TransportUDP, 150)
	sub := newUDPSub(t, uap)
	sub.hello(false)
	waitForN(t, func() int { return s.Stats().Clients }, 1, time.Second)

	// 3 frames -> no parity yet
	for i := 0; i < 3; i++ {
		s.ReleaseFrame(int64(i)*stream.FrameNanos, pcm(byte(i)))
	}
	h3 := sub.recvAll(t, 200*time.Millisecond)
	if countType(h3, stream.TypeFEC) != 0 {
		t.Fatal("parity before 4th frame")
	}
	// 4th frame -> exactly one parity
	s.ReleaseFrame(3*stream.FrameNanos, pcm(3))
	h4 := sub.recvAll(t, 300*time.Millisecond)
	if countType(h4, stream.TypeFEC) != 1 {
		t.Fatalf("parity count=%d want 1", countType(h4, stream.TypeFEC))
	}
}

func TestServerRestartReprimes(t *testing.T) {
	s, uap, _ := newTestServer(t)
	s.StartSession(1, stream.TransportUDP, 150)
	for i := 0; i < 10; i++ {
		s.ReleaseFrame(int64(i)*stream.FrameNanos, pcm(byte(i)))
	}
	sub := newUDPSub(t, uap)
	sub.hello(false) // join without prime
	waitForN(t, func() int { return s.Stats().Clients }, 1, time.Second)
	sub.recvAll(t, 100*time.Millisecond) // drain reconfig etc

	sub.restart()
	hs := sub.recvAll(t, 500*time.Millisecond)
	if countType(hs, stream.TypeAudio) == 0 {
		t.Fatal("RESTART did not re-prime")
	}
	if s.Stats().Restarts != 1 {
		t.Fatalf("Restarts=%d want 1", s.Stats().Restarts)
	}
}

func TestServerReconfigBroadcast(t *testing.T) {
	s, uap, _ := newTestServer(t)
	s.StartSession(1, stream.TransportUDP, 150)
	sub := newUDPSub(t, uap)
	sub.hello(false)
	waitForN(t, func() int { return s.Stats().Clients }, 1, time.Second)
	sub.recvAll(t, 100*time.Millisecond)

	s.Reconfig()
	hs := sub.recvAll(t, 300*time.Millisecond)
	if countType(hs, stream.TypeReconfig) == 0 {
		t.Fatal("no RECONFIG broadcast received")
	}
}

func TestServerStopSessionFlushesAndNotifies(t *testing.T) {
	s, uap, _ := newTestServer(t)
	s.StartSession(1, stream.TransportUDP, 150)
	sub := newUDPSub(t, uap)
	sub.hello(false)
	waitForN(t, func() int { return s.Stats().Clients }, 1, time.Second)
	sub.recvAll(t, 100*time.Millisecond)

	// 6 frames: one full FEC block (4) + a 2-frame partial tail.
	for i := 0; i < 6; i++ {
		s.ReleaseFrame(int64(i)*stream.FrameNanos, pcm(byte(i)))
	}
	sub.recvAll(t, 200*time.Millisecond) // drain live + the full-block parity

	s.StopSession()
	hs := sub.recvAll(t, 300*time.Millisecond)
	if countType(hs, stream.TypeFEC) != 1 {
		t.Fatalf("expected 1 tail parity, got %d", countType(hs, stream.TypeFEC))
	}
	reconfStop := false
	for _, h := range hs {
		if h.Type == stream.TypeReconfig {
			reconfStop = true
		}
	}
	if !reconfStop {
		t.Fatal("no stop RECONFIG on StopSession")
	}
	if s.active {
		t.Fatal("session still active after stop")
	}
}

func TestServerReleaseNoSession(t *testing.T) {
	s, _, _ := newTestServer(t)
	if seq := s.ReleaseFrame(0, pcm(0)); seq != 0 {
		t.Fatalf("ReleaseFrame before StartSession returned %d", seq)
	}
}

func TestServerKeepaliveExpiry(t *testing.T) {
	s, uap, _ := newTestServer(t)
	s.StartSession(1, stream.TransportUDP, 150)
	sub := newUDPSub(t, uap)
	sub.hello(false)
	waitForN(t, func() int { return s.Stats().Clients }, 1, time.Second)

	// Force the registry entry stale by rewinding lastSeen, then sweep.
	s.mu.Lock()
	for _, sb := range s.reg.subs {
		sb.lastSeen = time.Now().Add(-20 * time.Second)
	}
	s.mu.Unlock()
	if !waitForN(t, func() int { return s.Stats().Clients }, 0, 3*time.Second) {
		t.Fatalf("clients=%d want 0 after expiry", s.Stats().Clients)
	}
}

func TestServerByeRemoves(t *testing.T) {
	s, uap, _ := newTestServer(t)
	s.StartSession(1, stream.TransportUDP, 150)
	sub := newUDPSub(t, uap)
	sub.hello(false)
	waitForN(t, func() int { return s.Stats().Clients }, 1, time.Second)
	sub.bye()
	if !waitForN(t, func() int { return s.Stats().Clients }, 0, time.Second) {
		t.Fatalf("clients=%d want 0 after BYE", s.Stats().Clients)
	}
}

func TestServerClose(t *testing.T) {
	uc, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	ln, _ := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	s := NewServer(Config{Self: id.New(), UDP: uc, TCP: ln})
	s.Run()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// idempotent
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// K owns the sockets: they remain usable (not closed by us).
	if err := uc.SetReadDeadline(time.Now()); err != nil {
		t.Fatalf("UDP socket closed by server: %v", err)
	}
	uc.Close()
	ln.Close()
}

func waitForN(t *testing.T, get func() int, want int, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if get() == want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return get() == want
}

func waitForAtLeast(t *testing.T, get func() int, want int, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if get() >= want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return get() >= want
}
