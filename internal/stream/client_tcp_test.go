package stream

import (
	"net"
	"net/netip"
	"testing"
	"time"
)

// fakeTCPSource accepts one subscriber and lets the test write frames.
type fakeTCPSource struct {
	ln   *net.TCPListener
	addr netip.AddrPort
	conn net.Conn
}

func newFakeTCPSource(t *testing.T) *fakeTCPSource {
	t.Helper()
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	ap, _ := netip.ParseAddrPort(ln.Addr().String())
	fs := &fakeTCPSource{ln: ln, addr: ap}
	t.Cleanup(func() {
		ln.Close()
		if fs.conn != nil {
			fs.conn.Close()
		}
	})
	return fs
}

func (f *fakeTCPSource) accept(t *testing.T) {
	t.Helper()
	f.ln.SetDeadline(time.Now().Add(2 * time.Second))
	conn, err := f.ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	f.conn = conn
}

func (f *fakeTCPSource) readControl(t *testing.T) Header {
	t.Helper()
	f.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	chunk, err := readFrame(f.conn)
	if err != nil {
		t.Fatal(err)
	}
	h, _, _ := DecodeFrame(chunk)
	return h
}

func (f *fakeTCPSource) sendAudio(seq uint64, pts int64, pay []byte) error {
	h := Header{Magic: Magic, Type: TypeAudio, Gen: 1, Seq: seq, PTS: pts, PayloadLen: uint16(len(pay))}
	return writeFrame(f.conn, h.AppendFrame(nil, pay))
}

func (f *fakeTCPSource) sendReconfig(stop bool) error {
	var flag byte
	if stop {
		flag = FlagStop
	}
	h := Header{Magic: Magic, Type: TypeReconfig, Gen: 1, PayloadLen: 1}
	return writeFrame(f.conn, h.AppendFrame(nil, []byte{flag}))
}

func TestClientTCPDeliver(t *testing.T) {
	mux := newLoopbackMux(t)
	mux.Run()
	col := &collector{}
	c := NewClient(ClientConfig{Mux: mux, Deliver: col.deliver})
	defer c.Close()
	fs := newFakeTCPSource(t)

	done := make(chan struct{})
	go func() {
		fs.accept(t)
		if h := fs.readControl(t); h.Type != TypeHello {
			t.Errorf("first frame not HELLO: %x", h.Type)
		}
		for i := uint64(0); i < 3; i++ {
			if err := fs.sendAudio(i, int64(i)*FrameNanos, payload(byte(i+1), FrameBytes)); err != nil {
				t.Errorf("sendAudio: %v", err)
			}
		}
		close(done)
	}()

	if err := c.Subscribe(fs.addr, 1, TransportTCP); err != nil {
		t.Fatal(err)
	}
	<-done
	if !waitFor(t, func() bool { return col.len() == 3 }, 2*time.Second) {
		t.Fatalf("delivered %d want 3", col.len())
	}
	ctr := c.Counters()
	if ctr.Delivered != 3 || ctr.FECParity != 0 || ctr.Recovered != 0 {
		t.Fatalf("counters: %+v", ctr)
	}
}

func TestClientTCPReconfigStop(t *testing.T) {
	mux := newLoopbackMux(t)
	mux.Run()
	gotStop := make(chan bool, 1)
	c := NewClient(ClientConfig{
		Mux:      mux,
		Deliver:  func(Header, []byte) {},
		Reconfig: func(stop bool) { gotStop <- stop },
	})
	defer c.Close()
	fs := newFakeTCPSource(t)

	go func() {
		fs.accept(t)
		fs.readControl(t) // HELLO
		fs.sendReconfig(true)
	}()
	if err := c.Subscribe(fs.addr, 1, TransportTCP); err != nil {
		t.Fatal(err)
	}
	select {
	case stop := <-gotStop:
		if !stop {
			t.Fatal("expected stop=true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no RECONFIG(stop) delivered over TCP")
	}
}

func TestClientTCPDialFailure(t *testing.T) {
	mux := newLoopbackMux(t)
	mux.Run()
	c := NewClient(ClientConfig{Mux: mux, Deliver: func(Header, []byte) {}})
	defer c.Close()
	// Nothing listening on this port.
	bad := netip.MustParseAddrPort("127.0.0.1:1")
	if err := c.Subscribe(bad, 1, TransportTCP); err == nil {
		t.Fatal("expected dial error")
	}
}
