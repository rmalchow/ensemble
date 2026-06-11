package audio

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"ensemble/internal/contracts"
)

// fileSource is the pull-paced source: a file under MEDIA_DIR decoded through a
// framer, returning real io.EOF at the end (D9).
type fileSource struct {
	f      *os.File
	fr     *framer
	meta   contracts.TrackMetadata // embedded tags, title falling back to the base name
	closed bool
}

// Metadata satisfies the optional metadata channel with the file's embedded tags
// (title/artist/album), the title falling back to the base name when untagged.
func (s *fileSource) Metadata() (contracts.TrackMetadata, bool) {
	if s.meta.Title == "" {
		return contracts.TrackMetadata{}, false
	}
	return s.meta, true
}

// resolveFilePath maps a "file:" URI (or bare path) to an absolute path under
// mediaDir, rejecting absolute paths and traversal outside it (§6).
func resolveFilePath(uri, mediaDir string) (string, error) {
	rel := uri
	if i := strings.Index(rel, ":"); i >= 0 && strings.EqualFold(rel[:i], "file") {
		rel = rel[i+1:]
	}
	rel = strings.TrimPrefix(rel, "//") // tolerate file://path

	// Absolute paths escape MEDIA_DIR by definition.
	if filepath.IsAbs(rel) {
		return "", ErrTraversal
	}
	clean := filepath.Clean(rel)
	full := filepath.Join(mediaDir, clean)

	// Verify the cleaned result stays inside mediaDir.
	relCheck, err := filepath.Rel(mediaDir, full)
	if err != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
		return "", ErrTraversal
	}
	return full, nil
}

// openFile constructs a file source for a "file:" URI or a bare path, bounding
// resolution to mediaDir (traversal guard, §6).
func openFile(_ context.Context, uri, mediaDir string) (Source, error) {
	full, err := resolveFilePath(uri, mediaDir)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(full)
	if err != nil {
		return nil, fmt.Errorf("%w: open %q: %v", ErrBadMedia, uri, err)
	}

	// Read embedded tags first (consumes the reader), then rewind so the decoder
	// sees the file from byte 0.
	meta := ReadTags(f, filepath.Base(full))
	meta.HasArt = meta.HasArt || folderCover(full) != "" // sibling cover.jpg/png/…
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, fmt.Errorf("%w: seek %q: %v", ErrBadMedia, uri, err)
	}

	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(full)), ".")
	dec, err := newDecoder(f, ext)
	if err != nil {
		f.Close()
		return nil, err
	}

	// Surface the track duration (for the UI transport bar) through the metadata
	// channel when the decoder can report it (mp3/flac/wav with a known length).
	if d, ok := dec.(interface{ duration() (float64, bool) }); ok {
		if secs, ok := d.duration(); ok && secs > 0 {
			meta.DurationSec = int(secs + 0.5)
		}
	}

	return &fileSource{f: f, fr: newFramer(dec), meta: meta}, nil
}

func (s *fileSource) ReadFrame(dst []byte) error { return s.fr.frame(dst) }

func (s *fileSource) Live() bool { return false }

// Seek jumps to sec within the file (satisfies Seeker), repositioning the decoder
// and clearing the framer's buffered/resampled state. ErrNotSeekable if the
// underlying decoder/reader can't seek.
func (s *fileSource) Seek(sec float64) error {
	if s.closed {
		return ErrNotSeekable
	}
	return s.fr.seek(sec)
}

func (s *fileSource) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.f.Close()
}
