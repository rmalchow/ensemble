package audio

import (
	"os"
	"path/filepath"
	"testing"
)

// A sibling cover image is found case-insensitively and served with the right
// content type, and the track's metadata advertises HasArt.
func TestCoverArtFolderImage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "song.mp3"), id3v2([2]string{"TIT2", "S"}), 0o644); err != nil {
		t.Fatal(err)
	}
	// Mixed case on both stem and extension — must still match.
	pngBytes := []byte("\x89PNG\r\n\x1a\nfake")
	if err := os.WriteFile(filepath.Join(dir, "Cover.PNG"), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	data, ctype, ok := CoverArt("file:song.mp3", dir)
	if !ok {
		t.Fatal("CoverArt ok=false, want a folder cover")
	}
	if ctype != "image/png" {
		t.Errorf("content type = %q, want image/png", ctype)
	}
	if string(data) != string(pngBytes) {
		t.Errorf("cover bytes mismatch")
	}

	if md, ok := Probe(nil, "file:song.mp3", dir); !ok || !md.HasArt {
		t.Errorf("Probe HasArt = %v (ok=%v), want true", md.HasArt, ok)
	}
}

// "cover" wins over alternate stems regardless of directory order.
func TestFolderCoverPrefersCover(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"front.jpg", "cover.jpg", "folder.png"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := folderCover(filepath.Join(dir, "track.flac"))
	if filepath.Base(got) != "cover.jpg" {
		t.Errorf("folderCover = %q, want cover.jpg", filepath.Base(got))
	}
}

// No sibling image and no embedded picture → no art (ok=false, HasArt=false).
func TestCoverArtNone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bare.mp3"), id3v2([2]string{"TIT2", "S"}), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := CoverArt("file:bare.mp3", dir); ok {
		t.Error("CoverArt ok=true, want false for a track with no art")
	}
	if md, ok := Probe(nil, "file:bare.mp3", dir); !ok || md.HasArt {
		t.Errorf("Probe HasArt = %v, want false", md.HasArt)
	}
}

// A non-file URI (spotify/http) is never a cover source.
func TestCoverArtRejectsNonFile(t *testing.T) {
	if _, _, ok := CoverArt("spotify:track:x", t.TempDir()); ok {
		t.Error("CoverArt ok=true for a spotify URI, want false")
	}
}

// A random sibling image that isn't a recognized cover stem is ignored.
func TestFolderCoverIgnoresOtherImages(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "screenshot.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := folderCover(filepath.Join(dir, "track.mp3")); got != "" {
		t.Errorf("folderCover = %q, want empty (non-cover image)", got)
	}
}
