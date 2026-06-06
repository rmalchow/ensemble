package fec

import (
	"crypto/subtle"
	"encoding/binary"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/wire"
)

// xorFEC is the default scheme (FECID = XORParity, doc 05 §5.5.1/§5.5.3, A.10):
// k source packets per parity group, time-interleaved across D lanes so a burst
// of up to D packets touches each lane at most once and is fully recoverable
// (A.10: "recoverable burst ≈ D packets ≈ 40 ms"). Overhead is 1/k (+12.5%).
// Any SINGLE loss within a lane group (its k sources + 1 repair) is recovered by
// XOR; two losses in one lane group are unrecoverable (the receiver conceals).
//
// Lane assignment is by sequence: lane(seq) = seq mod D (the §5.5.3 interleave
// diagram seq 0→A,1→B,2→C,3→D,4→A,…). Within a lane the covered source seqs are
// contiguous in the lane: {firstSeq, firstSeq+D, …, firstSeq+(k-1)·D}, so they
// are derivable from the repair's anchor seq and need not be listed on the wire.
//
// Pure compute: no goroutines, no I/O, no clock. Protect and Recover are a single
// XOR pass over ≤k payloads into reused per-lane buffers; only the emitted repair
// / recovered packet is freshly allocated (unavoidable — it leaves the package).
type xorFEC struct {
	cfg XORConfig

	enc []xorLane // D encode lanes (master side, Protect)
	dec []decLane // D decode lanes (follower side, Recover)
}

// --- repair payload framing -------------------------------------------------
//
// The repair packet is a normal wire packet with FlagRepair set and a synthetic
// payload that fec owns end-to-end (wire treats it as opaque bytes). Layout:
//
//	[0:2]  uint16  xorPayloadLen — XOR of the k covered sources' true PayloadLen
//	[2: ]  maxLen  xorPayload    — XOR of the k covered sources' payloads, each
//	                              zero-padded to maxLen before XOR (§5.5.1)
//
// Recovery of the one missing source:
//   - its true PayloadLen = xorPayloadLen XOR (XOR of the arrived sources' lens)
//   - its padded payload   = xorPayload    XOR (XOR of the arrived padded payloads)
//     XOR repairPayload, then trimmed to that PayloadLen.
//
// The missing source's SampleIndex/MasterMono are recovered the same way from the
// repair header (which carries the XOR of the covered sources' values); its Seq is
// derived from the lane anchor + position; StreamGen/CodecID/Rate100/keyframe are
// constant within a generation and copied from the repair header.
const repairLenHdr = 2 // bytes of xorPayloadLen prefix in the repair payload

// FECXORWire is the wire-layer FECID stamped into XOR repair/recovered packet
// headers (offset 7). fec.XORParity and wire.FECXORParity share the value 1; the
// alias keeps the dependency direction fec→wire explicit at the marshal site.
const FECXORWire = wire.FECXORParity

// NewXOR constructs the XOR-parity scheme with cfg (non-positive fields fall back
// to the A.12 defaults). Pre-allocates the D encode/decode lanes once.
func NewXOR(cfg XORConfig) FEC {
	cfg = cfg.normalize()
	f := &xorFEC{
		cfg: cfg,
		enc: make([]xorLane, cfg.Interleave),
		dec: make([]decLane, cfg.Interleave),
	}
	for i := range f.dec {
		f.dec[i].members = make([]decMember, cfg.K)
	}
	return f
}

// ID reports XORParity (1).
func (*xorFEC) ID() FECID { return XORParity }

// === master side: Protect ====================================================

// xorLane is one encode lane's open parity group: the running XOR of its (so far)
// covered source payloads, plus the metadata needed to stamp the repair header.
type xorLane struct {
	xor    []byte // running XOR of zero-padded payloads (reused buffer)
	xorLen uint16 // running XOR of true PayloadLen values
	maxLen int    // group max payload length so far
	count  int    // covered source packets so far (0..K)

	firstSeq  uint64 // anchor: seq of the first source in this open group
	xorSample int64  // running XOR of covered SampleIndex values
	xorMono   int64  // running XOR of covered MasterMono values
	streamGen uint64 // group generation (constant; from the first source)
	codecID   wire.CodecID
	rate100   uint16
	keyframe  bool // OR of the sources' keyframe flag (PCM => always true)
}

// Protect emits the source packet immediately and, when the source's lane group
// reaches K members, appends the lane's repair packet (doc 05 §5.5.3 pseudocode).
// pkt is a fully-marshaled source wire packet; it is parsed read-only to read the
// header fields the repair must carry. A malformed pkt (should not happen on the
// master's own output) is passed through unprotected.
func (f *xorFEC) Protect(seq uint64, pkt []byte) (out [][]byte) {
	hdr, payload, err := wire.Unmarshal(pkt)
	if err != nil {
		return [][]byte{pkt} // never protect bytes we cannot parse
	}

	lane := &f.enc[seq%uint64(f.cfg.Interleave)]
	if lane.count == 0 {
		lane.begin(seq, hdr)
	}
	lane.accumulate(hdr, payload)

	if lane.count < f.cfg.K {
		return [][]byte{pkt} // group still open — just the source
	}

	repair := lane.marshalRepair()
	lane.reset()
	if repair == nil {
		return [][]byte{pkt}
	}
	return [][]byte{pkt, repair}
}

// begin opens a fresh group on the lane, recording the anchor and the constant
// per-generation header fields from the first source.
func (l *xorLane) begin(seq uint64, hdr wire.Header) {
	l.firstSeq = seq
	l.streamGen = hdr.StreamGen
	l.codecID = hdr.CodecID
	l.rate100 = hdr.Rate100
	l.maxLen = 0
	l.xorLen = 0
	l.xorSample = 0
	l.xorMono = 0
	l.keyframe = false
	l.xor = l.xor[:0]
}

// accumulate folds one source into the open group: zero-pad-to-max XOR of the
// payload, plus XOR of the diffable header scalars used to reconstruct a loss.
func (l *xorLane) accumulate(hdr wire.Header, payload []byte) {
	if len(payload) > l.maxLen {
		l.grow(len(payload))
	}
	xorIntoPadded(l.xor, payload)
	l.xorLen ^= hdr.PayloadLen
	l.xorSample ^= hdr.SampleIndex
	l.xorMono ^= hdr.MasterMono
	l.keyframe = l.keyframe || hdr.Flags.Keyframe()
	l.count++
}

// grow extends the lane's XOR accumulator to n bytes, zero-filling the new tail
// (existing content is preserved — earlier payloads were implicitly zero-padded).
func (l *xorLane) grow(n int) {
	if cap(l.xor) >= n {
		old := len(l.xor)
		l.xor = l.xor[:n]
		for i := old; i < n; i++ {
			l.xor[i] = 0
		}
	} else {
		grown := make([]byte, n)
		copy(grown, l.xor)
		l.xor = grown
	}
	l.maxLen = n
}

// marshalRepair builds the lane's repair wire packet from the completed group.
// Returns nil only on an impossible marshal overflow (maxLen+2 > uint16), in
// which case the group is simply unprotected.
func (l *xorLane) marshalRepair() []byte {
	flags := wire.FlagRepair
	if l.keyframe {
		flags |= wire.FlagKeyframe
	}
	hdr := wire.Header{
		Flags:       flags,
		CodecID:     l.codecID,
		FECID:       FECXORWire,
		StreamGen:   l.streamGen,
		Seq:         l.firstSeq, // anchor; covered = firstSeq + i·D
		SampleIndex: l.xorSample,
		MasterMono:  l.xorMono,
		Rate100:     l.rate100,
	}
	body := make([]byte, repairLenHdr+l.maxLen)
	binary.BigEndian.PutUint16(body[:repairLenHdr], l.xorLen)
	copy(body[repairLenHdr:], l.xor)
	pkt, err := wire.Marshal(hdr, body)
	if err != nil {
		return nil
	}
	return pkt
}

// reset clears the lane for the next group (cheap: keeps the xor buffer's cap).
func (l *xorLane) reset() {
	l.count = 0
	l.maxLen = 0
	l.xor = l.xor[:0]
}

// === follower side: Recover ==================================================

// decLane is one decode lane's group-reassembly slot. Because lanes complete in
// order, the lane tracks a single open group keyed by its anchor firstSeq; an
// arrival for a newer group (anchor advanced by k·D) rolls the slot forward,
// dropping the stale partial group (it aged out — its window passed, §5.6.2).
type decLane struct {
	firstSeq  uint64 // anchor of the group this slot is reassembling
	have      bool   // slot is initialized for firstSeq
	members   []decMember
	gotCount  int  // arrived source members (0..K)
	hasRepair bool // the lane's repair packet has arrived

	repairPayload []byte // reused: padded XOR body (without the len prefix)
	repairXorLen  uint16
	repairSample  int64
	repairMono    int64
	streamGen     uint64
	codecID       wire.CodecID
	rate100       uint16
	keyframe      bool
}

// decMember is one source slot within a decode group.
type decMember struct {
	got        bool
	seq        uint64
	sampleIdx  int64
	masterMono int64
	payloadLen uint16
	payload    []byte // padded-to-group-max copy (for XOR); trimmed on output
}

// Recover ingests one received packet and returns any source packets that just
// became decodable. A source packet is always returned immediately (it is itself
// the data); if its (or the repair's) arrival completes a lane group missing
// exactly one member, the missing source is reconstructed and appended.
func (f *xorFEC) Recover(p wire.Packet) (recovered []wire.Packet) {
	d := f.cfg.Interleave
	lane := &f.dec[p.Header.Seq%uint64(d)]

	if p.Header.Flags.Repair() {
		f.ingestRepair(lane, p)
	} else {
		recovered = append(recovered, p)
		f.ingestSource(lane, p)
	}
	if rec, ok := f.tryRecover(lane); ok {
		recovered = append(recovered, rec)
	}
	return recovered
}

// laneAnchor returns the group anchor seq for a covered source seq: the smallest
// covered seq in the same lane group, i.e. floor to the group of k consecutive
// lane positions. lane(seq)=seq mod D; within the lane, position increments by D,
// so the anchor is seq − (laneIndex·D) where laneIndex is the source's position
// modulo k.
func (f *xorFEC) anchorOf(seq uint64) uint64 {
	d := uint64(f.cfg.Interleave)
	k := uint64(f.cfg.K)
	pos := (seq / d) % k // 0..k-1 position within the lane group
	return seq - pos*d
}

// rollTo points the lane slot at group anchor. Rolling FORWARD to a newer group
// discards the stale partial (the prior group resolved or aged out — only one
// open group per lane at a time; window > D·k guarantees this, §5.6.2). An
// arrival for an OLDER group (anchor < current) is for a group already aged out;
// rollTo reports ok=false so the caller ignores it (no mis-recovery, no panic).
func (l *decLane) rollTo(anchor uint64) (ok bool) {
	if l.have {
		switch {
		case l.firstSeq == anchor:
			return true
		case anchor < l.firstSeq:
			return false // stale: belongs to an aged-out group
		}
	}
	l.firstSeq = anchor
	l.have = true
	l.gotCount = 0
	l.hasRepair = false
	for i := range l.members {
		l.members[i] = decMember{}
	}
	return true
}

func (f *xorFEC) ingestSource(lane *decLane, p wire.Packet) {
	anchor := f.anchorOf(p.Header.Seq)
	if !lane.rollTo(anchor) {
		return // arrival for an aged-out group
	}
	d := uint64(f.cfg.Interleave)
	pos := int((p.Header.Seq - anchor) / d) // 0..k-1
	if pos < 0 || pos >= len(lane.members) {
		return
	}
	m := &lane.members[pos]
	if m.got {
		return // duplicate within the group; ignore
	}
	m.got = true
	m.seq = p.Header.Seq
	m.sampleIdx = p.Header.SampleIndex
	m.masterMono = p.Header.MasterMono
	m.payloadLen = p.Header.PayloadLen
	m.payload = clonePayload(m.payload, p.Payload)
	lane.gotCount++
	lane.streamGen = p.Header.StreamGen
	lane.codecID = p.Header.CodecID
	lane.rate100 = p.Header.Rate100
	if p.Header.Flags.Keyframe() {
		lane.keyframe = true
	}
}

func (f *xorFEC) ingestRepair(lane *decLane, p wire.Packet) {
	anchor := p.Header.Seq // repair's Seq IS the group anchor
	if !lane.rollTo(anchor) {
		return // repair for an aged-out group
	}
	if len(p.Payload) < repairLenHdr {
		return // malformed repair — ignore
	}
	lane.hasRepair = true
	lane.repairXorLen = binary.BigEndian.Uint16(p.Payload[:repairLenHdr])
	lane.repairPayload = clonePayload(lane.repairPayload, p.Payload[repairLenHdr:])
	lane.repairSample = p.Header.SampleIndex
	lane.repairMono = p.Header.MasterMono
	lane.streamGen = p.Header.StreamGen
	lane.codecID = p.Header.CodecID
	lane.rate100 = p.Header.Rate100
	if p.Header.Flags.Keyframe() {
		lane.keyframe = true
	}
}

// tryRecover reconstructs the single missing source of a lane group when the
// repair plus exactly k−1 of the k sources are present. Returns (pkt,true) on a
// recovery, (_,false) when nothing is recoverable (group complete, >1 missing,
// or repair absent).
func (f *xorFEC) tryRecover(lane *decLane) (wire.Packet, bool) {
	if !lane.have || !lane.hasRepair {
		return wire.Packet{}, false
	}
	k := f.cfg.K
	if lane.gotCount != k-1 {
		return wire.Packet{}, false // 0 or ≥2 missing, or already complete
	}

	// Locate the one missing position and XOR-accumulate the survivors.
	missing := -1
	xorLen := lane.repairXorLen
	xorSample := lane.repairSample
	xorMono := lane.repairMono
	recon := make([]byte, len(lane.repairPayload))
	copy(recon, lane.repairPayload)
	for i := range lane.members {
		m := &lane.members[i]
		if !m.got {
			missing = i
			continue
		}
		xorLen ^= m.payloadLen
		xorSample ^= m.sampleIdx
		xorMono ^= m.masterMono
		xorIntoPadded(recon, m.payload)
	}
	if missing < 0 {
		return wire.Packet{}, false // defensive: should not happen at k−1
	}

	// Reconstructed length must be sane (≤ the padded group width).
	if int(xorLen) > len(recon) {
		return wire.Packet{}, false // corrupt/inconsistent — conceal instead
	}
	d := uint64(f.cfg.Interleave)
	missingSeq := lane.firstSeq + uint64(missing)*d

	flags := wire.Flags(0)
	if lane.keyframe {
		flags |= wire.FlagKeyframe
	}
	rec := wire.Packet{
		Header: wire.Header{
			Flags:       flags,
			CodecID:     lane.codecID,
			FECID:       FECXORWire,
			StreamGen:   lane.streamGen,
			Seq:         missingSeq,
			SampleIndex: xorSample,
			MasterMono:  xorMono,
			PayloadLen:  xorLen,
			Rate100:     lane.rate100,
		},
		Payload: append([]byte(nil), recon[:xorLen]...),
	}
	// Mark the slot complete so a later arrival in this group does not re-recover.
	lane.members[missing].got = true
	lane.members[missing].seq = missingSeq
	lane.members[missing].payloadLen = xorLen
	lane.gotCount++
	return rec, true
}

// --- helpers ----------------------------------------------------------------

// xorIntoPadded XORs src into dst[:len(src)], treating any missing src tail as
// zero (dst is the group-max-width accumulator; src may be shorter). dst must be
// at least len(src) long — guaranteed by the callers (grow / repair sizing).
func xorIntoPadded(dst, src []byte) {
	if len(src) == 0 {
		return
	}
	subtle.XORBytes(dst[:len(src)], dst[:len(src)], src)
}

// clonePayload copies src into a reused buffer (resized as needed) and returns
// it, so the decode lane retains payloads past the read buffer's lifetime
// without per-call allocation in steady state (PCM payloads are fixed-size).
func clonePayload(dst, src []byte) []byte {
	if cap(dst) >= len(src) {
		dst = dst[:len(src)]
	} else {
		dst = make([]byte, len(src))
	}
	copy(dst, src)
	return dst
}
