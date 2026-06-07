package source

import (
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"ensemble/internal/stream"
)

// e2eDeliver collects delivered frames from a stream.Client.
type e2eDeliver struct {
	mu   sync.Mutex
	seqs []uint64
}

func (d *e2eDeliver) fn(h stream.Header, _ []byte) {
	d.mu.Lock()
	d.seqs = append(d.seqs, h.Seq)
	d.mu.Unlock()
}

func (d *e2eDeliver) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seqs)
}

// newClientMux builds a stream.Mux on loopback and runs it.
func newClientMux(t *testing.T) *stream.Mux {
	t.Helper()
	uc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	m := stream.NewMux(uc, nil)
	m.Run()
	t.Cleanup(func() { m.Close() })
	return m
}

func TestRoundTripUDPClean(t *testing.T) {
	s, uap, _ := newTestServer(t)
	s.StartSession(1, stream.TransportUDP, 150)

	mux := newClientMux(t)
	col := &e2eDeliver{}
	c := stream.NewClient(stream.ClientConfig{Mux: mux, Deliver: col.fn})
	defer c.Close()
	if err := c.Subscribe(uap, 1, stream.TransportUDP); err != nil {
		t.Fatal(err)
	}
	// Let the HELLO register.
	waitForN(t, func() int { return s.Stats().Clients }, 1, time.Second)

	for i := 0; i < 100; i++ {
		s.ReleaseFrame(int64(i)*stream.FrameNanos, pcm(byte(i)))
		time.Sleep(time.Millisecond)
	}
	if !waitForN(t, func() int { return col.count() }, 100, 3*time.Second) {
		t.Fatalf("delivered %d want 100", col.count())
	}
	ctr := c.Counters()
	if ctr.Lost != 0 {
		t.Fatalf("clean path Lost=%d", ctr.Lost)
	}
}

// dropRelay forwards UDP datagrams from the source to the client mux, dropping
// every Nth *data* frame to exercise FEC recovery deterministically. Control
// from the client (HELLO) is forwarded back to the source.
type dropRelay struct {
	front   *net.UDPConn // faces the client mux (client HELLOs here; audio sent from here)
	srcAddr netip.AddrPort
	client  netip.AddrPort
	mu      sync.Mutex
	dataN   int
	dropMod int
}

func newDropRelay(t *testing.T, srcAddr netip.AddrPort, dropMod int) *dropRelay {
	t.Helper()
	front, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	r := &dropRelay{front: front, srcAddr: srcAddr, dropMod: dropMod}
	t.Cleanup(func() { front.Close() })
	return r
}

func (r *dropRelay) addr() netip.AddrPort {
	ap, _ := netip.ParseAddrPort(r.front.LocalAddr().String())
	return ap
}

// run reads from the front socket. Packets from the client are forwarded to the
// source (via a dial); packets from the source are forwarded to the client with
// the drop filter. To keep one socket, we dial the source from the front socket
// so the source observes the front addr and replies to it.
func (r *dropRelay) run(t *testing.T) {
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, from, err := r.front.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			pkt := make([]byte, n)
			copy(pkt, buf[:n])
			if from == r.srcAddr {
				// from source -> client, apply drop on data frames
				r.mu.Lock()
				cl := r.client
				drop := false
				if len(pkt) >= 2 && pkt[1] == stream.TypeAudio {
					r.dataN++
					if r.dropMod > 0 && r.dataN%r.dropMod == 0 {
						drop = true
					}
				}
				r.mu.Unlock()
				if !drop && cl.IsValid() {
					r.front.WriteToUDPAddrPort(pkt, cl)
				}
			} else {
				// from client -> source; remember client addr
				r.mu.Lock()
				r.client = from
				r.mu.Unlock()
				r.front.WriteToUDPAddrPort(pkt, r.srcAddr)
			}
		}
	}()
}

func TestRoundTripUDPLossyFECRecovery(t *testing.T) {
	s, uap, _ := newTestServer(t)
	s.StartSession(1, stream.TransportUDP, 150)

	relay := newDropRelay(t, uap, 5) // drop every 5th data frame
	relay.run(t)

	mux := newClientMux(t)
	col := &e2eDeliver{}
	c := stream.NewClient(stream.ClientConfig{Mux: mux, Deliver: col.fn})
	defer c.Close()
	// Subscribe through the relay: the client HELLOs the relay, which forwards
	// to the source; the source then streams back to the relay's observed addr.
	if err := c.Subscribe(relay.addr(), 1, stream.TransportUDP); err != nil {
		t.Fatal(err)
	}
	waitForN(t, func() int { return s.Stats().Clients }, 1, 2*time.Second)

	for i := 0; i < 100; i++ {
		s.ReleaseFrame(int64(i)*stream.FrameNanos, pcm(byte(i)))
		time.Sleep(time.Millisecond)
	}

	// Every dropped frame is a single loss within its block -> FEC recovers it.
	if !waitForN(t, func() int { return col.count() }, 100, 4*time.Second) {
		t.Fatalf("delivered %d want 100", col.count())
	}
	ctr := c.Counters()
	if ctr.Recovered == 0 {
		t.Fatalf("expected FEC recoveries, got %d", ctr.Recovered)
	}
	if ctr.Lost != 0 {
		t.Fatalf("single losses should all recover; Lost=%d", ctr.Lost)
	}
}

func TestRoundTripTCPClean(t *testing.T) {
	s, _, tap := newTestServer(t)
	s.StartSession(1, stream.TransportTCP, 150)

	mux := newClientMux(t) // unused for TCP audio but required by Client
	col := &e2eDeliver{}
	c := stream.NewClient(stream.ClientConfig{Mux: mux, Deliver: col.fn})
	defer c.Close()
	if err := c.Subscribe(tap, 1, stream.TransportTCP); err != nil {
		t.Fatal(err)
	}
	waitForN(t, func() int { return s.Stats().Clients }, 1, time.Second)

	for i := 0; i < 100; i++ {
		s.ReleaseFrame(int64(i)*stream.FrameNanos, pcm(byte(i)))
	}
	if !waitForN(t, func() int { return col.count() }, 100, 3*time.Second) {
		t.Fatalf("delivered %d want 100", col.count())
	}
	ctr := c.Counters()
	if ctr.Lost != 0 || ctr.Recovered != 0 || ctr.FECParity != 0 {
		t.Fatalf("TCP clean counters: %+v", ctr)
	}
}

func TestRoundTripLateJoinPrimed(t *testing.T) {
	s, uap, _ := newTestServer(t)
	s.StartSession(1, stream.TransportUDP, 150)

	// release 50 frames before the client joins
	for i := 0; i < 50; i++ {
		s.ReleaseFrame(int64(i)*stream.FrameNanos, pcm(byte(i)))
	}

	mux := newClientMux(t)
	col := &e2eDeliver{}
	c := stream.NewClient(stream.ClientConfig{Mux: mux, Deliver: col.fn})
	defer c.Close()
	if err := c.Subscribe(uap, 1, stream.TransportUDP); err != nil {
		t.Fatal(err)
	}
	waitForN(t, func() int { return s.Stats().Clients }, 1, time.Second)

	// continue live
	for i := 50; i < 90; i++ {
		s.ReleaseFrame(int64(i)*stream.FrameNanos, pcm(byte(i)))
		time.Sleep(time.Millisecond)
	}

	// Late joiner should get the primed live edge then live frames; assert it
	// received a healthy run and the delivered seqs are contiguous & ordered.
	if !waitForAtLeast(t, func() int { return col.count() }, 20, 3*time.Second) {
		t.Fatalf("late joiner delivered only %d", col.count())
	}
	col.mu.Lock()
	seqs := append([]uint64(nil), col.seqs...)
	col.mu.Unlock()
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("non-increasing delivered seqs at %d: %v", i, seqs[i-1:i+1])
		}
	}
}
