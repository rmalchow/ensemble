package audio

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// id3v2 builds a minimal ID3v2.3 tag carrying the given TEXT frames (each a
// {frameID, text} pair) so the tag reader has something real to parse.
func id3v2(frames ...[2]string) []byte {
	var body []byte
	for _, fr := range frames {
		payload := append([]byte{0x00}, []byte(fr[1])...) // 0x00 = ISO-8859-1
		sz := len(payload)
		body = append(body, fr[0][0], fr[0][1], fr[0][2], fr[0][3],
			byte(sz>>24), byte(sz>>16), byte(sz>>8), byte(sz), 0, 0)
		body = append(body, payload...)
	}
	n := len(body)
	ss := []byte{byte((n >> 21) & 0x7f), byte((n >> 14) & 0x7f), byte((n >> 7) & 0x7f), byte(n & 0x7f)}
	head := append([]byte{'I', 'D', '3', 0x03, 0x00, 0x00}, ss...)
	return append(head, body...)
}

func TestReadTagsParsesTitleArtist(t *testing.T) {
	data := id3v2([2]string{"TIT2", "Test Title"}, [2]string{"TPE1", "Test Artist"}, [2]string{"TALB", "Test Album"})
	md := ReadTags(bytes.NewReader(data), "fallback.mp3")
	if md.Title != "Test Title" {
		t.Errorf("title = %q, want Test Title", md.Title)
	}
	if md.Artist != "Test Artist" {
		t.Errorf("artist = %q, want Test Artist", md.Artist)
	}
	if md.Album != "Test Album" {
		t.Errorf("album = %q, want Test Album", md.Album)
	}
}

func TestReadTagsFallbackToFilename(t *testing.T) {
	// Untagged bytes → title falls back to the base name, extension stripped.
	md := ReadTags(bytes.NewReader([]byte("not a tagged stream at all")), "My Song.mp3")
	if md.Title != "My Song" {
		t.Errorf("title = %q, want My Song", md.Title)
	}
	if md.Artist != "" || md.Album != "" {
		t.Errorf("artist/album should be empty, got %q/%q", md.Artist, md.Album)
	}
}

func TestReadTagsEmptyTitleTagFallsBack(t *testing.T) {
	// A present-but-empty title tag must still fall back to the filename.
	data := id3v2([2]string{"TIT2", ""})
	md := ReadTags(bytes.NewReader(data), "track01.flac")
	if md.Title != "track01" {
		t.Errorf("title = %q, want track01", md.Title)
	}
}

func TestProbe(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.mp3"), id3v2([2]string{"TIT2", "Hi"}), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	md, ok := Probe(ctx, "file:x.mp3", dir)
	if !ok || md.Title != "Hi" {
		t.Fatalf("Probe(file:x.mp3) = %+v, %v; want title Hi, ok", md, ok)
	}

	if _, ok := Probe(ctx, "http://example/stream", dir); ok {
		t.Errorf("Probe of non-file scheme should be ok=false")
	}
	if _, ok := Probe(ctx, "file:../escape.mp3", dir); ok {
		t.Errorf("Probe of traversal path should be ok=false")
	}
	if _, ok := Probe(ctx, "file:missing.mp3", dir); ok {
		t.Errorf("Probe of missing file should be ok=false")
	}
}
