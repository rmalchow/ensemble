package api

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// mediaExts are the playable file extensions (§6, lowercase, with dot).
var mediaExts = map[string]bool{
	".wav":  true,
	".mp3":  true,
	".flac": true,
}

// fsLister implements Media by walking a media directory (§6). Paths in the
// result are relative to the directory root, with traversal kept inside it.
type fsLister struct {
	dir string
}

// NewMediaLister returns a Media implementation that recursively scans dir for
// playable files (§6 extensions .wav/.mp3/.flac), rescanned on each List call.
func NewMediaLister(dir string) Media {
	return &fsLister{dir: dir}
}

// List walks the media directory and returns playable files, sorted by path.
// A missing directory yields an empty list (not an error): a node may have no
// media. Symlink loops are bounded by WalkDir's own handling.
func (l *fsLister) List() ([]MediaFile, error) {
	var out []MediaFile
	err := filepath.WalkDir(l.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !mediaExts[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}
		rel, rerr := filepath.Rel(l.dir, p)
		if rerr != nil {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		out = append(out, MediaFile{
			Path:      filepath.ToSlash(rel),
			Name:      d.Name(),
			SizeBytes: info.Size(),
			ModTime:   info.ModTime().Unix(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
