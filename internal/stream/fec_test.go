package stream

import (
	"bytes"
	"testing"
)

func payload(b byte, n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = b
	}
	return p
}

func xorAll(pays ...[]byte) []byte {
	mx := 0
	for _, p := range pays {
		if len(p) > mx {
			mx = len(p)
		}
	}
	out := make([]byte, mx)
	for _, p := range pays {
		XORInto(out, p)
	}
	return out
}

func TestFECBlockParityXOR(t *testing.T) {
	var b fecBlock
	b.reset(7)
	p0, p1, p2, p3 := payload(0x11, 8), payload(0x22, 8), payload(0x33, 8), payload(0x44, 8)
	b.fold(7, 0, p0)
	b.fold(7, 1, p1)
	b.fold(7, 2, p2)
	b.fold(7, 3, p3)
	if !b.ready() {
		t.Fatal("block should be ready after 4 frames")
	}
	pkt := b.parityPacket(nil)
	h, par, err := DecodeFrame(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if h.Type != TypeFEC || h.Gen != 7 || h.Seq != 0 {
		t.Fatalf("bad parity header: %+v", h)
	}
	want := xorAll(p0, p1, p2, p3)
	if !bytes.Equal(par, want) {
		t.Fatalf("parity mismatch")
	}
}

func TestFECRecoverMissingFromParity(t *testing.T) {
	var b fecBlock
	b.reset(1)
	pays := [][]byte{payload(0xA0, 16), payload(0xB1, 16), payload(0xC2, 16), payload(0xD3, 16)}
	ptsBase := int64(1000)
	for i, p := range pays {
		b.fold(1, uint64(i), p)
	}
	pkt := b.parityPacket(nil)
	_, par, _ := DecodeFrame(pkt)

	w := newRecoveryWindow(1)
	// Deliver data 0,1,3 (missing 2); then parity -> recover 2.
	for _, i := range []int{0, 1, 3} {
		_, _, _, ok := w.observeData(1, uint64(i), ptsBase+int64(i)*FrameNanos, pays[i])
		if ok {
			t.Fatal("should not recover before parity")
		}
	}
	rseq, rpts, rpay, ok := w.observeParity(1, 0, par)
	if !ok {
		t.Fatal("expected recovery")
	}
	if rseq != 2 {
		t.Fatalf("recovered seq=%d want 2", rseq)
	}
	if !bytes.Equal(rpay, pays[2]) {
		t.Fatalf("recovered payload mismatch")
	}
	if rpts != ptsBase+2*FrameNanos {
		t.Fatalf("recovered pts=%d want %d", rpts, ptsBase+2*FrameNanos)
	}
}

func TestFECRecoverWhenDataArrivesLast(t *testing.T) {
	var b fecBlock
	b.reset(1)
	pays := [][]byte{payload(1, 8), payload(2, 8), payload(3, 8), payload(4, 8)}
	for i, p := range pays {
		b.fold(1, uint64(i), p)
	}
	_, par, _ := DecodeFrame(b.parityPacket(nil))

	w := newRecoveryWindow(1)
	w.observeData(1, 0, 0, pays[0])
	w.observeData(1, 1, FrameNanos, pays[1])
	w.observeParity(1, 0, par) // parity arrives, still missing 2 & 3
	// deliver 3, now only 2 missing -> recover on this data arrival
	rseq, _, rpay, ok := w.observeData(1, 3, 3*FrameNanos, pays[3])
	if !ok || rseq != 2 || !bytes.Equal(rpay, pays[2]) {
		t.Fatalf("expected recovery of seq 2, got ok=%v seq=%d", ok, rseq)
	}
}

func TestFECDoubleLossUnrecoverable(t *testing.T) {
	var b fecBlock
	b.reset(1)
	pays := [][]byte{payload(1, 8), payload(2, 8), payload(3, 8), payload(4, 8)}
	for i, p := range pays {
		b.fold(1, uint64(i), p)
	}
	_, par, _ := DecodeFrame(b.parityPacket(nil))

	w := newRecoveryWindow(1)
	w.observeData(1, 0, 0, pays[0])
	w.observeData(1, 1, FrameNanos, pays[1])
	_, _, _, ok := w.observeParity(1, 0, par)
	if ok {
		t.Fatal("double loss must not recover")
	}
}

func TestFECShortPayloadPadding(t *testing.T) {
	var b fecBlock
	b.reset(1)
	pays := [][]byte{payload(0xAA, 4), payload(0xBB, 10), payload(0xCC, 7), payload(0xDD, 10)}
	for i, p := range pays {
		b.fold(1, uint64(i), p)
	}
	_, par, _ := DecodeFrame(b.parityPacket(nil))

	w := newRecoveryWindow(1)
	w.observeData(1, 0, 0, pays[0])
	w.observeData(1, 1, FrameNanos, pays[1])
	w.observeData(1, 3, 3*FrameNanos, pays[3])
	_, _, rpay, ok := w.observeParity(1, 0, par)
	if !ok {
		t.Fatal("expected recovery")
	}
	// Recovered payload is padded to parity length (10); the real payload[2]
	// is 7 bytes followed by zero padding.
	if len(rpay) < len(pays[2]) {
		t.Fatalf("recovered too short: %d", len(rpay))
	}
	if !bytes.Equal(rpay[:len(pays[2])], pays[2]) {
		t.Fatalf("recovered prefix mismatch")
	}
	for _, x := range rpay[len(pays[2]):] {
		if x != 0 {
			t.Fatal("padding should be zero")
		}
	}
}

func TestFECPartialFlush(t *testing.T) {
	var b fecBlock
	b.reset(2)
	p0, p1 := payload(0x11, 8), payload(0x22, 8)
	b.fold(2, 10, p0)
	b.fold(2, 11, p1)
	pkt := b.flushPartial(nil)
	h, par, err := DecodeFrame(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if h.Seq != 10 || h.Gen != 2 {
		t.Fatalf("bad tail parity header %+v", h)
	}
	if !bytes.Equal(par, xorAll(p0, p1)) {
		t.Fatal("tail parity mismatch")
	}
}

func TestFECResetOnGen(t *testing.T) {
	w := newRecoveryWindow(1)
	w.observeData(1, 0, 0, payload(1, 8))
	w.reset(2)
	if len(w.blocks) != 0 {
		t.Fatal("reset should drop blocks")
	}
	// old-gen data is ignored
	_, _, _, ok := w.observeData(1, 1, 0, payload(2, 8))
	if ok || len(w.blocks) != 0 {
		t.Fatal("old-gen data must be ignored")
	}
}
