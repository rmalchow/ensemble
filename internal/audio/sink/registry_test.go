package audio

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// stubPATH builds a temp dir holding executable stubs for each name in present
// and sets PATH to ONLY that dir for the test, so exec.LookPath resolves exactly
// the intended set. It returns nothing; t.Setenv restores PATH on cleanup.
func stubPATH(t *testing.T, present ...string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("PATH stub uses POSIX shell scripts")
	}
	dir := t.TempDir()
	for _, name := range present {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	t.Setenv("PATH", dir)
}

func names(bs []Backend) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Name
	}
	return out
}

func TestProbe(t *testing.T) {
	// In CI there is no usable real card, so the alsa probe must fail and never
	// appear (proving "node present != usable", 06 §1.1). These cases assert the
	// exec backends only.
	tests := []struct {
		name    string
		present []string // binaries placed on PATH
		cfg     ProbeConfig
		want    []string // expected backend Names in order
	}{
		{
			name:    "both exec players resolvable, default order pw-play before aplay",
			present: []string{"aplay", "pw-play"},
			cfg:     ProbeConfig{},
			want:    []string{"exec:pw-play", "exec:aplay"},
		},
		{
			name:    "only aplay present",
			present: []string{"aplay"},
			cfg:     ProbeConfig{},
			want:    []string{"exec:aplay"},
		},
		{
			name:    "only pw-play present",
			present: []string{"pw-play"},
			cfg:     ProbeConfig{},
			want:    []string{"exec:pw-play"},
		},
		{
			name:    "no players present => empty",
			present: nil,
			cfg:     ProbeConfig{},
			want:    nil,
		},
		{
			name:    "disable subtracts a backend",
			present: []string{"aplay", "pw-play"},
			cfg:     ProbeConfig{Disabled: []string{"exec:pw-play"}},
			want:    []string{"exec:aplay"},
		},
		{
			name:    "prefer reorders aplay ahead of pw-play",
			present: []string{"aplay", "pw-play"},
			cfg:     ProbeConfig{Prefer: []string{"exec:aplay"}},
			want:    []string{"exec:aplay", "exec:pw-play"},
		},
		{
			name:    "disable alsa is a no-op here (alsa not usable in CI anyway)",
			present: []string{"aplay"},
			cfg:     ProbeConfig{Disabled: []string{"alsa"}},
			want:    []string{"exec:aplay"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubPATH(t, tt.present...)
			// Hermetic: subtract the host-dependent ALSA tiers (the dev box may
			// genuinely have libasound); this table pins the EXEC ordering only.
			cfg := tt.cfg
			cfg.Disabled = append(append([]string{}, cfg.Disabled...), BackendALSALib)
			got := names(Probe(cfg))
			// alsa must never appear: no usable card in CI (06 §1.1 liveness check).
			if slices.Contains(got, BackendALSA) {
				t.Fatalf("alsa must not be returned in CI (no usable card); got %v", got)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("Probe order = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOpenSelection(t *testing.T) {
	stubPATH(t, "aplay", "pw-play")

	// Explicit exec:aplay selection returns an *ExecSink bound to the device.
	s, err := Open([]string{"exec:aplay"}, "hw:1,0")
	if err != nil {
		t.Fatalf("Open(exec:aplay): %v", err)
	}
	es, ok := s.(*ExecSink)
	if !ok {
		t.Fatalf("Open returned %T, want *ExecSink", s)
	}
	if es.device != "hw:1,0" {
		t.Fatalf("ExecSink device = %q, want %q", es.device, "hw:1,0")
	}
	if d, ok := s.Delay(); d != 0 || ok {
		t.Fatalf("exec Delay() = (%d,%v), want (0,false)", d, ok)
	}

	// alsa selection errors in CI (no usable card).
	if _, err := Open([]string{"alsa"}, "default"); err == nil {
		t.Fatalf("Open(alsa) should error in CI (no usable card)")
	}

	// Empty preferred falls back to default order. The host-dependent ALSA tiers
	// are excluded explicitly (a dev box may have libasound); among the exec
	// stubs pw-play wins by default preference.
	s2, err := Open([]string{"exec:pw-play", "exec:aplay"}, "default")
	if err != nil {
		t.Fatalf("Open([]) fallback: %v", err)
	}
	defer s2.Close()
	es2, ok := s2.(*ExecSink)
	if !ok {
		t.Fatalf("fallback Open returned %T, want *ExecSink", s2)
	}
	// pw-play template => argv[0] is pw-play.
	if es2.command == nil || es2.command[0] != "pw-play" {
		t.Fatalf("fallback chose %v, want pw-play first", es2.command)
	}

	// Unknown backend name errors.
	if _, err := Open([]string{"bogus"}, "default"); err == nil {
		t.Fatalf("Open(bogus) should error")
	}
}

func TestMaxRate(t *testing.T) {
	// Hermetic against a dev box with libasound: exec tiers only.
	hermetic := ProbeConfig{Disabled: []string{BackendALSALib, BackendALSA}}
	stubPATH(t, "aplay")
	if r := MaxRate(hermetic); r != canonicalRate {
		t.Fatalf("MaxRate with a usable backend = %d, want %d", r, canonicalRate)
	}

	stubPATH(t) // no players
	if r := MaxRate(hermetic); r != 0 {
		t.Fatalf("MaxRate with no usable backend = %d, want 0", r)
	}
}

func TestOrderedNames(t *testing.T) {
	// Prefer with unknown + duplicate names: unknowns dropped, dups collapsed,
	// remainder appended in default order.
	got := orderedNames([]string{"exec:aplay", "bogus", "exec:aplay"})
	want := []string{"exec:aplay", "alsalib", "alsa", "exec:pw-play"}
	if !slices.Equal(got, want) {
		t.Fatalf("orderedNames = %v, want %v", got, want)
	}
}
