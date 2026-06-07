package stream

// UDP subscription path. HELLO/BYE/RESTART go out from the member's STREAM_PORT
// mux socket to the master's SOURCE_PORT, so audio flows back to the observed
// addr (D22). Audio/FEC/RECONFIG arrive on the mux and are dispatched by the
// Client (see client.go onUDP/onReconfigUDP).

// helloUDP sends a control datagram (TypeHello/Bye/Restart) from the member's
// STREAM_PORT mux socket to the master's SOURCE_PORT. primeMe sets bit0 of the
// 1-byte payload flag (set on the initial HELLO and on RESTART; clear on
// keepalive HELLO and BYE).
func (s *subscription) helloUDP(typ byte, primeMe bool) {
	var flag byte
	if primeMe {
		flag = FlagPrimeMe
	}
	h := Header{
		Magic:      Magic,
		Type:       typ,
		Gen:        s.gen,
		Seq:        0,
		PTS:        0,
		PayloadLen: 1,
	}
	pkt := h.AppendFrame(make([]byte, 0, HeaderSize+1), []byte{flag})
	if _, err := s.mux.WriteTo(pkt, s.addr); err != nil {
		s.log.Debug("udp control write failed", "type", typ, "err", err)
	}
}
