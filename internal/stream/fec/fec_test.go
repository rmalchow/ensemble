package fec

import (
	"errors"
	"testing"
	"unsafe"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/wire"
)

func TestNoneID(t *testing.T) {
	if got := NewNone().ID(); got != None {
		t.Fatalf("ID() = %d, want %d (None)", got, None)
	}
	if None != 0 {
		t.Fatalf("None = %d, want 0", None)
	}
}

func TestRegistry(t *testing.T) {
	tests := []struct {
		name string
		id   FECID
	}{
		{"none", None},
		{"xorParity", XORParity}, // exact camelCase
		{"duplicate", Duplicate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := FromName(tt.name)
			if !ok || id != tt.id {
				t.Fatalf("FromName(%q) = %d,%v; want %d,true", tt.name, id, ok, tt.id)
			}
			n, ok := NameOf(tt.id)
			if !ok || n != tt.name {
				t.Fatalf("NameOf(%d) = %q,%v; want %q,true", tt.id, n, ok, tt.name)
			}
		})
	}
	if _, ok := FromName("xorparity"); ok {
		t.Fatal("FromName lowercased 'xorparity' should not match camelCase name")
	}
	if _, ok := FromName("reed-solomon"); ok {
		t.Fatal("FromName(unknown) returned ok=true")
	}
	if _, ok := NameOf(99); ok {
		t.Fatal("NameOf(unknown id) returned ok=true")
	}
}

func TestNewGating(t *testing.T) {
	// P5.1: None, XORParity and Duplicate are all constructible; only an unknown
	// id errors. Each constructed scheme reports its own id.
	for _, id := range []FECID{None, XORParity, Duplicate} {
		f, err := New(id)
		if err != nil {
			t.Fatalf("New(%d) error: %v", id, err)
		}
		if f.ID() != id {
			t.Fatalf("New(%d).ID() = %d, want %d", id, f.ID(), id)
		}
	}
	if _, err := New(99); !errors.Is(err, ErrUnsupportedFEC) {
		t.Fatalf("New(99) err = %v, want ErrUnsupportedFEC", err)
	}
}

func TestDefaultConfigsAreA12(t *testing.T) {
	if c := DefaultXORConfig(); c.K != 8 || c.Interleave != 4 {
		t.Fatalf("DefaultXORConfig = %+v, want {K:8 Interleave:4} (A.12)", c)
	}
	if c := DefaultDupConfig(); c.Offset != 5 {
		t.Fatalf("DefaultDupConfig = %+v, want {Offset:5} (A.12)", c)
	}
}

func TestProtectIdentity(t *testing.T) {
	f := NewNone()
	pkt := []byte{1, 2, 3, 4}
	out := f.Protect(7, pkt)
	if len(out) != 1 {
		t.Fatalf("Protect returned %d packets, want 1", len(out))
	}
	if out[0] == nil {
		t.Fatal("Protect returned nil element")
	}
	// Must be the SAME backing array (no copy, zero-cost passthrough).
	if len(out[0]) != len(pkt) || unsafe.SliceData(out[0]) != unsafe.SliceData(pkt) {
		t.Fatal("Protect copied pkt; want the same backing array")
	}
}

func TestRecoverIdentity(t *testing.T) {
	f := NewNone()
	p := wire.Packet{
		Header: wire.Header{
			Flags:       wire.FlagKeyframe,
			CodecID:     wire.CodecPCM,
			FECID:       wire.FECNone,
			StreamGen:   3,
			Seq:         42,
			SampleIndex: 480,
			MasterMono:  1_000_000,
			PayloadLen:  1920,
			Rate100:     480,
		},
		Payload: []byte{9, 8, 7},
	}
	out := f.Recover(p)
	if len(out) != 1 {
		t.Fatalf("Recover returned %d packets, want 1", len(out))
	}
	if out[0].Header != p.Header {
		t.Fatalf("Recover header = %+v, want %+v", out[0].Header, p.Header)
	}
	if string(out[0].Payload) != string(p.Payload) {
		t.Fatal("Recover payload differs")
	}
}

func TestFECAllocs(t *testing.T) {
	f := NewNone()
	pkt := []byte{1, 2, 3, 4}
	if n := testing.AllocsPerRun(100, func() { _ = f.Protect(1, pkt) }); n > 1 {
		t.Fatalf("Protect: %v allocs/op, want <=1", n)
	}
	p := wire.Packet{Payload: pkt}
	if n := testing.AllocsPerRun(100, func() { _ = f.Recover(p) }); n > 1 {
		t.Fatalf("Recover: %v allocs/op, want <=1", n)
	}
}
