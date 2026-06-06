package sink_net

import (
	"bytes"
	"io"
	"testing"
)

// TestParseTransport: the string<->enum mapping is the README §6.5 JSON enum;
// only "tcp" selects TCP, everything else (incl. "" and garbage) is the UDP
// default.
func TestParseTransport(t *testing.T) {
	tests := []struct {
		in   string
		want Transport
	}{
		{"tcp", TransportTCP},
		{"udp", TransportUDP},
		{"", TransportUDP},
		{"TCP", TransportUDP}, // case-sensitive enum: only lowercase "tcp"
		{"garbage", TransportUDP},
	}
	for _, tc := range tests {
		if got := ParseTransport(tc.in); got != tc.want {
			t.Errorf("ParseTransport(%q)=%v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestTransportString: String round-trips to the canonical token.
func TestTransportString(t *testing.T) {
	tests := []struct {
		t    Transport
		want string
	}{
		{TransportUDP, "udp"},
		{TransportTCP, "tcp"},
		{Transport(99), "udp"}, // out-of-range renders the safe default
	}
	for _, tc := range tests {
		if got := tc.t.String(); got != tc.want {
			t.Errorf("Transport(%d).String()=%q, want %q", tc.t, got, tc.want)
		}
		// Round-trip the in-range values through ParseTransport.
		if tc.t == TransportUDP || tc.t == TransportTCP {
			if rt := ParseTransport(tc.want); rt != tc.t {
				t.Errorf("round-trip ParseTransport(%q)=%v, want %v", tc.want, rt, tc.t)
			}
		}
	}
}

// TestFrameRoundTrip: WriteFrame then frameReader.next yields back the identical
// bytes, for back-to-back packets of varying lengths.
func TestFrameRoundTrip(t *testing.T) {
	pkts := [][]byte{
		[]byte("hello"),
		{}, // zero-length frame
		bytes.Repeat([]byte{0xAB}, 1000),
		[]byte("a single packet at the canonical PCM size would be ~1964 bytes"),
	}
	var buf bytes.Buffer
	for _, p := range pkts {
		if err := WriteFrame(&buf, p); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}
	fr := newFrameReader(&buf)
	for i, want := range pkts {
		got, err := fr.next()
		if err != nil {
			t.Fatalf("frame %d next: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("frame %d = %q, want %q", i, got, want)
		}
	}
	if _, err := fr.next(); err != io.EOF {
		t.Errorf("after last frame next err=%v, want io.EOF", err)
	}
}

// splitReader yields its bytes one at a time so the deframer must reassemble both
// the length prefix and the body across many short reads (TCP boundary stress).
type splitReader struct {
	b   []byte
	pos int
}

func (s *splitReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.b) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = s.b[s.pos]
	s.pos++
	return 1, nil
}

// TestFrameSplitReads: a reader that returns one byte per Read still deframes
// correctly (io.ReadFull reassembles split prefixes and bodies, 05 §5.9).
func TestFrameSplitReads(t *testing.T) {
	var buf bytes.Buffer
	want := [][]byte{[]byte("first"), []byte("second-frame"), bytes.Repeat([]byte{1, 2, 3}, 50)}
	for _, p := range want {
		if err := WriteFrame(&buf, p); err != nil {
			t.Fatal(err)
		}
	}
	fr := newFrameReader(&splitReader{b: buf.Bytes()})
	for i, w := range want {
		got, err := fr.next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if !bytes.Equal(got, w) {
			t.Errorf("frame %d=%q want %q", i, got, w)
		}
	}
}

// TestFrameTooLarge: WriteFrame rejects an oversized packet, and a corrupt prefix
// declaring more than maxFrame is rejected by the deframer (03 §6.3 hostile input).
func TestFrameTooLarge(t *testing.T) {
	big := make([]byte, maxFrame+1)
	if err := WriteFrame(io.Discard, big); err != errFrameTooLarge {
		t.Errorf("WriteFrame oversize err=%v, want errFrameTooLarge", err)
	}
	// Hand-craft a prefix claiming a length > maxFrame.
	corrupt := []byte{0xFF, 0xFF} // 65535 > maxFrame(2048)
	fr := newFrameReader(bytes.NewReader(corrupt))
	if _, err := fr.next(); err != errFrameTooLarge {
		t.Errorf("deframe corrupt-prefix err=%v, want errFrameTooLarge", err)
	}
}
