package clock

// Copied from gitlab.rand0m.me/ruben/go/media/internal/clock/server.go, then
// gated. The unauthenticated clock plane MUST drop requests whose source IP is
// not a current cluster member (spine §4 clock plane, A.8, doc 04 §4.1.2). The
// gate is an injected per-packet predicate — clock does NOT import
// internal/allowlist (P3.1 §6); cmd binds set.AllowedAddr into ListenGated.

import (
	"errors"
	"net"
	"net/netip"
)

// Server answers clock requests over UDP. The elected master runs one of these;
// for every accepted request it stamps the receive time (t2) and the send time
// (t3) and echoes the follower's t1, so the follower can complete the
// four-timestamp math.
type Server struct {
	conn    *net.UDPConn
	allowed func(netip.Addr) bool
}

// allowAll is the ungated predicate used by Listen (tests/solo).
func allowAll(netip.Addr) bool { return true }

// Listen binds an ungated UDP clock server on addr; equivalent to
// ListenGated(addr, allowAll). Convenience for tests and solo masters.
func Listen(addr string) (*Server, error) {
	return ListenGated(addr, allowAll)
}

// ListenGated binds a UDP clock server on addr (e.g. ":9000") and starts serving
// in the background, dropping every request whose source IP fails the allowed
// predicate before it reaches the decoder (P2.4 §5.3). allowed is typically
// set.AllowedAddr from the shared *allowlist.Set (P2.4); a nil predicate denies
// all. Call Close to stop it.
func ListenGated(addr string, allowed func(netip.Addr) bool) (*Server, error) {
	if allowed == nil {
		allowed = func(netip.Addr) bool { return false }
	}
	uaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", uaddr)
	if err != nil {
		return nil, err
	}
	s := &Server{conn: conn, allowed: allowed}
	go s.serve()
	return s, nil
}

// Addr returns the address the server is bound to.
func (s *Server) Addr() net.Addr { return s.conn.LocalAddr() }

// Close stops the server.
func (s *Server) Close() error { return s.conn.Close() }

func (s *Server) serve() {
	buf := make([]byte, PacketSize)
	for {
		// ReadFromUDPAddrPort yields a netip.AddrPort with no per-packet
		// *net.UDPAddr alloc on the Pi hot path.
		n, src, err := s.conn.ReadFromUDPAddrPort(buf)
		t2 := nowMono() // stamp receive ASAP, before any branch
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		// Drop non-member sources before unmarshal: untrusted bytes never reach
		// the decoder, no reply is sent (A.8 silent drop). src.Addr() is passed
		// unmodified — the predicate owns mapped-v4 normalization (P3.1 Q3).
		if !s.allowed(src.Addr()) {
			continue
		}
		req, err := unmarshal(buf[:n])
		if err != nil || req.kind != kindRequest {
			continue
		}
		reply := packet{
			kind: kindReply,
			seq:  req.seq,
			t1:   req.t1, // echo follower send time
			t2:   t2,
			t3:   nowMono(), // stamp send as late as possible
		}
		// Recover *net.UDPAddr once per accepted packet only.
		_, _ = s.conn.WriteToUDP(reply.marshal(), net.UDPAddrFromAddrPort(src))
	}
}
