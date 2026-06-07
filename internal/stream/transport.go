package stream

import (
	"encoding/binary"
	"errors"
	"io"
)

// Transport selects the wire transport for a subscription (group setting §8.4),
// mirroring contracts.GroupSettings.Transport ("udp" | "tcp").
type Transport int

const (
	// TransportUDP uses datagrams + XOR FEC (the §8.4 default).
	TransportUDP Transport = iota
	// TransportTCP uses a persistent length-prefixed conn on SOURCE_PORT.
	TransportTCP
)

func (t Transport) String() string {
	switch t {
	case TransportTCP:
		return "tcp"
	default:
		return "udp"
	}
}

// ParseTransport maps the group-setting string to a Transport. Unknown values
// map to UDP so a malformed record never wedges a subscription.
func ParseTransport(s string) Transport {
	switch s {
	case "tcp", "TCP":
		return TransportTCP
	default:
		return TransportUDP
	}
}

// --- TCP length framing (D13: uint32 big-endian length prefix) --------------

// writeFrame writes a single length-prefixed chunk (uint32-BE len | chunk).
func writeFrame(w io.Writer, chunk []byte) error {
	var lp [4]byte
	binary.BigEndian.PutUint32(lp[:], uint32(len(chunk)))
	if _, err := w.Write(lp[:]); err != nil {
		return err
	}
	_, err := w.Write(chunk)
	return err
}

// maxFrameLen bounds a length-prefixed chunk so a hostile/garbled prefix can't
// trigger a huge allocation. Header + a generous payload ceiling.
const maxFrameLen = 64 * 1024

// errFrameTooBig guards against absurd length prefixes.
var errFrameTooBig = errors.New("stream: framed chunk too large")

// readFrame reads one length-prefixed chunk into a freshly allocated buffer.
func readFrame(r io.Reader) ([]byte, error) {
	var lp [4]byte
	if _, err := io.ReadFull(r, lp[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(lp[:])
	if n < HeaderSize || n > maxFrameLen {
		return nil, errFrameTooBig
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
