package wire

import "testing"

func TestCodecNameID(t *testing.T) {
	tests := []struct {
		id   CodecID
		name string
	}{
		{CodecPCM, "pcm"},
		{CodecOpus, "opus"},
	}
	for _, tc := range tests {
		got, ok := CodecName(tc.id)
		if !ok || got != tc.name {
			t.Errorf("CodecName(%d) = (%q,%v) want (%q,true)", tc.id, got, ok, tc.name)
		}
		id, ok := ParseCodec(tc.name)
		if !ok || id != tc.id {
			t.Errorf("ParseCodec(%q) = (%d,%v) want (%d,true)", tc.name, id, ok, tc.id)
		}
	}
}

func TestCodecUnknown(t *testing.T) {
	if name, ok := CodecName(9); ok || name != "" {
		t.Errorf("CodecName(9) = (%q,%v) want (\"\",false)", name, ok)
	}
	// FLAC is decoded to PCM at the source; it is NOT a wire codec (R2/05 §5.4).
	if id, ok := ParseCodec("flac"); ok || id != 0 {
		t.Errorf("ParseCodec(flac) = (%d,%v) want (0,false)", id, ok)
	}
	if id, ok := ParseCodec(""); ok || id != 0 {
		t.Errorf("ParseCodec(\"\") = (%d,%v) want (0,false)", id, ok)
	}
}

func TestFECNameID(t *testing.T) {
	tests := []struct {
		id   FECID
		name string
	}{
		{FECNone, "none"},
		{FECXORParity, "xorParity"},
		{FECDuplicate, "duplicate"},
	}
	for _, tc := range tests {
		got, ok := FECName(tc.id)
		if !ok || got != tc.name {
			t.Errorf("FECName(%d) = (%q,%v) want (%q,true)", tc.id, got, ok, tc.name)
		}
		id, ok := ParseFEC(tc.name)
		if !ok || id != tc.id {
			t.Errorf("ParseFEC(%q) = (%d,%v) want (%d,true)", tc.name, id, ok, tc.id)
		}
	}
}

func TestFECUnknown(t *testing.T) {
	if name, ok := FECName(7); ok || name != "" {
		t.Errorf("FECName(7) = (%q,%v) want (\"\",false)", name, ok)
	}
	// Reed-Solomon is excluded by D4.
	if id, ok := ParseFEC("reedSolomon"); ok || id != 0 {
		t.Errorf("ParseFEC(reedSolomon) = (%d,%v) want (0,false)", id, ok)
	}
}

// TestRegistryTotality: every defined const has a name and parses back to itself.
func TestRegistryTotality(t *testing.T) {
	for _, id := range []CodecID{CodecPCM, CodecOpus} {
		name, ok := CodecName(id)
		if !ok {
			t.Fatalf("CodecName(%d) unknown", id)
		}
		back, ok := ParseCodec(name)
		if !ok || back != id {
			t.Errorf("codec round-trip %d -> %q -> (%d,%v)", id, name, back, ok)
		}
	}
	for _, id := range []FECID{FECNone, FECXORParity, FECDuplicate} {
		name, ok := FECName(id)
		if !ok {
			t.Fatalf("FECName(%d) unknown", id)
		}
		back, ok := ParseFEC(name)
		if !ok || back != id {
			t.Errorf("fec round-trip %d -> %q -> (%d,%v)", id, name, back, ok)
		}
	}
}
