package api

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMediaListerWalksAndFilters(t *testing.T) {
	dir := t.TempDir()
	must := func(rel string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("song.flac")
	must("sub/track.mp3")
	must("a.wav")
	must("notes.txt")  // skipped
	must("cover.jpg")  // skipped
	must("sub/x.FLAC") // case-insensitive ext

	l := NewMediaLister(dir)
	files, err := l.List()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f.Path] = true
	}
	want := []string{"a.wav", "song.flac", "sub/track.mp3", "sub/x.FLAC"}
	if len(files) != len(want) {
		t.Fatalf("got %d files %v, want %d", len(files), files, len(want))
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing %q in %v", w, got)
		}
	}
	// Sorted by path.
	for i := 1; i < len(files); i++ {
		if files[i-1].Path > files[i].Path {
			t.Errorf("not sorted: %q before %q", files[i-1].Path, files[i].Path)
		}
	}
	// Metadata present.
	if files[0].Name == "" || files[0].SizeBytes == 0 || files[0].ModTime == 0 {
		t.Errorf("metadata missing: %+v", files[0])
	}
}

func TestMediaListerMissingDir(t *testing.T) {
	l := NewMediaLister(filepath.Join(t.TempDir(), "does-not-exist"))
	files, err := l.List()
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("missing dir should yield empty list, got %v", files)
	}
}
