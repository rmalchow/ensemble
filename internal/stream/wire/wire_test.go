package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// headerCases drives the round-trip / big-endian table.
var headerCases = []struct {
	name string
	h    Header
	pay  []byte
}{
	{
		name: "zero",
		h:    Header{Rate100: 480},
		pay:  []byte{},
	},
	{
		name: "pcm keyframe canonical",
		h: Header{
			Flags:       FlagKeyframe,
			CodecID:     CodecPCM,
			FECID:       FECXORParity,
			StreamGen:   0x2A3F,
			Seq:         123456,
			SampleIndex: 58982400,
			MasterMono:  0x00000C2FA1B2C3D4,
			PayloadLen:  1920,
			Rate100:     480,
		},
		pay: bytes.Repeat([]byte{0xAB}, 1920),
	},
	{
		name: "opus repair big counters",
		h: Header{
			Flags:       FlagRepair,
			CodecID:     CodecOpus,
			FECID:       FECDuplicate,
			StreamGen:   0xFFFFFFFFFFFFFFFF,
			Seq:         0x0102030405060708,
			SampleIndex: 0x7FFFFFFFFFFFFFFF,
			MasterMono:  0x7FFFFFFFFFFFFFFF,
			Rate100:     480,
		},
		pay: []byte{1, 2, 3, 4, 5},
	},
	{
		name: "negative signed fields",
		h: Header{
			CodecID:     CodecPCM,
			SampleIndex: -58982400,
			MasterMono:  -1,
			Rate100:     480,
		},
		pay: []byte{0xDE, 0xAD},
	},
	{
		name: "max payloadlen",
		h: Header{
			CodecID: CodecPCM,
			Rate100: 480,
		},
		pay: bytes.Repeat([]byte{0x7E}, maxPayload),
	},
}

func TestRoundTrip(t *testing.T) {
	for _, tc := range headerCases {
		t.Run(tc.name, func(t *testing.T) {
			buf, err := Marshal(tc.h, tc.pay)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if len(buf) != HeaderSize+len(tc.pay) {
				t.Fatalf("len=%d want %d", len(buf), HeaderSize+len(tc.pay))
			}
			got, pay, err := Unmarshal(buf)
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			// Marshal sets PayloadLen from len(payload); reflect that in want.
			want := tc.h
			want.PayloadLen = uint16(len(tc.pay))
			if got != want {
				t.Errorf("header mismatch\n got %+v\nwant %+v", got, want)
			}
			if !bytes.Equal(pay, tc.pay) {
				t.Errorf("payload mismatch len got=%d want=%d", len(pay), len(tc.pay))
			}
		})
	}
}

// goldenHeader is the exact 44 header bytes of the §5.10 worked packet.
var goldenHeader = []byte{
	'E', 'S', 'N', 'D', // off0  magic
	0x01,                                           // off4  version
	0x02,                                           // off5  flags: keyframe
	0x00,                                           // off6  codecID PCM
	0x01,                                           // off7  fecID XORParity
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x2A, 0x3F, // off8  streamGen 0x2A3F
	0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0xE2, 0x40, // off16 seq 123456
	0x00, 0x00, 0x00, 0x00, 0x03, 0x85, 0x7A, 0x00, // off24 sampleIndex 58982400
	0x00, 0x00, 0x0C, 0x2F, 0xA1, 0xB2, 0xC3, 0xD4, // off32 masterMono
	0x07, 0x80, // off40 payloadLen 1920
	0x01, 0xE0, // off42 rate/100 480
}

func TestGoldenBytes(t *testing.T) {
	// SampleIndex is the value the §5.10 diagram encodes on the wire (0x03857A00 =
	// 59079168). The diagram's prose annotation "= 58982400" disagrees with its own
	// hex bytes; the byte string is the normative wire layout, so we lock to it.
	// See the open-question note returned with this piece.
	h := Header{
		Flags:       FlagKeyframe,
		CodecID:     CodecPCM,
		FECID:       FECXORParity,
		StreamGen:   0x2A3F,
		Seq:         123456,
		SampleIndex: 0x03857A00,
		MasterMono:  0x00000C2FA1B2C3D4,
		PayloadLen:  1920,
		Rate100:     480,
	}
	dst := make([]byte, HeaderSize)
	if err := MarshalInto(dst, h); err != nil {
		t.Fatalf("MarshalInto: %v", err)
	}
	if !bytes.Equal(dst, goldenHeader) {
		t.Fatalf("golden mismatch\n got %x\nwant %x", dst, goldenHeader)
	}
}

func TestBigEndianness(t *testing.T) {
	// Distinct byte patterns so MSB-first ordering is unambiguous at each offset.
	h := Header{
		Flags:       0xA5,
		CodecID:     0x11,
		FECID:       0x22,
		StreamGen:   0x1122334455667788,
		Seq:         0x99AABBCCDDEEFF00,
		SampleIndex: 0x0102030405060708,
		MasterMono:  0x1011121314151617,
		PayloadLen:  0x0304,
		Rate100:     0x0506,
	}
	dst := make([]byte, HeaderSize)
	if err := MarshalInto(dst, h); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		off  int
		want []byte
	}{
		{offMagic, []byte{0x45, 0x53, 0x4E, 0x44}},
		{offVersion, []byte{0x01}},
		{offFlags, []byte{0xA5}},
		{offCodecID, []byte{0x11}},
		{offFECID, []byte{0x22}},
		{offStreamGen, []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}},
		{offSeq, []byte{0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x00}},
		{offSampleIndex, []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}},
		{offMasterMono, []byte{0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17}},
		{offPayloadLen, []byte{0x03, 0x04}},
		{offRate100, []byte{0x05, 0x06}},
	}
	for _, c := range checks {
		got := dst[c.off : c.off+len(c.want)]
		if !bytes.Equal(got, c.want) {
			t.Errorf("off %d: got %x want %x", c.off, got, c.want)
		}
	}
}

func TestMarshalIntoMatchesMarshal(t *testing.T) {
	h := headerCases[1].h
	pay := headerCases[1].pay
	full, err := Marshal(h, pay)
	if err != nil {
		t.Fatal(err)
	}
	// Oversize dst, pre-filled, to prove MarshalInto touches only [:HeaderSize].
	dst := bytes.Repeat([]byte{0x5A}, HeaderSize+10)
	if err := MarshalInto(dst, h); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dst[:HeaderSize], full[:HeaderSize]) {
		t.Errorf("header bytes differ\n got %x\nwant %x", dst[:HeaderSize], full[:HeaderSize])
	}
	for i := HeaderSize; i < len(dst); i++ {
		if dst[i] != 0x5A {
			t.Errorf("MarshalInto wrote past header at %d: %#x", i, dst[i])
		}
	}
}

func TestMarshalIntoShortDst(t *testing.T) {
	if err := MarshalInto(make([]byte, HeaderSize-1), Header{}); !errors.Is(err, ErrShort) {
		t.Fatalf("got %v want ErrShort", err)
	}
}

func TestPayloadLenOverflow(t *testing.T) {
	big := bytes.Repeat([]byte{0}, maxPayload+1)
	if _, err := Marshal(Header{}, big); !errors.Is(err, ErrOverflow) {
		t.Fatalf("Marshal got %v want ErrOverflow", err)
	}
}

func TestZeroAllocHotPath(t *testing.T) {
	h := headerCases[1].h
	dst := make([]byte, HeaderSize+len(headerCases[1].pay))
	copy(dst[HeaderSize:], headerCases[1].pay)

	if n := testing.AllocsPerRun(100, func() {
		_ = MarshalInto(dst, h)
	}); n != 0 {
		t.Errorf("MarshalInto allocs = %v, want 0", n)
	}

	buf, err := Marshal(h, headerCases[1].pay)
	if err != nil {
		t.Fatal(err)
	}
	if n := testing.AllocsPerRun(100, func() {
		_, _, _ = Unmarshal(buf)
	}); n != 0 {
		t.Errorf("Unmarshal allocs = %v, want 0", n)
	}
}

func TestUnmarshalValidation(t *testing.T) {
	// Build a valid 44+8 byte packet to mutate.
	base := func() []byte {
		b, _ := Marshal(Header{CodecID: CodecPCM, Rate100: 480}, []byte{1, 2, 3, 4, 5, 6, 7, 8})
		return b
	}

	tests := []struct {
		name    string
		mutate  func([]byte) []byte
		wantErr error
	}{
		{"short", func(b []byte) []byte { return b[:HeaderSize-1] }, ErrShort},
		{"empty", func([]byte) []byte { return nil }, ErrShort},
		{"bad magic", func(b []byte) []byte { b[0] = 'X'; return b }, ErrMagic},
		{"bad version", func(b []byte) []byte { b[offVersion] = 2; return b }, ErrVersion},
		{"payloadlen overflow buf", func(b []byte) []byte {
			binary.BigEndian.PutUint16(b[offPayloadLen:], 9999)
			return b
		}, ErrPayloadLen},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, pay, err := Unmarshal(tc.mutate(base()))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if h != (Header{}) {
				t.Errorf("header not zero on error: %+v", h)
			}
			if pay != nil {
				t.Errorf("payload not nil on error: %v", pay)
			}
		})
	}
}

func TestUnmarshalExactFit(t *testing.T) {
	pay := []byte{9, 8, 7}
	buf, _ := Marshal(Header{CodecID: CodecPCM}, pay)
	if len(buf) != HeaderSize+len(pay) {
		t.Fatalf("setup: len %d", len(buf))
	}
	_, got, err := Unmarshal(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, pay) {
		t.Errorf("payload %v want %v", got, pay)
	}
}

func TestUnmarshalTrailingBytesTolerated(t *testing.T) {
	pay := []byte{1, 2, 3}
	buf, _ := Marshal(Header{CodecID: CodecPCM}, pay)
	buf = append(buf, 0xFF, 0xFF, 0xFF) // UDP padding past the declared payload
	h, got, err := Unmarshal(buf)
	if err != nil {
		t.Fatal(err)
	}
	if h.PayloadLen != 3 {
		t.Fatalf("PayloadLen %d", h.PayloadLen)
	}
	if !bytes.Equal(got, pay) {
		t.Errorf("payload %v want exactly %v (no trailing)", got, pay)
	}
}

func TestUnmarshalUnknownIDsStructuralOnly(t *testing.T) {
	// Unknown codec/fec ids must NOT error: wire is codec-agnostic (§5.3).
	buf, _ := Marshal(Header{CodecID: 9, FECID: 7, Rate100: 480}, []byte{0})
	h, _, err := Unmarshal(buf)
	if err != nil {
		t.Fatalf("unexpected err for unknown ids: %v", err)
	}
	if h.CodecID != 9 || h.FECID != 7 {
		t.Fatalf("ids not preserved: codec=%d fec=%d", h.CodecID, h.FECID)
	}
	if _, ok := CodecName(h.CodecID); ok {
		t.Errorf("CodecName(9) should be unknown")
	}
	if _, ok := FECName(h.FECID); ok {
		t.Errorf("FECName(7) should be unknown")
	}
}

func FuzzUnmarshal(f *testing.F) {
	for _, tc := range headerCases {
		if b, err := Marshal(tc.h, tc.pay); err == nil {
			f.Add(b)
		}
	}
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte{0}, 44))
	f.Fuzz(func(t *testing.T, buf []byte) {
		h, pay, err := Unmarshal(buf)
		if err != nil {
			return // must not panic; error cases carry no invariants
		}
		// On success: declared payload fits within buf and matches the subslice.
		if HeaderSize+int(h.PayloadLen) > len(buf) {
			t.Fatalf("invariant: %d+%d > %d", HeaderSize, h.PayloadLen, len(buf))
		}
		if len(pay) != int(h.PayloadLen) {
			t.Fatalf("payload len %d != PayloadLen %d", len(pay), h.PayloadLen)
		}
	})
}
