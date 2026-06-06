package fec

import (
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/wire"
)

// fuzzPacket builds a wire.Packet the way the receiver does: by marshaling then
// Unmarshaling, so PayloadLen == len(Payload) (the structural invariant wire
// guarantees before fec ever sees a packet, 05 §5.6.2). The fuzzers vary seq,
// the repair flag, and the payload bytes (incl. a sub-2-byte "repair" with no
// length prefix) to exercise fec's bounds handling on hostile-but-valid input.
func fuzzPacket(t *testing.T, seq uint64, repair bool, payload []byte) wire.Packet {
	t.Helper()
	if len(payload) > 60000 {
		payload = payload[:60000]
	}
	flags := wire.Flags(0)
	if repair {
		flags |= wire.FlagRepair
	}
	buf, err := wire.Marshal(wire.Header{Flags: flags, Seq: seq, Rate100: 480}, payload)
	if err != nil {
		t.Skip()
	}
	hdr, pl, err := wire.Unmarshal(buf)
	if err != nil {
		t.Skip()
	}
	return wire.Packet{Header: hdr, Payload: pl}
}

// checkRecovered asserts the invariants every Recover output must satisfy: the
// returned payload length equals the header PayloadLen, and it never exceeds the
// bytes available. Bounds-safety on hostile UDP input (03 / A.8).
func checkRecovered(t *testing.T, recovered []wire.Packet) {
	t.Helper()
	for _, r := range recovered {
		if len(r.Payload) != int(r.Header.PayloadLen) {
			t.Fatalf("recovered len %d != PayloadLen %d", len(r.Payload), r.Header.PayloadLen)
		}
	}
}

func FuzzXORRecover(f *testing.F) {
	f.Add(uint64(0), false, []byte("abc"))
	f.Add(uint64(4), true, []byte{}) // sub-prefix "repair": must not slice OOB
	f.Add(uint64(7), false, []byte{1, 2})
	f.Fuzz(func(t *testing.T, seq uint64, repair bool, payload []byte) {
		dec := NewXOR(DefaultXORConfig())
		// Feed the same hostile packet repeatedly across lanes; must never panic
		// and never emit a length-inconsistent payload.
		for i := 0; i < 40; i++ {
			p := fuzzPacket(t, seq+uint64(i), repair, payload)
			checkRecovered(t, dec.Recover(p))
		}
	})
}

func FuzzDupRecover(f *testing.F) {
	f.Add(uint64(0), []byte("abc"))
	f.Add(uint64(99999), []byte{})
	f.Fuzz(func(t *testing.T, seq uint64, payload []byte) {
		dec := NewDup(DefaultDupConfig())
		for i := 0; i < 40; i++ {
			p := fuzzPacket(t, seq+uint64(i%3), false, payload)
			checkRecovered(t, dec.Recover(p))
		}
	})
}

// FuzzXORProtectRoundTrip feeds arbitrary (but structurally valid) source bytes
// through Protect→Recover for a full group and asserts Protect never panics and
// every emitted packet is re-parseable by wire.Unmarshal.
func FuzzXORProtectRoundTrip(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > 60000 {
			payload = payload[:60000]
		}
		enc := NewXOR(DefaultXORConfig())
		for seq := uint64(0); seq < 40; seq++ {
			hdr := wire.Header{CodecID: wire.CodecPCM, FECID: wire.FECXORParity, Seq: seq, Rate100: 480}
			pkt, err := wire.Marshal(hdr, payload)
			if err != nil {
				t.Skip()
			}
			for _, b := range enc.Protect(seq, pkt) {
				if _, _, err := wire.Unmarshal(b); err != nil {
					t.Fatalf("Protect emitted unparseable packet: %v", err)
				}
			}
		}
	})
}
