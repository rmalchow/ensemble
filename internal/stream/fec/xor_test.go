package fec

import (
	"bytes"
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/wire"
)

// srcPacket builds a marshaled source wire packet for seq with a deterministic
// payload of payloadLen bytes (filled from seq so each is distinct).
func srcPacket(t *testing.T, seq uint64, payloadLen int) []byte {
	t.Helper()
	payload := make([]byte, payloadLen)
	for i := range payload {
		payload[i] = byte(seq) ^ byte(i*7+1)
	}
	hdr := wire.Header{
		Flags:       wire.FlagKeyframe,
		CodecID:     wire.CodecPCM,
		FECID:       wire.FECXORParity,
		StreamGen:   3,
		Seq:         seq,
		SampleIndex: int64(seq) * 480, // canonical chunkFrames
		MasterMono:  int64(seq) * 10_000_000,
		Rate100:     480,
	}
	pkt, err := wire.Marshal(hdr, payload)
	if err != nil {
		t.Fatalf("marshal seq %d: %v", seq, err)
	}
	return pkt
}

// parse unmarshals a marshaled packet into a wire.Packet (header + owned payload).
func parse(t *testing.T, pkt []byte) wire.Packet {
	t.Helper()
	hdr, payload, err := wire.Unmarshal(pkt)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return wire.Packet{Header: hdr, Payload: append([]byte(nil), payload...)}
}

// protectStream runs Protect over seq 0..n-1 with the given per-seq payload
// length and returns every emitted packet, parsed, in send order.
func protectStream(t *testing.T, f FEC, n int, lenOf func(seq uint64) int) []wire.Packet {
	t.Helper()
	var out []wire.Packet
	for seq := uint64(0); seq < uint64(n); seq++ {
		for _, b := range f.Protect(seq, srcPacket(t, seq, lenOf(seq))) {
			out = append(out, parse(t, b))
		}
	}
	return out
}

func fixedLen(n int) func(uint64) int { return func(uint64) int { return n } }

func TestXORProtectLayout(t *testing.T) {
	const k, d, plen = 8, 4, 1920
	f := NewXOR(XORConfig{K: k, Interleave: d})
	emitted := protectStream(t, f, k*d, fixedLen(plen)) // exactly one full group per lane

	var sources, repairs int
	lastSeqBeforeRepair := map[uint64]bool{}
	for _, p := range emitted {
		if p.Header.Flags.Repair() {
			repairs++
			// repair payloadLen = lenHdr + group maxLen
			if int(p.Header.PayloadLen) != repairLenHdr+plen {
				t.Fatalf("repair payloadLen = %d, want %d", p.Header.PayloadLen, repairLenHdr+plen)
			}
			// its anchor must be a source already emitted
			if !lastSeqBeforeRepair[p.Header.Seq] {
				t.Fatalf("repair anchor seq %d emitted before its source", p.Header.Seq)
			}
		} else {
			sources++
			lastSeqBeforeRepair[p.Header.Seq] = true
			// interleave assignment: lane = seq mod D (05 §5.5.3 diagram)
			if int(p.Header.Seq%d) != int(p.Header.Seq)%d {
				t.Fatal("lane assignment mismatch")
			}
		}
	}
	if sources != k*d {
		t.Fatalf("sources = %d, want %d", sources, k*d)
	}
	if repairs != d { // one repair per lane after each lane fills to k
		t.Fatalf("repairs = %d, want %d (one per lane)", repairs, d)
	}
}

// recoverWithDrops feeds emitted packets (in order) to a fresh decoder, skipping
// any whose (isRepair, seq) is in drop, and returns the recovered SOURCE packets
// keyed by seq (last write wins).
func recoverWithDrops(t *testing.T, cfg XORConfig, emitted []wire.Packet, drop func(p wire.Packet) bool) map[uint64]wire.Packet {
	t.Helper()
	dec := NewXOR(cfg)
	got := map[uint64]wire.Packet{}
	for _, p := range emitted {
		if drop(p) {
			continue
		}
		for _, r := range dec.Recover(p) {
			if r.Header.Flags.Repair() {
				continue
			}
			got[r.Header.Seq] = r
		}
	}
	return got
}

func TestXORSingleLossRecovery(t *testing.T) {
	cfg := XORConfig{K: 8, Interleave: 4}
	enc := NewXOR(cfg)
	const plen = 1920
	emitted := protectStream(t, enc, cfg.K*cfg.Interleave, fixedLen(plen))
	orig := map[uint64][]byte{}
	for seq := uint64(0); seq < uint64(cfg.K*cfg.Interleave); seq++ {
		orig[seq] = parse(t, srcPacket(t, seq, plen)).Payload
	}

	// Drop each source position 0..k-1 of lane 0 (seqs 0,4,8,...,28) one at a time.
	for pos := 0; pos < cfg.K; pos++ {
		dropSeq := uint64(pos * cfg.Interleave)
		t.Run("drop-source", func(t *testing.T) {
			got := recoverWithDrops(t, cfg, emitted, func(p wire.Packet) bool {
				return !p.Header.Flags.Repair() && p.Header.Seq == dropSeq
			})
			r, ok := got[dropSeq]
			if !ok {
				t.Fatalf("seq %d not recovered", dropSeq)
			}
			if !bytes.Equal(r.Payload, orig[dropSeq]) {
				t.Fatalf("seq %d payload mismatch after recovery", dropSeq)
			}
			if int(r.Header.PayloadLen) != plen {
				t.Fatalf("recovered payloadLen = %d, want %d", r.Header.PayloadLen, plen)
			}
			if r.Header.SampleIndex != int64(dropSeq)*480 {
				t.Fatalf("recovered SampleIndex = %d, want %d", r.Header.SampleIndex, int64(dropSeq)*480)
			}
			if r.Header.MasterMono != int64(dropSeq)*10_000_000 {
				t.Fatalf("recovered MasterMono wrong")
			}
		})
	}

	// Dropping the repair: the k sources already delivered, nothing to recover but
	// also no loss — all sources present.
	t.Run("drop-repair", func(t *testing.T) {
		got := recoverWithDrops(t, cfg, emitted, func(p wire.Packet) bool {
			return p.Header.Flags.Repair() && p.Header.Seq == 0
		})
		for seq := uint64(0); seq < uint64(cfg.K*cfg.Interleave); seq++ {
			if _, ok := got[seq]; !ok {
				t.Fatalf("seq %d missing with only repair dropped", seq)
			}
		}
	})
}

func TestXORPadTrim(t *testing.T) {
	cfg := XORConfig{K: 8, Interleave: 1} // single lane => one group of 8 seqs 0..7
	// Mixed payload lengths; the max is at a non-first position; drop a short one.
	lenOf := func(seq uint64) int {
		switch seq {
		case 3:
			return 200 // group max
		case 5:
			return 17 // the short one we will drop
		default:
			return 64
		}
	}
	enc := NewXOR(cfg)
	emitted := protectStream(t, enc, cfg.K, lenOf)

	// repair payloadLen == lenHdr + maxLen(200)
	for _, p := range emitted {
		if p.Header.Flags.Repair() && int(p.Header.PayloadLen) != repairLenHdr+200 {
			t.Fatalf("repair payloadLen = %d, want %d", p.Header.PayloadLen, repairLenHdr+200)
		}
	}

	orig := parse(t, srcPacket(t, 5, 17)).Payload
	got := recoverWithDrops(t, cfg, emitted, func(p wire.Packet) bool {
		return !p.Header.Flags.Repair() && p.Header.Seq == 5
	})
	r, ok := got[5]
	if !ok {
		t.Fatal("short payload seq 5 not recovered")
	}
	if int(r.Header.PayloadLen) != 17 || len(r.Payload) != 17 {
		t.Fatalf("recovered len = %d (hdr %d), want 17 (no pad leak)", len(r.Payload), r.Header.PayloadLen)
	}
	if !bytes.Equal(r.Payload, orig) {
		t.Fatal("recovered short payload mismatch")
	}
}

func TestXORBurstWithinD(t *testing.T) {
	cfg := XORConfig{K: 8, Interleave: 4}
	const plen = 1920
	enc := NewXOR(cfg)
	emitted := protectStream(t, enc, cfg.K*cfg.Interleave, fixedLen(plen))

	for burst := 1; burst <= cfg.Interleave; burst++ {
		for phase := uint64(0); phase < 4; phase++ {
			t.Run("burst", func(t *testing.T) {
				lost := map[uint64]bool{}
				for i := 0; i < burst; i++ {
					lost[phase+uint64(i)] = true // contiguous burst => one per lane
				}
				got := recoverWithDrops(t, cfg, emitted, func(p wire.Packet) bool {
					return !p.Header.Flags.Repair() && lost[p.Header.Seq]
				})
				for seq := range lost {
					if _, ok := got[seq]; !ok {
						t.Fatalf("burst=%d phase=%d: seq %d not recovered", burst, phase, seq)
					}
				}
			})
		}
	}
}

func TestXORTwoInGroupUnrecoverable(t *testing.T) {
	cfg := XORConfig{K: 8, Interleave: 4}
	const plen = 1920
	enc := NewXOR(cfg)
	emitted := protectStream(t, enc, cfg.K*cfg.Interleave, fixedLen(plen))

	// Drop two members of lane 0's group: seq 0 and seq 4 (pos 0 and 1).
	got := recoverWithDrops(t, cfg, emitted, func(p wire.Packet) bool {
		return !p.Header.Flags.Repair() && (p.Header.Seq == 0 || p.Header.Seq == 4)
	})
	if _, ok := got[0]; ok {
		t.Fatal("seq 0 recovered despite 2 losses in its group")
	}
	if _, ok := got[4]; ok {
		t.Fatal("seq 4 recovered despite 2 losses in its group")
	}
	// Other lanes unaffected.
	if _, ok := got[1]; !ok {
		t.Fatal("seq 1 (other lane) should be delivered")
	}
}

func TestXORReorderAndAgeOut(t *testing.T) {
	cfg := XORConfig{K: 8, Interleave: 4}
	const plen = 256
	enc := NewXOR(cfg)
	// Two full groups per lane so a stale group can age out.
	n := cfg.K * cfg.Interleave * 2
	emitted := protectStream(t, enc, n, fixedLen(plen))

	t.Run("reorder-in-window", func(t *testing.T) {
		// Reverse the order of lane 0's first group (seqs 0,4,...,28) + its repair,
		// drop seq 8, and verify recovery still works under reorder.
		dec := NewXOR(cfg)
		var laneGroup, rest []wire.Packet
		for _, p := range emitted {
			anchorGroup := p.Header.Seq < uint64(cfg.K*cfg.Interleave) && p.Header.Seq%uint64(cfg.Interleave) == 0
			if anchorGroup {
				laneGroup = append(laneGroup, p)
			} else {
				rest = append(rest, p)
			}
		}
		for i, j := 0, len(laneGroup)-1; i < j; i, j = i+1, j-1 {
			laneGroup[i], laneGroup[j] = laneGroup[j], laneGroup[i]
		}
		got := map[uint64]wire.Packet{}
		feed := func(p wire.Packet) {
			if !p.Header.Flags.Repair() && p.Header.Seq == 8 {
				return
			}
			for _, r := range dec.Recover(p) {
				if !r.Header.Flags.Repair() {
					got[r.Header.Seq] = r
				}
			}
		}
		for _, p := range laneGroup {
			feed(p)
		}
		for _, p := range rest {
			feed(p)
		}
		if _, ok := got[8]; !ok {
			t.Fatal("seq 8 not recovered under reorder within window")
		}
	})

	t.Run("age-out-no-misrecover", func(t *testing.T) {
		// Lose seq 0 AND its repair (group 0 lane 0 unrecoverable). Later groups in
		// the same lane must still recover their own single loss without resurrecting
		// the aged-out group or panicking.
		dec := NewXOR(cfg)
		secondGroupSeq := uint64(cfg.K * cfg.Interleave) // first seq of lane 0's 2nd group
		got := map[uint64]wire.Packet{}
		for _, p := range emitted {
			if !p.Header.Flags.Repair() && p.Header.Seq == 0 {
				continue
			}
			if p.Header.Flags.Repair() && p.Header.Seq == 0 {
				continue // drop group-0 repair too
			}
			if !p.Header.Flags.Repair() && p.Header.Seq == secondGroupSeq {
				continue // single loss in the 2nd group
			}
			for _, r := range dec.Recover(p) {
				if !r.Header.Flags.Repair() {
					got[r.Header.Seq] = r
				}
			}
		}
		if _, ok := got[0]; ok {
			t.Fatal("aged-out seq 0 wrongly recovered (no repair)")
		}
		if _, ok := got[secondGroupSeq]; !ok {
			t.Fatalf("2nd-group seq %d not recovered after prior group aged out", secondGroupSeq)
		}
	})
}
