package source

import (
	"os"
	"path/filepath"
	"testing"
)

func TestList(t *testing.T) {
	dir := t.TempDir()
	// Playable media (varied case extensions).
	writeWAV(t, filepath.Join(dir, "b.wav"), sineSamples(2205, 44100, 2), 44100, 2)
	writeFLAC(t, filepath.Join(dir, "a.flac"), sineSamples(2205, 48000, 2), 48000, 2)
	copyData(t, dir, mp3Fixture("sine_44100.mp3"), "c.MP3")
	// Non-media + a subdir, both ignored.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cover.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeWAV(t, filepath.Join(dir, "sub", "nested.wav"), sineSamples(100, 44100, 2), 44100, 2)

	got, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Sorted by Name, only {a.flac, b.wav, c.MP3}.
	wantNames := []string{"a.flac", "b.wav", "c.MP3"}
	if len(got) != len(wantNames) {
		t.Fatalf("got %d entries %+v, want %d", len(got), got, len(wantNames))
	}
	for i, w := range wantNames {
		if got[i].Name != w {
			t.Errorf("entry %d Name=%q want %q", i, got[i].Name, w)
		}
		if got[i].SizeBytes <= 0 {
			t.Errorf("entry %q SizeBytes=%d", got[i].Name, got[i].SizeBytes)
		}
	}
	byName := map[string]MediaInfo{}
	for _, m := range got {
		byName[m.Name] = m
	}
	if m := byName["a.flac"]; m.Format != "flac" || m.SampleRate != 48000 || m.Channels != 2 {
		t.Errorf("a.flac: %+v", m)
	}
	if m := byName["b.wav"]; m.Format != "wav" || m.SampleRate != 44100 || m.Channels != 2 {
		t.Errorf("b.wav: %+v", m)
	}
	if m := byName["c.MP3"]; m.Format != "mp3" || m.SampleRate != 44100 || m.Channels != 2 {
		t.Errorf("c.MP3: %+v", m)
	}
}

func TestListEmptyAndMissing(t *testing.T) {
	empty := t.TempDir()
	got, err := List(empty)
	if err != nil {
		t.Fatalf("List(empty): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty dir returned %d entries", len(got))
	}
	if _, err := List(filepath.Join(empty, "does-not-exist")); err == nil {
		t.Error("List(missing): expected error, got nil")
	}
}
