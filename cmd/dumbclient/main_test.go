package main

import "testing"

// TestParseHeaderRoundTrip verifies the 24-byte big-endian header codec matches
// encodeFrame (the canonical wire layout).
func TestParseHeaderRoundTrip(t *testing.T) {
	payload := []byte("hello-payload")
	pkt := encodeFrame(typeAudio, 0x01020304, 0x1122334455667788, -1234567890, payload)
	if len(pkt) != headerSize+len(payload) {
		t.Fatalf("len=%d want %d", len(pkt), headerSize+len(payload))
	}
	h, ok := parseHeader(pkt)
	if !ok {
		t.Fatal("parseHeader returned ok=false")
	}
	if h.magic != magic {
		t.Errorf("magic=%#x want %#x", h.magic, magic)
	}
	if h.typ != typeAudio {
		t.Errorf("type=%#x want %#x", h.typ, typeAudio)
	}
	if h.gen != 0x01020304 {
		t.Errorf("gen=%#x", h.gen)
	}
	if h.seq != 0x1122334455667788 {
		t.Errorf("seq=%#x", h.seq)
	}
	if h.pts != -1234567890 {
		t.Errorf("pts=%d want -1234567890", h.pts)
	}
	if int(h.payloadLen) != len(payload) {
		t.Errorf("payloadLen=%d want %d", h.payloadLen, len(payload))
	}
}

func TestParseHeaderShort(t *testing.T) {
	if _, ok := parseHeader(make([]byte, headerSize-1)); ok {
		t.Fatal("parseHeader accepted a short buffer")
	}
}

// TestComputeSample checks the NTP offset/rtt formulas against a hand-worked
// example: a master 1000 ns ahead of local, with a symmetric 200 ns RTT.
func TestComputeSample(t *testing.T) {
	// local sends t1=0, arrives at master at master-time t2; master replies at
	// t3; local receives at t4. With offset +1000 and one-way delay 100:
	//   t1=0 (local) -> t2 = 0 + 100 + 1000 = 1100 (master)
	//   t3 = 1100 (master, instant turnaround)
	//   t4 = (1100 - 1000) + 100 = 200 (local)
	s := computeSample(0, 1100, 1100, 200)
	if s.offset != 1000 {
		t.Errorf("offset=%d want 1000", s.offset)
	}
	if s.rtt != 200 {
		t.Errorf("rtt=%d want 200", s.rtt)
	}
}

// TestMedianOffset checks best-RTT selection + median, and the window bound.
func TestMedianOffset(t *testing.T) {
	if _, ok := medianOffset(nil, clockWindow, clockBest); ok {
		t.Fatal("empty samples reported ok=true")
	}

	// Five samples: the best-RTT 3 (rtt 1,2,3) have offsets 100, 50, 150 ->
	// median 100. The two high-RTT outliers (offset 9000) must be excluded.
	ss := []sample{
		{offset: 100, rtt: 1},
		{offset: 9000, rtt: 100},
		{offset: 50, rtt: 2},
		{offset: 9000, rtt: 200},
		{offset: 150, rtt: 3},
	}
	off, ok := medianOffset(ss, clockWindow, 3)
	if !ok {
		t.Fatal("ok=false")
	}
	if off != 100 {
		t.Errorf("median offset=%d want 100", off)
	}

	// Window bound: only the last `window` samples are considered. Fill the
	// window with rtt=1 offset=7, then the latest sample (offset=7) dominates
	// once the early low-rtt sample is evicted.
	var many []sample
	many = append(many, sample{offset: 999, rtt: 0}) // would-be best, but evicted
	for i := 0; i < clockWindow; i++ {
		many = append(many, sample{offset: 7, rtt: 1})
	}
	off, ok = medianOffset(many, clockWindow, clockBest)
	if !ok || off != 7 {
		t.Errorf("windowed median=%d ok=%v want 7", off, ok)
	}
}
