package source

import "ensemble/internal/stream"

// fecBlockSize is the number of data frames per FEC block (§8.4).
const fecBlockSize = 4

// fecBlock accumulates up to 4 audio payloads (XORed together, zero-padded to
// the longest) plus the base Seq/Gen of the block, then produces one XOR parity
// packet (TypeFEC). UDP only. Guarded by Server.mu.
type fecBlock struct {
	count   int
	baseSeq uint64
	gen     uint32
	parity  [stream.FrameBytes]byte // running XOR
	maxLen  int                     // longest payload folded -> parity PayloadLen
}

// fold XORs one payload into the running parity; the first frame records the
// block's base Seq.
func (b *fecBlock) fold(gen uint32, seq uint64, payload []byte) {
	if b.count == 0 {
		b.baseSeq = seq
		b.gen = gen
	}
	stream.XORInto(b.parity[:], payload)
	if len(payload) > b.maxLen {
		b.maxLen = len(payload)
	}
	b.count++
}

// ready reports whether a full block (4 frames) has been folded.
func (b *fecBlock) ready() bool { return b.count == fecBlockSize }

// parityPacket encodes the parity datagram for a full block and resets it.
// Returns nil (without reset) if empty.
func (b *fecBlock) parityPacket(buf []byte) []byte {
	if b.count == 0 {
		return nil
	}
	pkt := b.encode(buf)
	b.resetCounters()
	return pkt
}

// flushPartial emits parity for a 1..3-frame tail block (D13) and resets.
func (b *fecBlock) flushPartial(buf []byte) []byte {
	if b.count == 0 {
		return nil
	}
	pkt := b.encode(buf)
	b.resetCounters()
	return pkt
}

func (b *fecBlock) encode(buf []byte) []byte {
	h := stream.Header{
		Magic:      stream.Magic,
		Type:       stream.TypeFEC,
		Gen:        b.gen,
		Seq:        b.baseSeq,
		PTS:        0,
		PayloadLen: uint16(b.maxLen),
	}
	return h.AppendFrame(buf[:0], b.parity[:b.maxLen])
}

// reset clears all state and binds to a new generation.
func (b *fecBlock) reset(gen uint32) {
	b.gen = gen
	b.resetCounters()
}

func (b *fecBlock) resetCounters() {
	b.count = 0
	b.baseSeq = 0
	b.maxLen = 0
	for i := range b.parity {
		b.parity[i] = 0
	}
}
