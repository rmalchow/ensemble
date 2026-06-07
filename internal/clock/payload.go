package clock

import (
	"encoding/binary"

	"ensemble/internal/stream"
)

// clockPayloadSize is the fixed clock payload: t1|t2|t3, three big-endian int64.
const clockPayloadSize = 24

// packetSize is header + payload for a clock datagram.
const packetSize = stream.HeaderSize + clockPayloadSize // 24 + 24 = 48

// encodeClock writes a clock datagram into dst[:packetSize] and returns the
// number of bytes written (packetSize). typ is TypeClockReq or TypeClockRsp.
// gen is the session generation; seq is the probe sequence (echoed in replies).
// On a request, t2 and t3 are 0. Panics if len(dst) < packetSize.
func encodeClock(dst []byte, typ byte, gen uint32, seq uint64, t1, t2, t3 int64) int {
	_ = dst[packetSize-1] // bounds check / panic if too short
	h := stream.Header{
		Magic:      stream.Magic,
		Type:       typ,
		Gen:        gen,
		Seq:        seq,
		PTS:        0,
		PayloadLen: clockPayloadSize,
	}
	h.Encode(dst)
	binary.BigEndian.PutUint64(dst[stream.HeaderSize+0:], uint64(t1))
	binary.BigEndian.PutUint64(dst[stream.HeaderSize+8:], uint64(t2))
	binary.BigEndian.PutUint64(dst[stream.HeaderSize+16:], uint64(t3))
	return packetSize
}

// decodeClock parses a clock datagram. It returns the header plus t1,t2,t3.
// It validates Magic, that PayloadLen >= clockPayloadSize, and that the buffer
// holds the full payload; otherwise returns an error (stream.ErrShort /
// stream.ErrBadMagic).
func decodeClock(pkt []byte) (h stream.Header, t1, t2, t3 int64, err error) {
	h, err = stream.Decode(pkt)
	if err != nil {
		return stream.Header{}, 0, 0, 0, err
	}
	if h.Magic != stream.Magic {
		return stream.Header{}, 0, 0, 0, stream.ErrBadMagic
	}
	if h.PayloadLen < clockPayloadSize || len(pkt) < stream.HeaderSize+clockPayloadSize {
		return stream.Header{}, 0, 0, 0, stream.ErrShort
	}
	t1 = int64(binary.BigEndian.Uint64(pkt[stream.HeaderSize+0:]))
	t2 = int64(binary.BigEndian.Uint64(pkt[stream.HeaderSize+8:]))
	t3 = int64(binary.BigEndian.Uint64(pkt[stream.HeaderSize+16:]))
	return h, t1, t2, t3, nil
}
