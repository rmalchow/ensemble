package stream

import (
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// --- helpers ---------------------------------------------------------------

// newLoopbackMux binds a UDP socket on 127.0.0.1:0 and wraps it in a Mux.
func newLoopbackMux(t *testing.T) *Mux {
	t.Helper()
	uc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	m := NewMux(uc, nil)
	t.Cleanup(func() { m.Close() })
	return m
}

// fakeSource is a raw UDP socket that plays the role of a source: it writes
// audio/FEC/reconfig datagrams to a client mux addr and reads control packets.
type fakeSource struct {
	conn *net.UDPConn
	addr netip.AddrPort
}

func newFakeSource(t *testing.T) *fakeSource {
	t.Helper()
	uc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	ap, _ := netip.ParseAddrPort(uc.LocalAddr().String())
	fs := &fakeSource{conn: uc, addr: ap}
	t.Cleanup(func() { uc.Close() })
	return fs
}

func (f *fakeSource) sendAudio(to netip.AddrPort, gen uint32, seq uint64, pts int64, pay []byte) {
	h := Header{Magic: Magic, Type: TypeAudio, Gen: gen, Seq: seq, PTS: pts, PayloadLen: uint16(len(pay))}
	pkt := h.AppendFrame(nil, pay)
	f.conn.WriteToUDPAddrPort(pkt, to)
}

func (f *fakeSource) sendParity(to netip.AddrPort, gen uint32, baseSeq uint64, parity []byte) {
	h := Header{Magic: Magic, Type: TypeFEC, Gen: gen, Seq: baseSeq, PayloadLen: uint16(len(parity))}
	pkt := h.AppendFrame(nil, parity)
	f.conn.WriteToUDPAddrPort(pkt, to)
}

func (f *fakeSource) sendReconfig(to netip.AddrPort, gen uint32, stop bool) {
	var flag byte
	if stop {
		flag = FlagStop
	}
	h := Header{Magic: Magic, Type: TypeReconfig, Gen: gen, PayloadLen: 1}
	pkt := h.AppendFrame(nil, []byte{flag})
	f.conn.WriteToUDPAddrPort(pkt, to)
}

// readControl reads one control datagram from a subscriber.
func (f *fakeSource) readControl(t *testing.T, timeout time.Duration) (Header, []byte, netip.AddrPort) {
	t.Helper()
	f.conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 2048)
	n, from, err := f.conn.ReadFromUDPAddrPort(buf)
	if err != nil {
		t.Fatalf("readControl: %v", err)
	}
	h, pay, derr := DecodeFrame(buf[:n])
	if derr != nil {
		t.Fatalf("readControl decode: %v", derr)
	}
	return h, pay, from
}

// collector is a thread-safe DeliverFunc sink.
type collector struct {
	mu    sync.Mutex
	heads []Header
	pays  [][]byte
}

func (c *collector) deliver(h Header, p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]byte, len(p))
	copy(cp, p)
	c.heads = append(c.heads, h)
	c.pays = append(c.pays, cp)
}

func (c *collector) seqs() []uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]uint64, len(c.heads))
	for i, h := range c.heads {
		out[i] = h.Seq
	}
	return out
}

func (c *collector) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.heads)
}

func waitFor(t *testing.T, cond func() bool, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// --- tests -----------------------------------------------------------------

func TestClientUDPDeliver(t *testing.T) {
	mux := newLoopbackMux(t)
	mux.Run()
	col := &collector{}
	c := NewClient(ClientConfig{Mux: mux, Deliver: col.deliver})
	defer c.Close()

	fs := newFakeSource(t)
	if err := c.Subscribe(fs.addr, 1, TransportUDP); err != nil {
		t.Fatal(err)
	}
	// consume the initial HELLO
	fs.readControl(t, time.Second)

	for i := uint64(0); i < 4; i++ {
		fs.sendAudio(mux.LocalAddr(), 1, i, int64(i)*FrameNanos, payload(byte(i+1), FrameBytes))
	}
	if !waitFor(t, func() bool { return col.len() == 4 }, 2*time.Second) {
		t.Fatalf("delivered %d want 4", col.len())
	}
	if c.Counters().Delivered != 4 {
		t.Fatalf("Delivered=%d", c.Counters().Delivered)
	}
}

func TestClientFECRecovery(t *testing.T) {
	mux := newLoopbackMux(t)
	mux.Run()
	col := &collector{}
	c := NewClient(ClientConfig{Mux: mux, Deliver: col.deliver})
	defer c.Close()
	fs := newFakeSource(t)
	c.Subscribe(fs.addr, 1, TransportUDP)
	fs.readControl(t, time.Second)

	pays := [][]byte{payload(1, FrameBytes), payload(2, FrameBytes), payload(3, FrameBytes), payload(4, FrameBytes)}
	to := mux.LocalAddr()
	// drop seq 1; send 0,2,3 then parity
	fs.sendAudio(to, 1, 0, 0, pays[0])
	fs.sendAudio(to, 1, 2, 2*FrameNanos, pays[2])
	fs.sendAudio(to, 1, 3, 3*FrameNanos, pays[3])
	fs.sendParity(to, 1, 0, xorAll(pays...))

	if !waitFor(t, func() bool { return col.len() == 4 }, 2*time.Second) {
		t.Fatalf("delivered %d want 4", col.len())
	}
	ctr := c.Counters()
	if ctr.Recovered != 1 {
		t.Fatalf("Recovered=%d want 1", ctr.Recovered)
	}
	if ctr.Lost != 0 {
		t.Fatalf("Lost=%d want 0", ctr.Lost)
	}
}

func TestClientStaleGenDropped(t *testing.T) {
	mux := newLoopbackMux(t)
	mux.Run()
	col := &collector{}
	c := NewClient(ClientConfig{Mux: mux, Deliver: col.deliver})
	defer c.Close()
	fs := newFakeSource(t)
	c.Subscribe(fs.addr, 5, TransportUDP)
	fs.readControl(t, time.Second)

	fs.sendAudio(mux.LocalAddr(), 4, 0, 0, payload(1, FrameBytes)) // stale gen
	time.Sleep(100 * time.Millisecond)
	if col.len() != 0 {
		t.Fatalf("stale-gen frame delivered")
	}
	if c.Counters().StaleGen == 0 {
		t.Fatal("StaleGen not counted")
	}
}

func TestClientReorderThenDeliver(t *testing.T) {
	mux := newLoopbackMux(t)
	mux.Run()
	col := &collector{}
	c := NewClient(ClientConfig{Mux: mux, Deliver: col.deliver})
	defer c.Close()
	fs := newFakeSource(t)
	c.Subscribe(fs.addr, 1, TransportUDP)
	fs.readControl(t, time.Second)

	to := mux.LocalAddr()
	fs.sendAudio(to, 1, 0, 0, payload(1, FrameBytes))
	fs.sendAudio(to, 1, 2, 0, payload(3, FrameBytes))
	fs.sendAudio(to, 1, 1, 0, payload(2, FrameBytes))
	if !waitFor(t, func() bool { return col.len() == 3 }, 2*time.Second) {
		t.Fatalf("delivered %d", col.len())
	}
	got := col.seqs()
	for i, s := range got {
		if s != uint64(i) {
			t.Fatalf("out of order: %v", got)
		}
	}
}

func TestClientDuplicateCounted(t *testing.T) {
	mux := newLoopbackMux(t)
	mux.Run()
	col := &collector{}
	c := NewClient(ClientConfig{Mux: mux, Deliver: col.deliver})
	defer c.Close()
	fs := newFakeSource(t)
	c.Subscribe(fs.addr, 1, TransportUDP)
	fs.readControl(t, time.Second)

	to := mux.LocalAddr()
	fs.sendAudio(to, 1, 0, 0, payload(1, FrameBytes))
	if !waitFor(t, func() bool { return col.len() == 1 }, time.Second) {
		t.Fatal("first not delivered")
	}
	fs.sendAudio(to, 1, 0, 0, payload(1, FrameBytes)) // dup
	if !waitFor(t, func() bool { return c.Counters().Duplicate == 1 }, time.Second) {
		t.Fatalf("Duplicate=%d", c.Counters().Duplicate)
	}
	if col.len() != 1 {
		t.Fatal("duplicate delivered")
	}
}

func TestClientMalformedDropped(t *testing.T) {
	mux := newLoopbackMux(t)
	mux.Run()
	col := &collector{}
	c := NewClient(ClientConfig{Mux: mux, Deliver: col.deliver})
	defer c.Close()
	fs := newFakeSource(t)
	c.Subscribe(fs.addr, 1, TransportUDP)
	fs.readControl(t, time.Second)

	// A datagram with correct magic+type but truncated payload (PayloadLen too big).
	h := Header{Magic: Magic, Type: TypeAudio, Gen: 1, Seq: 0, PayloadLen: 5000}
	var hb [HeaderSize]byte
	h.Encode(hb[:])
	fs.conn.WriteToUDPAddrPort(hb[:], mux.LocalAddr())
	if !waitFor(t, func() bool { return c.Counters().Malformed == 1 }, time.Second) {
		t.Fatalf("Malformed=%d", c.Counters().Malformed)
	}
}

func TestClientHelloFromMuxSocket(t *testing.T) {
	mux := newLoopbackMux(t)
	mux.Run()
	c := NewClient(ClientConfig{Mux: mux, Deliver: func(Header, []byte) {}})
	defer c.Close()
	fs := newFakeSource(t)
	c.Subscribe(fs.addr, 1, TransportUDP)
	h, pay, from := fs.readControl(t, time.Second)
	if h.Type != TypeHello {
		t.Fatalf("first control is %x not HELLO", h.Type)
	}
	if pay[0]&FlagPrimeMe == 0 {
		t.Fatal("initial HELLO must set prime-me")
	}
	if from != mux.LocalAddr() {
		t.Fatalf("HELLO from %v, want mux %v", from, mux.LocalAddr())
	}
}

func TestClientWatchdogRestart(t *testing.T) {
	mux := newLoopbackMux(t)
	mux.Run()
	col := &collector{}
	c := NewClient(ClientConfig{Mux: mux, Deliver: col.deliver})
	defer c.Close()
	fs := newFakeSource(t)
	c.Subscribe(fs.addr, 1, TransportUDP)
	fs.readControl(t, time.Second) // initial HELLO

	// feed one frame so gotFrame=true, then stall.
	fs.sendAudio(mux.LocalAddr(), 1, 0, 0, payload(1, FrameBytes))
	waitFor(t, func() bool { return col.len() == 1 }, time.Second)

	// Expect a RESTART within ~2.5 s (watchdog timeout 2 s + tick slack),
	// possibly preceded by keepalive HELLOs.
	deadline := time.Now().Add(4 * time.Second)
	gotRestart := false
	for time.Now().Before(deadline) {
		h, pay, _ := fs.readControl(t, 4*time.Second)
		if h.Type == TypeRestart {
			if pay[0]&FlagPrimeMe == 0 {
				t.Fatal("RESTART must set prime-me")
			}
			gotRestart = true
			break
		}
	}
	if !gotRestart {
		t.Fatal("no RESTART issued on starvation")
	}
	if c.Counters().Restarts == 0 {
		t.Fatal("Restarts not counted")
	}
}

func TestClientReconfigStopUDP(t *testing.T) {
	mux := newLoopbackMux(t)
	mux.Run()
	var stops []bool
	var mu sync.Mutex
	c := NewClient(ClientConfig{
		Mux:     mux,
		Deliver: func(Header, []byte) {},
		Reconfig: func(stop bool) {
			mu.Lock()
			stops = append(stops, stop)
			mu.Unlock()
		},
	})
	defer c.Close()
	fs := newFakeSource(t)
	c.Subscribe(fs.addr, 1, TransportUDP)
	fs.readControl(t, time.Second)

	fs.sendReconfig(mux.LocalAddr(), 1, true)
	if !waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(stops) == 1 && stops[0]
	}, 2*time.Second) {
		t.Fatal("OnReconfig(stop=true) not invoked")
	}
}

func TestClientResubscribeNewGen(t *testing.T) {
	mux := newLoopbackMux(t)
	mux.Run()
	col := &collector{}
	c := NewClient(ClientConfig{Mux: mux, Deliver: col.deliver})
	defer c.Close()
	fs := newFakeSource(t)
	c.Subscribe(fs.addr, 1, TransportUDP)
	fs.readControl(t, time.Second) // hello gen1

	c.Subscribe(fs.addr, 2, TransportUDP)
	// Expect a BYE (gen1 teardown) and a HELLO gen2.
	sawBye, sawHello2 := false, false
	for i := 0; i < 2; i++ {
		h, _, _ := fs.readControl(t, time.Second)
		if h.Type == TypeBye {
			sawBye = true
		}
		if h.Type == TypeHello && h.Gen == 2 {
			sawHello2 = true
		}
	}
	if !sawBye || !sawHello2 {
		t.Fatalf("expected BYE then HELLO gen2 (bye=%v hello2=%v)", sawBye, sawHello2)
	}
	// gen1 frames now stale
	fs.sendAudio(mux.LocalAddr(), 1, 0, 0, payload(1, FrameBytes))
	if !waitFor(t, func() bool { return c.Counters().StaleGen >= 1 }, time.Second) {
		t.Fatal("gen1 frame should be stale after resubscribe to gen2")
	}
	if col.len() != 0 {
		t.Fatal("stale frame delivered")
	}
}

func TestClientClose(t *testing.T) {
	mux := newLoopbackMux(t)
	mux.Run()
	c := NewClient(ClientConfig{Mux: mux, Deliver: func(Header, []byte) {}})
	fs := newFakeSource(t)
	c.Subscribe(fs.addr, 1, TransportUDP)
	fs.readControl(t, time.Second) // hello

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// BYE should arrive.
	h, _, _ := fs.readControl(t, time.Second)
	if h.Type != TypeBye {
		t.Fatalf("expected BYE, got %x", h.Type)
	}
	// idempotent
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}
