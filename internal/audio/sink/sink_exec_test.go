package audio

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestF32ToS16LE(t *testing.T) {
	// le builds the expected 2-byte little-endian encoding of a sample value.
	le := func(n int16) []byte {
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], uint16(n))
		return b[:]
	}

	cases := []struct {
		name string
		in   []float32
		want []byte
	}{
		{"zero", []float32{0}, le(0)},
		{"full positive", []float32{1.0}, le(32767)},
		{"full negative", []float32{-1.0}, le(-32767)},
		{"clamp over +1", []float32{2.5}, le(32767)},
		{"clamp under -1", []float32{-2.5}, le(-32767)},
		{"half positive", []float32{0.5}, le(int16(math.Round(0.5 * 32767)))},
		{"half negative", []float32{-0.5}, le(int16(math.Round(-0.5 * 32767)))},
		{
			"interleave stereo order preserved",
			[]float32{1.0, -1.0, 0.0, 0.5},
			concatBytes(le(32767), le(-32767), le(0), le(int16(math.Round(0.5*32767)))),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dst := make([]byte, len(c.in)*2)
			f32ToS16LE(c.in, dst)
			if string(dst) != string(c.want) {
				t.Fatalf("f32ToS16LE(%v) = % x, want % x", c.in, dst, c.want)
			}
		})
	}
}

func TestExpandPlayerCommand(t *testing.T) {
	tmpl := defaultPlayerCommand("hw:0")
	got := expandPlayerCommand(tmpl, 48000, 2, "hw:0")
	want := []string{"aplay", "-q", "-t", "raw", "-f", "S16_LE", "-r", "48000", "-c", "2", "-D", "hw:0", "-"}
	if len(got) != len(want) {
		t.Fatalf("argv len = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// pw-play template substitution (the registry's PipeWire coarse backend).
	pw := expandPlayerCommand(pwPlayCommand("alsa_output.x"), 44100, 1, "alsa_output.x")
	wantPw := []string{"pw-play", "--rate", "44100", "--channels", "1", "--format", "s16", "--target", "alsa_output.x", "-"}
	if len(pw) != len(wantPw) {
		t.Fatalf("pw-play argv len = %d, want %d: %v", len(pw), len(wantPw), pw)
	}
	for i := range wantPw {
		if pw[i] != wantPw[i] {
			t.Fatalf("pw-play argv[%d] = %q, want %q", i, pw[i], wantPw[i])
		}
	}

	// {device} placeholder substitution (for a custom template).
	custom := []string{"player", "--rate", "{rate}", "--ch", "{channels}", "--dev", "{device}"}
	gotc := expandPlayerCommand(custom, 44100, 1, "pulse")
	wantc := []string{"player", "--rate", "44100", "--ch", "1", "--dev", "pulse"}
	for i := range wantc {
		if gotc[i] != wantc[i] {
			t.Fatalf("custom argv[%d] = %q, want %q", i, gotc[i], wantc[i])
		}
	}
}

func TestNewExecSinkDefaultDevice(t *testing.T) {
	s := NewExecSink("")
	if s.device != "default" {
		t.Fatalf("empty device should default to %q, got %q", "default", s.device)
	}
	if d, ok := s.Delay(); d != 0 || ok {
		t.Fatalf("Delay() = (%d,%v), want (0,false)", d, ok)
	}
}

// writeStubPlayer drops a tiny shell script that copies its stdin to outPath and
// exits 0, then returns a command template that invokes it. It is the stand-in
// for aplay so the lifecycle test needs no real audio device.
func writeStubPlayer(t *testing.T, dir, outPath string) []string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub player script is POSIX shell")
	}
	script := filepath.Join(dir, "stubplayer.sh")
	// The template carries the placeholders so we also exercise expansion through
	// Start; the stub ignores the args and just drains stdin to the output file.
	body := "#!/bin/sh\ncat > \"" + outPath + "\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return []string{script, "-r", "{rate}", "-c", "{channels}", "-D", "{device}"}
}

func TestExecSinkLifecycle(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "captured.pcm")

	s := &ExecSink{device: "hw:0", command: writeStubPlayer(t, dir, out)}

	if err := s.Start(48000, 2); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Second Start must error (idempotent-error contract).
	if err := s.Start(48000, 2); err == nil {
		t.Fatalf("second Start should error")
	}

	if d, ok := s.Delay(); d != 0 || ok {
		t.Fatalf("Delay() = (%d,%v), want (0,false)", d, ok)
	}

	frames := []float32{1.0, -1.0, 0.0, 0.5}
	n, err := s.Write(frames)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(frames) {
		t.Fatalf("Write consumed %d samples, want %d", n, len(frames))
	}

	// Odd-length write on a stereo sink is rejected.
	if _, err := s.Write([]float32{0.1}); err == nil {
		t.Fatalf("odd-length Write should error")
	}

	// Close drains stdin and waits for the stub to exit.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close is idempotent.
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read captured: %v", err)
	}
	want := make([]byte, len(frames)*2)
	f32ToS16LE(frames, want)
	if string(got) != string(want) {
		t.Fatalf("captured PCM = % x, want % x", got, want)
	}
}

func TestExecSinkWriteBeforeStart(t *testing.T) {
	s := NewExecSink("default")
	if _, err := s.Write([]float32{0, 0}); err == nil {
		t.Fatalf("Write before Start should error")
	}
}

func TestExecSinkStartClosed(t *testing.T) {
	dir := t.TempDir()
	s := &ExecSink{device: "default", command: writeStubPlayer(t, dir, filepath.Join(dir, "x.pcm"))}
	if err := s.Close(); err != nil {
		t.Fatalf("Close before Start: %v", err)
	}
	if err := s.Start(48000, 2); err == nil {
		t.Fatalf("Start on closed sink should error")
	}
}

func TestExecSinkInvalidStartArgs(t *testing.T) {
	s := NewExecSink("default")
	if err := s.Start(0, 2); err == nil {
		t.Fatalf("Start with rate 0 should error")
	}
	if err := s.Start(48000, 0); err == nil {
		t.Fatalf("Start with channels 0 should error")
	}
}

func concatBytes(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
