package stream

import (
	"net/netip"
	"testing"
)

func TestAttachRoundTrip(t *testing.T) {
	cases := []AttachPayload{
		{
			Source:    netip.MustParseAddrPort("10.0.0.5:9200"),
			Clock:     netip.MustParseAddrPort("10.0.0.5:9090"),
			Codec:     CodecOpus,
			Transport: TransportUDP,
			BufferMs:  150,
		},
		{
			Source:    netip.MustParseAddrPort("192.168.1.42:50000"),
			Clock:     netip.MustParseAddrPort("192.168.1.43:65535"),
			Codec:     CodecPCM,
			Transport: TransportTCP,
			BufferMs:  500,
		},
		{
			// unset endpoints encode as 0.0.0.0:0 (the always-4-byte wire) and
			// decode back as such — the receiver treats 0.0.0.0 as "unset" (§6.1).
			Source: netip.MustParseAddrPort("0.0.0.0:0"),
			Clock:  netip.MustParseAddrPort("0.0.0.0:0"),
		},
	}
	for i, a := range cases {
		buf := a.AppendTo(nil)
		if len(buf) != AttachLen {
			t.Fatalf("case %d: AppendTo len = %d, want %d", i, len(buf), AttachLen)
		}
		got, err := DecodeAttach(buf)
		if err != nil {
			t.Fatalf("case %d: DecodeAttach: %v", i, err)
		}
		// netip.AddrPort from 4-byte addrs compares equal field-by-field.
		if got.Source != a.Source || got.Clock != a.Clock ||
			got.Codec != a.Codec || got.Transport != a.Transport || got.BufferMs != a.BufferMs {
			t.Fatalf("case %d: round-trip got %+v want %+v", i, got, a)
		}
	}
}

func TestAttachIPv4In6Normalizes(t *testing.T) {
	// A v4-in-v6 endpoint must encode to its 4-byte form and decode back as IPv4.
	a := AttachPayload{
		Source: netip.MustParseAddrPort("[::ffff:10.0.0.9]:9200"),
		Clock:  netip.MustParseAddrPort("10.0.0.9:9090"),
	}
	got, err := DecodeAttach(a.AppendTo(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.Source.Addr() != netip.MustParseAddr("10.0.0.9") || got.Source.Port() != 9200 {
		t.Fatalf("v4-in-v6 source not normalized: %v", got.Source)
	}
}

func TestSetVolRoundTrip(t *testing.T) {
	for _, c := range []SetVolPayload{{0, false}, {100, true}, {73, false}, {50, true}} {
		buf := c.AppendTo(nil)
		if len(buf) != SetVolLen {
			t.Fatalf("AppendTo len = %d, want %d", len(buf), SetVolLen)
		}
		got, err := DecodeSetVol(buf)
		if err != nil {
			t.Fatalf("DecodeSetVol: %v", err)
		}
		if got != c {
			t.Fatalf("round-trip got %+v want %+v", got, c)
		}
	}
}

func TestSetVolClampsAndIgnoresHighFlagBits(t *testing.T) {
	got, err := DecodeSetVol([]byte{200, 0xFE}) // pct>100, mute bit clear
	if err != nil {
		t.Fatal(err)
	}
	if got.VolumePct != 100 {
		t.Fatalf("VolumePct = %d, want clamp to 100", got.VolumePct)
	}
	if got.Mute {
		t.Fatal("Mute should be false when bit0 clear")
	}
}

func TestSetDelayRoundTripSigned(t *testing.T) {
	for _, ms := range []int16{0, 1, -1, 500, -500, 32767, -32768} {
		c := SetDelayPayload{DelayMs: ms}
		got, err := DecodeSetDelay(c.AppendTo(nil))
		if err != nil {
			t.Fatalf("DecodeSetDelay(%d): %v", ms, err)
		}
		if got.DelayMs != ms {
			t.Fatalf("round-trip got %d want %d", got.DelayMs, ms)
		}
	}
}

func TestSetCapRoundTrip(t *testing.T) {
	for _, c := range []SetCapPayload{{0, true}, {3, false}, {255, true}} {
		got, err := DecodeSetCap(c.AppendTo(nil))
		if err != nil {
			t.Fatalf("DecodeSetCap: %v", err)
		}
		if got != c {
			t.Fatalf("round-trip got %+v want %+v", got, c)
		}
	}
}

func TestSetEqualizeRoundTrip(t *testing.T) {
	for _, ms := range []uint16{0, 1, 70, 250, 500, 65535} {
		e := SetEqualizePayload{DelayMs: ms}
		got, err := DecodeSetEqualize(e.AppendTo(nil))
		if err != nil {
			t.Fatalf("DecodeSetEqualize(%d): %v", ms, err)
		}
		if got.DelayMs != ms {
			t.Fatalf("round-trip got %d want %d", got.DelayMs, ms)
		}
	}
}

func TestStatusRoundTrip(t *testing.T) {
	s := StatusPayload{
		NodeID:          [16]byte{0x4e, 0xd7, 0x95, 0xd4, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
		Synced:          true,
		Playing:         true,
		Calibrated:      true,
		Buffered:        9,
		LastSeq:         1 << 40,
		OffsetNs:        -123456789,
		RTTNs:           420000,
		RatePPMx1000:    -31250, // -31.25 ppm
		Played:          100000,
		Silence:         42,
		Late:            7,
		DeviceDelayNs:   185_000_000, // 185 ms (D63 telemetry)
		PhaseErrNs:      -2_500_000,  // -2.5 ms (D64 telemetry)
		SamplesInjected: 1_234,       // grounded resample counts
		SamplesDropped:  5_678,
	}
	buf := s.AppendTo(nil)
	if len(buf) != StatusLen {
		t.Fatalf("AppendTo len = %d, want %d", len(buf), StatusLen)
	}
	got, err := DecodeStatus(buf)
	if err != nil {
		t.Fatalf("DecodeStatus: %v", err)
	}
	if got != s {
		t.Fatalf("round-trip got %+v want %+v", got, s)
	}
}

func TestControlDecodeShortErrors(t *testing.T) {
	if _, err := DecodeAttach(make([]byte, AttachLen-1)); err == nil {
		t.Fatal("DecodeAttach short: want error")
	}
	if _, err := DecodeSetVol(make([]byte, 1)); err == nil {
		t.Fatal("DecodeSetVol short: want error")
	}
	if _, err := DecodeSetDelay(make([]byte, 1)); err == nil {
		t.Fatal("DecodeSetDelay short: want error")
	}
	if _, err := DecodeSetCap(make([]byte, 1)); err == nil {
		t.Fatal("DecodeSetCap short: want error")
	}
	if _, err := DecodeSetEqualize(make([]byte, 1)); err == nil {
		t.Fatal("DecodeSetEqualize short: want error")
	}
	if _, err := DecodeStatus(make([]byte, StatusLen-1)); err == nil {
		t.Fatal("DecodeStatus short: want error")
	}
}

func TestCodecStringParse(t *testing.T) {
	if CodecPCM.String() != "pcm" || CodecOpus.String() != "opus" {
		t.Fatal("Codec.String mismatch")
	}
	if ParseCodec("opus") != CodecOpus || ParseCodec("pcm") != CodecPCM || ParseCodec("flac") != CodecPCM {
		t.Fatal("ParseCodec mismatch")
	}
}

// The new control types must be distinct from each other and from the data-plane
// types, so a single demux switch never aliases.
func TestControlTypesDistinct(t *testing.T) {
	all := []byte{
		TypeAudio, TypeFEC, TypeClockReq, TypeClockRsp,
		TypeHello, TypeBye, TypeRestart, TypeReconfig,
		TypeAttach, TypeDetach, TypeSetVol, TypeSetDelay, TypeSetCap, TypeSetEq, TypeStatus,
	}
	seen := map[byte]bool{}
	for _, ty := range all {
		if seen[ty] {
			t.Fatalf("duplicate packet type 0x%02x", ty)
		}
		seen[ty] = true
	}
}
