package wire

import (
	"bytes"
	"testing"
)

func TestPacketFlagAccessors(t *testing.T) {
	tests := []struct {
		flags        Flags
		wantRepair   bool
		wantKeyframe bool
	}{
		{0, false, false},
		{FlagRepair, true, false},
		{FlagKeyframe, false, true},
		{FlagRepair | FlagKeyframe, true, true},
	}
	for _, tc := range tests {
		p := Packet{Header: Header{Flags: tc.flags}}
		if p.Repair() != tc.wantRepair {
			t.Errorf("flags %#x Repair()=%v want %v", tc.flags, p.Repair(), tc.wantRepair)
		}
		if p.Keyframe() != tc.wantKeyframe {
			t.Errorf("flags %#x Keyframe()=%v want %v", tc.flags, p.Keyframe(), tc.wantKeyframe)
		}
	}
}

func TestPacketCloneDeepCopy(t *testing.T) {
	src := Packet{
		Header:  Header{CodecID: CodecPCM, Seq: 7, PayloadLen: 4},
		Payload: []byte{1, 2, 3, 4},
	}
	clone := src.Clone()
	if !bytes.Equal(clone.Payload, src.Payload) {
		t.Fatalf("clone payload %v != src %v", clone.Payload, src.Payload)
	}
	// Mutating the source payload must not touch the clone (deep copy).
	src.Payload[0] = 0xFF
	if clone.Payload[0] == 0xFF {
		t.Errorf("clone aliases source payload")
	}
	if clone.Header != (Packet{Header: Header{CodecID: CodecPCM, Seq: 7, PayloadLen: 4}}).Header {
		t.Errorf("clone header changed: %+v", clone.Header)
	}
}

func TestPacketCloneNilPayload(t *testing.T) {
	c := Packet{Header: Header{Seq: 1}}.Clone()
	if c.Payload != nil {
		t.Errorf("nil payload became %v", c.Payload)
	}
}

func TestPacketMarshalSymmetry(t *testing.T) {
	p := Packet{
		Header: Header{
			Flags:       FlagKeyframe,
			CodecID:     CodecOpus,
			FECID:       FECNone,
			StreamGen:   42,
			Seq:         99,
			SampleIndex: 123456789,
			MasterMono:  987654321,
			Rate100:     480,
		},
		Payload: []byte{0xCA, 0xFE, 0xBA, 0xBE},
	}
	buf, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	h, pay, err := Unmarshal(buf)
	if err != nil {
		t.Fatal(err)
	}
	want := p.Header
	want.PayloadLen = uint16(len(p.Payload))
	if h != want {
		t.Errorf("header\n got %+v\nwant %+v", h, want)
	}
	if !bytes.Equal(pay, p.Payload) {
		t.Errorf("payload %v want %v", pay, p.Payload)
	}
}
