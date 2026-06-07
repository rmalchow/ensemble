package stream

// FEC: XOR parity over fixed blocks of 4 audio frames (§8.4). The source folds
// each released frame into a fecBlock and, after every 4th frame, emits one
// parity datagram (TypeFEC). A subscriber that loses exactly one of the four
// data frames reconstructs it from the parity plus the other three.

// fecBlockSize is the number of data frames per FEC block.
const fecBlockSize = 4

// --- source side ------------------------------------------------------------

// fecBlock accumulates up to 4 audio payloads (XORed together, zero-padded to
// the longest) plus the base Seq/Gen of the block, then produces one XOR parity
// packet. UDP only. Guarded by the caller's mutex.
type fecBlock struct {
	count   int
	baseSeq uint64
	gen     uint32
	parity  [FrameBytes]byte // running XOR
	maxLen  int              // longest payload folded -> parity PayloadLen
}

// fold XORs one payload into the running parity. The first frame of a block
// records the base Seq. count is incremented.
func (b *fecBlock) fold(gen uint32, seq uint64, payload []byte) {
	if b.count == 0 {
		b.baseSeq = seq
		b.gen = gen
	}
	XORInto(b.parity[:], payload)
	if len(payload) > b.maxLen {
		b.maxLen = len(payload)
	}
	b.count++
}

// ready reports whether a full block (4 frames) has been folded.
func (b *fecBlock) ready() bool { return b.count == fecBlockSize }

// parityPacket encodes the parity datagram for a full block and resets the
// block. Returns nil (without reset) if the block is empty. The header carries
// Seq = baseSeq, PayloadLen = maxLen, PTS = 0.
func (b *fecBlock) parityPacket(buf []byte) []byte {
	if b.count == 0 {
		return nil
	}
	pkt := b.encode(buf)
	b.resetCounters()
	return pkt
}

// flushPartial emits parity for a 1..3-frame tail block at StopSession (D13)
// and resets. Returns nil if nothing is pending.
func (b *fecBlock) flushPartial(buf []byte) []byte {
	if b.count == 0 {
		return nil
	}
	pkt := b.encode(buf)
	b.resetCounters()
	return pkt
}

func (b *fecBlock) encode(buf []byte) []byte {
	h := Header{
		Magic:      Magic,
		Type:       TypeFEC,
		Gen:        b.gen,
		Seq:        b.baseSeq,
		PTS:        0,
		PayloadLen: uint16(b.maxLen),
	}
	return h.AppendFrame(buf[:0], b.parity[:b.maxLen])
}

// reset clears all state and binds the accumulator to a new generation.
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

// --- client side ------------------------------------------------------------

// recoveryWindow tracks, per FEC block (keyed by baseSeq within a gen), which
// of the 4 data frames + the parity have arrived. When exactly one data frame
// is missing AND parity is present, it reconstructs the missing payload.
type recoveryWindow struct {
	gen    uint32
	blocks map[uint64]*fecState
}

type fecState struct {
	baseSeq uint64
	have    [fecBlockSize]bool
	payload [fecBlockSize][]byte
	pts     [fecBlockSize]int64
	parity  []byte
	parLen  int
	hasPar  bool
}

func newRecoveryWindow(gen uint32) recoveryWindow {
	return recoveryWindow{gen: gen, blocks: make(map[uint64]*fecState)}
}

// reset drops all pending blocks and rebinds to a new generation.
func (w *recoveryWindow) reset(gen uint32) {
	w.gen = gen
	w.blocks = make(map[uint64]*fecState)
}

// blockBase returns the base Seq of the block containing seq.
func blockBase(seq uint64) uint64 { return seq - seq%fecBlockSize }

func (w *recoveryWindow) state(baseSeq uint64) *fecState {
	st := w.blocks[baseSeq]
	if st == nil {
		st = &fecState{baseSeq: baseSeq}
		w.blocks[baseSeq] = st
	}
	return st
}

// observeData records a received data frame. If it completes a single-loss
// recovery, returns the recovered (seq, pts, payload) and ok=true.
func (w *recoveryWindow) observeData(gen uint32, seq uint64, pts int64, payload []byte) (uint64, int64, []byte, bool) {
	if gen != w.gen {
		return 0, 0, nil, false
	}
	base := blockBase(seq)
	st := w.state(base)
	idx := int(seq - base)
	if st.have[idx] {
		return 0, 0, nil, false
	}
	cp := make([]byte, len(payload))
	copy(cp, payload)
	st.payload[idx] = cp
	st.pts[idx] = pts
	st.have[idx] = true
	return w.tryRecover(st)
}

// observeParity records a parity packet for the block based at baseSeq.
func (w *recoveryWindow) observeParity(gen uint32, baseSeq uint64, parity []byte) (uint64, int64, []byte, bool) {
	if gen != w.gen {
		return 0, 0, nil, false
	}
	base := blockBase(baseSeq)
	st := w.state(base)
	if st.hasPar {
		return 0, 0, nil, false
	}
	cp := make([]byte, len(parity))
	copy(cp, parity)
	st.parity = cp
	st.parLen = len(parity)
	st.hasPar = true
	return w.tryRecover(st)
}

// tryRecover reconstructs the single missing frame of st if exactly one data
// frame is absent and parity is present. The recovered block is then removed.
func (w *recoveryWindow) tryRecover(st *fecState) (uint64, int64, []byte, bool) {
	if !st.hasPar {
		return 0, 0, nil, false
	}
	missing := -1
	haveCount := 0
	for i := 0; i < fecBlockSize; i++ {
		if st.have[i] {
			haveCount++
		} else if missing == -1 {
			missing = i
		} else {
			// more than one missing -> unrecoverable for now
			return 0, 0, nil, false
		}
	}
	if missing == -1 {
		// all data present; parity redundant. Block done.
		delete(w.blocks, st.baseSeq)
		return 0, 0, nil, false
	}
	// Recover the one missing frame: missing = parity XOR (all present data).
	recovered := make([]byte, st.parLen)
	copy(recovered, st.parity)
	for i := 0; i < fecBlockSize; i++ {
		if st.have[i] {
			XORInto(recovered, st.payload[i])
		}
	}
	// Derive PTS from a present neighbor (PTS linear in Seq within a gen).
	var nbrIdx int = -1
	for i := 0; i < fecBlockSize; i++ {
		if st.have[i] {
			nbrIdx = i
			break
		}
	}
	rseq := st.baseSeq + uint64(missing)
	var rpts int64
	if nbrIdx >= 0 {
		rpts = st.pts[nbrIdx] + int64(missing-nbrIdx)*FrameNanos
	}
	delete(w.blocks, st.baseSeq)
	return rseq, rpts, recovered, true
}
