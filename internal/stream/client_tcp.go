package stream

import (
	"net"
	"time"
)

// TCP subscription path: dial SOURCE_PORT, send control frames and read
// length-prefixed audio off the same conn (D13). TCP carries no FEC; every
// audio chunk is a real frame.

// dialTCP dials the master's SOURCE_PORT, sends the initial prime-me HELLO
// (control frame on the conn), and starts readTCP. On dial failure it returns
// the error and starts nothing.
func (s *subscription) dialTCP() error {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.Dial("tcp", s.addr.String())
	if err != nil {
		return err
	}
	s.conn = conn
	if err := s.writeControlTCP(TypeHello, FlagPrimeMe); err != nil {
		_ = conn.Close()
		s.conn = nil
		return err
	}
	s.wg.Add(1)
	go s.readTCP()
	return nil
}

// writeControlTCP writes a length-prefixed control frame on the conn.
func (s *subscription) writeControlTCP(typ, flag byte) error {
	if s.conn == nil {
		return net.ErrClosed
	}
	h := Header{Magic: Magic, Type: typ, Gen: s.gen, PayloadLen: 1}
	chunk := h.AppendFrame(make([]byte, 0, HeaderSize+1), []byte{flag})
	return writeFrame(s.conn, chunk)
}

// readTCP loops reading length-prefixed chunks: TypeAudio -> ingest;
// TypeReconfig -> fire the hook (stop-flag => end of session). On EOF/error it
// returns; the watchdog / H re-subscribe.
func (s *subscription) readTCP() {
	defer s.wg.Done()
	for {
		select {
		case <-s.done:
			return
		default:
		}
		chunk, err := readFrame(s.conn)
		if err != nil {
			select {
			case <-s.done:
			default:
				s.log.Debug("tcp read ended", "err", err)
			}
			return
		}
		h, payload, derr := DecodeFrame(chunk)
		if derr != nil {
			s.ctr.malformed.Add(1)
			continue
		}
		switch h.Type {
		case TypeAudio:
			s.ingest(h, payload, true)
		case TypeReconfig:
			stop := len(payload) > 0 && payload[0]&FlagStop != 0
			if s.client != nil {
				s.client.fireReconfig(stop)
			}
		}
	}
}
