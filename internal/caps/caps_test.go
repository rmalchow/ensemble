package caps

import (
	"slices"
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/config"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

func boolPtr(b bool) *bool { return &b }

// allFEC is the canonical FEC set every node advertises (A.10, D4).
var allFEC = []string{fecDuplicate, fecNone, fecXORParity} // sorted

func TestCompute(t *testing.T) {
	tests := []struct {
		name string
		d    Detected
		m    Mask
		want state.Capabilities
	}{
		{
			name: "no mask",
			d: Detected{
				Sinks:        []string{"alsa", "exec:aplay"},
				EncodeCodecs: []string{codecPCM},
				DecodeCodecs: []string{codecPCM},
				FEC:          []string{fecNone, fecXORParity, fecDuplicate},
				MaxRate:      canonicalRate,
			},
			m: Mask{},
			want: state.Capabilities{
				Render:       true,
				Sinks:        []string{"alsa", "exec:aplay"},
				EncodeCodecs: []string{codecPCM},
				DecodeCodecs: []string{codecPCM},
				FEC:          allFEC,
				MaxRate:      canonicalRate,
			},
		},
		{
			name: "disable backend keeps render",
			d: Detected{
				Sinks:   []string{"alsa", "exec:aplay"},
				FEC:     []string{fecNone, fecXORParity, fecDuplicate},
				MaxRate: canonicalRate,
			},
			m: Mask{DisableBackends: []string{"alsa"}},
			want: state.Capabilities{
				Render:  true,
				Sinks:   []string{"exec:aplay"},
				FEC:     allFEC,
				MaxRate: canonicalRate,
			},
		},
		{
			name: "disable last backend => sink-less",
			d: Detected{
				Sinks:   []string{"alsa", "exec:aplay"},
				FEC:     []string{fecNone, fecXORParity, fecDuplicate},
				MaxRate: canonicalRate,
			},
			m: Mask{DisableBackends: []string{"alsa", "exec:aplay"}},
			want: state.Capabilities{
				Render:  false,
				Sinks:   nil,
				FEC:     allFEC,
				MaxRate: 0,
			},
		},
		{
			name: "force render false empties sinks",
			d: Detected{
				Sinks:        []string{"alsa"},
				EncodeCodecs: []string{codecPCM},
				FEC:          []string{fecNone, fecXORParity, fecDuplicate},
				MaxRate:      canonicalRate,
			},
			m: Mask{ForceRender: boolPtr(false)},
			want: state.Capabilities{
				Render:       false,
				Sinks:        nil,
				EncodeCodecs: []string{codecPCM},
				FEC:          allFEC,
				MaxRate:      0,
			},
		},
		{
			name: "force render true is not force-on",
			d: Detected{
				Sinks: nil,
				FEC:   []string{fecNone, fecXORParity, fecDuplicate},
			},
			m: Mask{ForceRender: boolPtr(true)},
			want: state.Capabilities{
				Render:  false,
				Sinks:   nil,
				FEC:     allFEC,
				MaxRate: 0,
			},
		},
		{
			name: "disable codec drops opus from both, keeps pcm",
			d: Detected{
				Sinks:        []string{"alsa"},
				EncodeCodecs: []string{codecPCM, codecOpus},
				DecodeCodecs: []string{codecPCM, codecOpus},
				FEC:          []string{fecNone, fecXORParity, fecDuplicate},
				MaxRate:      canonicalRate,
			},
			m: Mask{DisableCodecs: []string{codecOpus}},
			want: state.Capabilities{
				Render:       true,
				Sinks:        []string{"alsa"},
				EncodeCodecs: []string{codecPCM},
				DecodeCodecs: []string{codecPCM},
				FEC:          allFEC,
				MaxRate:      canonicalRate,
			},
		},
		{
			name: "prefer is order-only, does not change membership",
			d: Detected{
				Sinks:   []string{"alsa", "exec:aplay"},
				FEC:     []string{fecNone, fecXORParity, fecDuplicate},
				MaxRate: canonicalRate,
			},
			m: Mask{PreferBackends: []string{"exec:aplay", "alsa"}},
			want: state.Capabilities{
				Render:  true,
				Sinks:   []string{"alsa", "exec:aplay"}, // sorted, membership unchanged
				FEC:     allFEC,
				MaxRate: canonicalRate,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Compute(tt.d, tt.m)
			if !capsEqual(got, tt.want) {
				t.Fatalf("Compute() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestComputeDeterminism verifies shuffled inputs yield identical sorted/deduped
// output (§7.1 "determinism / sorted").
func TestComputeDeterminism(t *testing.T) {
	a := Detected{
		Sinks:        []string{"exec:aplay", "alsa", "alsa"},
		EncodeCodecs: []string{codecOpus, codecPCM},
		DecodeCodecs: []string{codecPCM, codecOpus, codecPCM},
		FEC:          []string{fecDuplicate, fecNone, fecXORParity},
		MaxRate:      canonicalRate,
	}
	b := Detected{
		Sinks:        []string{"alsa", "exec:aplay"},
		EncodeCodecs: []string{codecPCM, codecOpus},
		DecodeCodecs: []string{codecOpus, codecPCM},
		FEC:          []string{fecXORParity, fecDuplicate, fecNone},
		MaxRate:      canonicalRate,
	}
	ca, cb := Compute(a, Mask{}), Compute(b, Mask{})
	if !capsEqual(ca, cb) {
		t.Fatalf("non-deterministic Compute: %+v vs %+v", ca, cb)
	}
	// And no duplicate "alsa" survives.
	if got := ca.Sinks; !slices.Equal(got, []string{"alsa", "exec:aplay"}) {
		t.Fatalf("Sinks not deduped/sorted: %v", got)
	}
}

func TestMaskFromConfig(t *testing.T) {
	a := config.AudioConfig{
		Render: boolPtr(false),
		Backends: config.BackendsConfig{
			Disable: []string{"pipewire"},
			Prefer:  []string{"alsa"},
		},
		Codecs: config.CodecsConfig{
			Disable: []string{codecOpus},
		},
	}
	got := MaskFromConfig(a)

	if got.ForceRender == nil || *got.ForceRender != false {
		t.Errorf("ForceRender = %v, want &false", got.ForceRender)
	}
	if !slices.Equal(got.DisableBackends, []string{"pipewire"}) {
		t.Errorf("DisableBackends = %v", got.DisableBackends)
	}
	if !slices.Equal(got.PreferBackends, []string{"alsa"}) {
		t.Errorf("PreferBackends = %v", got.PreferBackends)
	}
	if !slices.Equal(got.DisableCodecs, []string{codecOpus}) {
		t.Errorf("DisableCodecs = %v", got.DisableCodecs)
	}
}

func TestProbe(t *testing.T) {
	tests := []struct {
		name     string
		sinks    SinkProber
		maxRate  MaxRateProber
		wantSink []string
		wantRate int
	}{
		{
			name: "assembly with two sinks",
			sinks: func() []Backend {
				return []Backend{
					{Name: "alsa", Precise: true},
					{Name: "exec:aplay", Precise: false},
				}
			},
			maxRate:  func() int { return canonicalRate },
			wantSink: []string{"alsa", "exec:aplay"},
			wantRate: canonicalRate,
		},
		{
			name:     "sink-less node",
			sinks:    func() []Backend { return nil },
			maxRate:  func() int { return 0 },
			wantSink: nil,
			wantRate: 0,
		},
		{
			name: "coarse exec path falls back to canonical rate",
			sinks: func() []Backend {
				return []Backend{{Name: "exec:aplay"}}
			},
			maxRate:  func() int { return 0 }, // prober cannot query the device
			wantSink: []string{"exec:aplay"},
			wantRate: canonicalRate,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Probe(ProbeDeps{Sinks: tt.sinks, MaxRate: tt.maxRate})

			if !slices.Equal(d.Sinks, tt.wantSink) {
				t.Errorf("Sinks = %v, want %v", d.Sinks, tt.wantSink)
			}
			if d.MaxRate != tt.wantRate {
				t.Errorf("MaxRate = %d, want %d", d.MaxRate, tt.wantRate)
			}
			// Under -tags opus with libopus present, Opus is legitimately
			// detected by detectCodecs, so the codec lists include opus.
			// The exact-match assertion below is only valid for the default
			// (!opus) build or when libopus is absent at runtime.
			// Opus presence is covered by TestOpusPresentInDetect (caps_opus_test.go).
			if !opusAvailable() {
				// Opus is deferred (A.11): MVP advertises pcm only.
				if !slices.Equal(d.EncodeCodecs, []string{codecPCM}) {
					t.Errorf("EncodeCodecs = %v, want [pcm]", d.EncodeCodecs)
				}
				if !slices.Equal(d.DecodeCodecs, []string{codecPCM}) {
					t.Errorf("DecodeCodecs = %v, want [pcm]", d.DecodeCodecs)
				}
			} else {
				// Opus is present: pcm floor must still be there.
				if !slices.Contains(d.EncodeCodecs, codecPCM) {
					t.Errorf("EncodeCodecs %v missing pcm floor", d.EncodeCodecs)
				}
				if !slices.Contains(d.DecodeCodecs, codecPCM) {
					t.Errorf("DecodeCodecs %v missing pcm floor", d.DecodeCodecs)
				}
			}
			if !slices.Equal(d.FEC, allFEC) {
				t.Errorf("FEC = %v, want %v", d.FEC, allFEC)
			}
		})
	}
}

// TestProbeNilSeams verifies a defensive nil seam never panics and reports a
// sink-less node (R7 fail-soft).
func TestProbeNilSeams(t *testing.T) {
	d := Probe(ProbeDeps{})
	if d.Sinks != nil || d.MaxRate != 0 {
		t.Fatalf("nil seams: got Sinks=%v MaxRate=%d, want nil/0", d.Sinks, d.MaxRate)
	}
}

// TestProbeThroughCompute checks the sink-less probe flows to Render=false while
// keeping the pcm encode baseline (control/media-only origin, D17, §7.1).
func TestProbeThroughCompute(t *testing.T) {
	d := Probe(ProbeDeps{
		Sinks:   func() []Backend { return nil },
		MaxRate: func() int { return 0 },
	})
	c := Compute(d, Mask{})
	if c.Render {
		t.Error("sink-less probe should yield Render=false")
	}
	// Under -tags opus with libopus present, Opus is legitimately detected so
	// EncodeCodecs contains both pcm and opus.  The pcm floor must always be
	// present; the exact-[pcm]-only assertion is only valid when Opus is absent.
	if !opusAvailable() {
		if !slices.Equal(c.EncodeCodecs, []string{codecPCM}) {
			t.Errorf("EncodeCodecs = %v, want [pcm] for a sink-less origin", c.EncodeCodecs)
		}
	} else {
		if !slices.Contains(c.EncodeCodecs, codecPCM) {
			t.Errorf("EncodeCodecs %v missing pcm floor", c.EncodeCodecs)
		}
	}
}

// TestOpusAbsentMVP pins the default-build behavior: with Opus compiled out
// (no `opus` build tag) detectCodecs reports no opus. Under `-tags opus` the
// codec is intentionally available, so this assertion is skipped — Opus
// presence is covered by caps_opus_test.go (//go:build opus).
func TestOpusAbsentMVP(t *testing.T) {
	if opusAvailable() {
		t.Skip("opus build: Opus is available by design; presence covered by caps_opus_test.go")
	}
	enc, dec := detectCodecs()
	if slices.Contains(enc, codecOpus) || slices.Contains(dec, codecOpus) {
		t.Fatalf("opus must be absent: enc=%v dec=%v", enc, dec)
	}
}
