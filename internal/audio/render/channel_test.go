package render

import (
	"math"
	"testing"
)

// TestSelectChannelRoles covers the doc 06 §5.1 channel-role fan-out table for
// the canonical stereo source frame on a stereo sink (outCh=2).
func TestSelectChannelRoles(t *testing.T) {
	const L, R = float32(0.5), float32(-0.5)
	src := []float32{L, R}

	tests := []struct {
		name     string
		role     Channel
		outCh    int
		wantPerO []float32 // expected output sample per output channel oc
	}{
		{"stereo passthrough", ChannelStereo, 2, []float32{L, R}},
		{"left fan-out", ChannelLeft, 2, []float32{L, L}},
		{"right fan-out", ChannelRight, 2, []float32{R, R}},
		{"stereo mono sink", ChannelStereo, 1, []float32{L}},
		{"left mono sink", ChannelLeft, 1, []float32{L}},
		{"right mono sink", ChannelRight, 1, []float32{R}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for oc := 0; oc < tc.outCh; oc++ {
				got := SelectChannel(src, tc.role, tc.outCh, oc)
				if got != tc.wantPerO[oc] {
					t.Fatalf("SelectChannel(role=%s, outCh=%d, oc=%d)=%v, want %v",
						tc.role, tc.outCh, oc, got, tc.wantPerO[oc])
				}
			}
		})
	}
}

// TestSelectChannelStereoPairImage is the "stable stereo image" proof (A.13 P6):
// a left-node and a right-node, fed the SAME source frame, each fan their chosen
// source channel to both of their output pins, and neither leaks the other's
// channel.
func TestSelectChannelStereoPairImage(t *testing.T) {
	const L, R = float32(0.7), float32(-0.3)
	src := []float32{L, R}
	const outCh = 2

	var leftOut, rightOut [outCh]float32
	for oc := 0; oc < outCh; oc++ {
		leftOut[oc] = SelectChannel(src, ChannelLeft, outCh, oc)
		rightOut[oc] = SelectChannel(src, ChannelRight, outCh, oc)
	}

	// Left node: both pins == L, never R.
	for oc, v := range leftOut {
		if v != L {
			t.Fatalf("left node oc%d=%v, want L=%v", oc, v, L)
		}
		if v == R {
			t.Fatalf("left node oc%d leaked R=%v", oc, R)
		}
	}
	// Right node: both pins == R, never L.
	for oc, v := range rightOut {
		if v != R {
			t.Fatalf("right node oc%d=%v, want R=%v", oc, v, R)
		}
		if v == L {
			t.Fatalf("right node oc%d leaked L=%v", oc, L)
		}
	}
}

// TestSelectChannelBounds covers the bounds guards: a mono source under
// ChannelRight has no ch1 (returns 0, no panic); an empty frame returns 0.
func TestSelectChannelBounds(t *testing.T) {
	mono := []float32{0.9}
	if v := SelectChannel(mono, ChannelRight, 2, 0); v != 0 {
		t.Fatalf("mono+right oc0=%v, want 0 (no ch1)", v)
	}
	if v := SelectChannel(mono, ChannelRight, 2, 1); v != 0 {
		t.Fatalf("mono+right oc1=%v, want 0 (no ch1)", v)
	}
	// mono source, stereo role, outCh=2: clamp to the last available channel.
	if v := SelectChannel(mono, ChannelStereo, 2, 1); v != 0.9 {
		t.Fatalf("mono+stereo oc1=%v, want 0.9 (clamp to ch0)", v)
	}
	// mono source under ChannelLeft fans ch0.
	if v := SelectChannel(mono, ChannelLeft, 2, 1); v != 0.9 {
		t.Fatalf("mono+left oc1=%v, want 0.9", v)
	}
	// empty frame: every role returns 0, no panic.
	for _, role := range []Channel{ChannelStereo, ChannelLeft, ChannelRight} {
		if v := SelectChannel(nil, role, 2, 0); v != 0 {
			t.Fatalf("empty frame role=%s=%v, want 0", role, v)
		}
	}
}

// TestParseChannel covers the §6.5 string enum mapping incl. the "" default and
// the unknown-value error.
func TestParseChannel(t *testing.T) {
	tests := []struct {
		in      string
		want    Channel
		wantErr bool
	}{
		{"stereo", ChannelStereo, false},
		{"left", ChannelLeft, false},
		{"right", ChannelRight, false},
		{"", ChannelStereo, false}, // default = passthrough (R5)
		{"bogus", ChannelStereo, true},
		{"LEFT", ChannelStereo, true}, // case-sensitive enum
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseChannel(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseChannel(%q) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("ParseChannel(%q)=%v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestChannelString round-trips the three enum values to the §6.5 string enum.
func TestChannelString(t *testing.T) {
	for _, s := range []string{"stereo", "left", "right"} {
		c, err := ParseChannel(s)
		if err != nil {
			t.Fatalf("ParseChannel(%q) unexpected err: %v", s, err)
		}
		if got := c.String(); got != s {
			t.Fatalf("Channel(%q).String()=%q, want %q", s, got, s)
		}
	}
}

// TestGainLinear covers the doc 06 §5.2 mapping: the 0 dB short-circuit (exact
// 1.0, no math.Pow) and the ±6.0206 dB → ×0.5 / ×2.0 conversions, plus its use
// as g·SelectChannel on a sample vector.
func TestGainLinear(t *testing.T) {
	if g := GainLinear(0); g != 1.0 {
		t.Fatalf("GainLinear(0)=%v, want exactly 1.0", g)
	}
	if g := GainLinear(-6.0206); math.Abs(float64(g)-0.5) > 1e-3 {
		t.Fatalf("GainLinear(-6.0206)=%v, want ~0.5", g)
	}
	if g := GainLinear(6.0206); math.Abs(float64(g)-2.0) > 1e-3 {
		t.Fatalf("GainLinear(6.0206)=%v, want ~2.0", g)
	}

	// Applied as g · SelectChannel on a stereo passthrough vector.
	const L, R = float32(0.4), float32(-0.2)
	src := []float32{L, R}
	g := GainLinear(-6.0206)
	out0 := g * SelectChannel(src, ChannelStereo, 2, 0)
	out1 := g * SelectChannel(src, ChannelStereo, 2, 1)
	if math.Abs(float64(out0-L*0.5)) > 1e-3 {
		t.Fatalf("gained oc0=%v, want ~%v", out0, L*0.5)
	}
	if math.Abs(float64(out1-R*0.5)) > 1e-3 {
		t.Fatalf("gained oc1=%v, want ~%v", out1, R*0.5)
	}
}
