package source

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"ensemble/internal/stream"
)

// tcpSub dials a source's SOURCE_PORT and exchanges length-prefixed frames.
type tcpSub struct {
	conn net.Conn
}

func newTCPSub(t *testing.T, src netip.AddrPort) *tcpSub {
	t.Helper()
	conn, err := net.DialTimeout("tcp", src.String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &tcpSub{conn: conn}
}

func (s *tcpSub) hello(primeMe bool) error {
	var flag byte
	if primeMe {
		flag = stream.FlagPrimeMe
	}
	h := stream.Header{Magic: stream.Magic, Type: stream.TypeHello, PayloadLen: 1}
	return writeTCPFrame(s.conn, h.AppendFrame(nil, []byte{flag}))
}

func (s *tcpSub) recvAudio(t *testing.T, want int, d time.Duration) []stream.Header {
	t.Helper()
	var out []stream.Header
	s.conn.SetReadDeadline(time.Now().Add(d))
	for len(out) < want {
		chunk, err := readTCPFrame(s.conn)
		if err != nil {
			break
		}
		h, _, derr := stream.DecodeFrame(chunk)
		if derr == nil && h.Type == stream.TypeAudio {
			out = append(out, h)
		}
	}
	return out
}

func TestServerSubscribeTCP(t *testing.T) {
	s, _, tap := newTestServer(t)
	s.StartSession(1, stream.TransportTCP, 150)
	for i := 0; i < 8; i++ {
		s.ReleaseFrame(int64(i)*stream.FrameNanos, pcm(byte(i)))
	}
	sub := newTCPSub(t, tap)
	if err := sub.hello(true); err != nil {
		t.Fatal(err)
	}
	// Continue releasing live frames.
	go func() {
		for i := 8; i < 16; i++ {
			s.ReleaseFrame(int64(i)*stream.FrameNanos, pcm(byte(i)))
			time.Sleep(2 * time.Millisecond)
		}
	}()
	hs := sub.recvAudio(t, 5, time.Second)
	if len(hs) < 5 {
		t.Fatalf("received %d audio frames want >=5", len(hs))
	}
	// Seqs must be non-decreasing (ordered on a TCP conn).
	for i := 1; i < len(hs); i++ {
		if hs[i].Seq < hs[i-1].Seq {
			t.Fatalf("out of order on TCP: %d then %d", hs[i-1].Seq, hs[i].Seq)
		}
	}
	if s.Stats().Connects != 1 || s.Stats().Primes != 1 {
		t.Fatalf("stats connects=%d primes=%d", s.Stats().Connects, s.Stats().Primes)
	}
}
