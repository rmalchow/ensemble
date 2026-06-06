package sink_net

// transport.go (P7.1) declares the audio wire transport selector shared by the
// origin (master) and the receiver (follower) halves of the stream plane. It
// mirrors GroupRecord.Profile.Transport ("udp"|"tcp", 07 §2.4 / README §6.5) and
// is the single place the string<->enum mapping lives so neither half hand-rolls
// it (05 §5.9, D2).
//
// The enum lives in sink_net (not origin) because origin imports sink_net's
// Transport for its Config field — keeping the type in the lower of the two
// packages avoids any import edge from sink_net back up to origin (01 §2: both
// are leaves of the audio plane and must not import each other in a cycle).

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
)

// Transport selects the audio wire transport for a group (D2 / 05 §5.9). It
// mirrors the GroupRecord.Profile.Transport string enum: "udp" (default) | "tcp".
type Transport int

const (
	// TransportUDP is the default: UDP unicast + FEC (05 §5.6).
	TransportUDP Transport = iota
	// TransportTCP is the reliable fallback: a length-prefixed stream with FEC
	// forced None, since TCP retransmission already guarantees delivery (05 §5.9).
	TransportTCP
)

// ParseTransport maps the ConfigDoc/API string enum to a Transport. Only the
// exact token "tcp" selects TCP; anything else (including "udp", "" and garbage)
// falls back to the UDP default (README §6.5 — UDP is the documented default).
func ParseTransport(s string) Transport {
	if s == "tcp" {
		return TransportTCP
	}
	return TransportUDP
}

// String renders a Transport as its canonical JSON enum token ("udp"|"tcp",
// README §6.5). An out-of-range value renders as "udp" (the safe default).
func (t Transport) String() string {
	if t == TransportTCP {
		return "tcp"
	}
	return "udp"
}

// lenPrefixBytes is the fixed 2-byte big-endian length prefix in front of each
// marshaled wire packet on the TCP stream (05 §5.9). It delimits packets; the
// header's own PayloadLen is present but not relied on for framing.
const lenPrefixBytes = 2

// maxFrame bounds one length-prefixed TCP frame (a full ESND packet). It matches
// the UDP maxDatagram budget (44-byte header + a full PCM chunk + slack) so a
// hostile or corrupt prefix cannot drive an unbounded allocation (03 §6.3).
const maxFrame = maxDatagram

// errFrameTooLarge guards the deframer against an implausible length prefix.
var errFrameTooLarge = errors.New("sink_net: tcp frame exceeds max size")

// WriteFrame writes one length-prefixed wire packet to w: a 2-byte big-endian
// length followed by pkt verbatim (05 §5.9). It is the framing primitive shared
// by the origin's TCP sender (which imports this package) and the receiver's TCP
// deframer; a single packet never exceeds maxFrame at the canonical profile, and
// the uint16 prefix bounds it structurally. Exported so origin reuses the exact
// framing the receiver deframes (one definition, no drift).
func WriteFrame(w io.Writer, pkt []byte) error {
	if len(pkt) > maxFrame {
		return errFrameTooLarge
	}
	var hdr [lenPrefixBytes]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(pkt)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(pkt)
	return err
}

// frameReader deframes a length-prefixed TCP stream back into individual wire
// packets (05 §5.9). It owns a small reusable buffer so steady-state deframing is
// allocation-free; the returned slice is valid only until the next next() call
// (the caller Clones before retaining, exactly as the UDP path does — receiver.go
// deliver/window already Clone). It transparently handles TCP read boundaries
// (split and coalesced) via io.ReadFull on the length and the body.
type frameReader struct {
	r   io.Reader
	hdr [lenPrefixBytes]byte
	buf []byte
}

// newFrameReader wraps r with a deframer sized for the canonical profile.
func newFrameReader(r io.Reader) *frameReader {
	return &frameReader{r: r, buf: make([]byte, maxFrame)}
}

// next reads one complete frame, returning the marshaled wire packet bytes. It
// returns io.EOF when the stream ends cleanly between frames. A frame whose
// declared length exceeds maxFrame is rejected (errFrameTooLarge) so a corrupt
// prefix cannot allocate unbounded memory. The returned slice aliases the
// reader's internal buffer — valid only until the next next().
func (fr *frameReader) next() ([]byte, error) {
	if _, err := io.ReadFull(fr.r, fr.hdr[:]); err != nil {
		return nil, err // io.EOF (clean) or io.ErrUnexpectedEOF (truncated prefix)
	}
	n := int(binary.BigEndian.Uint16(fr.hdr[:]))
	if n > maxFrame {
		return nil, errFrameTooLarge
	}
	if _, err := io.ReadFull(fr.r, fr.buf[:n]); err != nil {
		return nil, err
	}
	return fr.buf[:n], nil
}

// dialTCP connects a TCP socket to addr (used by the origin's per-listener
// sender). A connected stream lets Write copy into the socket and TCP frame it.
func dialTCP(addr *net.TCPAddr) (*net.TCPConn, error) {
	return net.DialTCP("tcp", nil, addr)
}
