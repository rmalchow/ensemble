package audio

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/dhowden/tag"
)

// coverStems are the sibling image basenames (no extension, case-insensitive) we
// treat as folder cover art, in preference order — "cover" wins over the common
// alternates. A track's now-playing art is its folder cover if present, else the
// file's embedded picture.
var coverStems = []string{"cover", "folder", "front", "album", "albumart"}

// coverExts maps a lowercase image extension to the content type a browser renders
// inline. Only these are considered cover art.
var coverExts = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".gif":  "image/gif",
}

// maxCoverBytes caps a folder cover read so a stray huge image can't balloon a
// response (embedded art is already bounded by the file).
const maxCoverBytes = 8 << 20 // 8 MiB

// folderCover returns the path of the sibling cover image for the track at full
// (e.g. <dir>/cover.jpg — both name and extension matched case-insensitively),
// preferring coverStems order, or "" when the folder has none.
func folderCover(full string) string {
	dir := filepath.Dir(full)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best, bestRank := "", len(coverStems)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if coverExts[strings.ToLower(filepath.Ext(name))] == "" {
			continue
		}
		stem := strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
		for rank, want := range coverStems {
			if stem == want && rank < bestRank {
				best, bestRank = filepath.Join(dir, name), rank
				break
			}
		}
	}
	return best
}

// hasCoverArt reports whether the track at full has displayable art: a sibling
// cover image, or (when m is non-nil) an embedded picture. Used at tag-read time
// to advertise TrackMetadata.HasArt without loading the bytes.
func hasCoverArt(full string, m tag.Metadata) bool {
	if folderCover(full) != "" {
		return true
	}
	return m != nil && m.Picture() != nil
}

// CoverArt resolves a file: URI under mediaDir and returns its cover image bytes
// and content type: the sibling cover image (preferred) else the embedded picture.
// ok=false when the URI is not a file under mediaDir, or no art exists. Served by
// the /cover endpoint; the UI requests it only when HasArt was advertised.
func CoverArt(uri, mediaDir string) (data []byte, contentType string, ok bool) {
	if schemeOf(uri) != SchemeFile {
		return nil, "", false
	}
	full, err := resolveFilePath(uri, mediaDir)
	if err != nil {
		return nil, "", false
	}
	if p := folderCover(full); p != "" {
		if fi, err := os.Stat(p); err == nil && fi.Size() > 0 && fi.Size() <= maxCoverBytes {
			if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
				return b, coverExts[strings.ToLower(filepath.Ext(p))], true
			}
		}
	}
	// Fall back to the file's embedded picture.
	f, err := os.Open(full)
	if err != nil {
		return nil, "", false
	}
	defer f.Close()
	m, err := tag.ReadFrom(f)
	if err != nil {
		return nil, "", false
	}
	if pic := m.Picture(); pic != nil && len(pic.Data) > 0 {
		ct := pic.MIMEType
		if ct == "" {
			ct = "application/octet-stream"
		}
		return pic.Data, ct, true
	}
	return nil, "", false
}
