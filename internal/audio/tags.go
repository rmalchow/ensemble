package audio

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dhowden/tag"

	"ensemble/internal/contracts"
)

// ReadTags reads embedded media tags (ID3v1/v2, FLAC/Vorbis, MP4, OGG) from r
// and returns the now-playing metadata, falling back to fallbackName (typically
// the file's base name) when the title tag is absent or empty. r is left at an
// arbitrary offset — the caller must Seek(0) before decoding audio from it.
// A non-tagged or unreadable stream is not an error: it yields {Title: fallback}.
func ReadTags(r io.ReadSeeker, fallbackName string) contracts.TrackMetadata {
	md := contracts.TrackMetadata{Title: titleFallback(fallbackName)}
	m, err := tag.ReadFrom(r)
	if err != nil {
		return md
	}
	if t := strings.TrimSpace(m.Title()); t != "" {
		md.Title = t
	}
	if a := strings.TrimSpace(m.Artist()); a != "" {
		md.Artist = a
	}
	if al := strings.TrimSpace(m.Album()); al != "" {
		md.Album = al
	}
	md.HasArt = m.Picture() != nil // embedded art; callers OR-in any folder cover
	return md
}

// titleFallback strips a file's directory + extension to a human-ish title.
func titleFallback(name string) string {
	base := filepath.Base(name)
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}

// Probe reads tags for a media URI without standing up a decoder/session — used
// to pre-fill a queue entry's metadata at enqueue time. Only file URIs under
// mediaDir are probed; any other scheme (or a resolution/IO failure) returns
// ok=false, and the caller uses the URI-derived label instead.
func Probe(_ context.Context, uri, mediaDir string) (contracts.TrackMetadata, bool) {
	if schemeOf(uri) != SchemeFile {
		return contracts.TrackMetadata{}, false
	}
	full, err := resolveFilePath(uri, mediaDir)
	if err != nil {
		return contracts.TrackMetadata{}, false
	}
	f, err := os.Open(full)
	if err != nil {
		return contracts.TrackMetadata{}, false
	}
	defer f.Close()
	md := ReadTags(f, filepath.Base(full))
	md.HasArt = md.HasArt || folderCover(full) != "" // sibling cover.jpg/png/…
	return md, true
}
