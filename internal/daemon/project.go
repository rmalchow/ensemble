package daemon

// project.go holds the state.ConfigDoc -> web view-type projections the media/
// status Deps closures need (08 §0.7 / §G.2 field names). They live in daemon
// (the bridge) so web stays decoupled from state's concrete type. The
// ConfigDoc.Secrets (ClusterSecrets: CA private key + shared secret) is NEVER
// projected — web read endpoints must not serve the CA key (doc 01 §2 / 09 §2.8
// redaction); configView simply omits the field, so it cannot leak.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/source"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/web"
)

// configView projects a state.ConfigDoc into the web.ConfigView, REDACTING
// Secrets (the CA private key / shared secret are never present in a view type).
// It is the single projection used by every read closure so the redaction is in
// one place. Every slice field is emitted NON-NIL: nil marshals to JSON null,
// which the SPA's `addrs.join(...)`-style consumers crash on.
func configView(doc state.ConfigDoc) web.ConfigView {
	v := web.ConfigView{Version: doc.Version}
	for _, n := range doc.Nodes {
		v.Nodes = append(v.Nodes, nodeView(n))
	}
	for _, g := range doc.Groups {
		v.Groups = append(v.Groups, web.GroupView{
			ID:            g.ID,
			Name:          g.Name,
			MemberNodeIDs: nonNil(g.MemberNodeIDs),
			Profile:       profileView(g.Profile),
			Media:         web.Media{File: g.Media.File, Loop: g.Media.Loop},
			Playing:       g.Playing,
		})
	}
	return v
}

// nodeView projects one state.NodeRecord into the web.NodeView (shared by the
// ConfigView list and the §D.2 node detail).
func nodeView(n state.NodeRecord) web.NodeView {
	devs := make([]web.AudioDeviceView, 0, len(n.AudioDevices))
	for _, d := range n.AudioDevices {
		devs = append(devs, web.AudioDeviceView{ID: d.ID, Label: d.Label})
	}
	return web.NodeView{
		ID:        n.ID,
		Name:      n.Name,
		Addrs:     nonNil(n.Addrs),
		HWDelayUs: n.HWDelayUs,
		Channel:   n.Channel,
		GainDB:    n.GainDB,
		Device:       n.Device,
		AudioDevices: devs,
		Caps: web.Capabilities{
			Render:       n.Caps.Render,
			Sinks:        nonNil(n.Caps.Sinks),
			EncodeCodecs: nonNil(n.Caps.EncodeCodecs),
			DecodeCodecs: nonNil(n.Caps.DecodeCodecs),
			FEC:          nonNil(n.Caps.FEC),
			MaxRate:      n.Caps.MaxRate,
		},
	}
}

// nonNil returns s, or an empty slice when s is nil (JSON [] instead of null).
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// profileView projects a state.TransportProfile into the web.Profile view.
func profileView(p state.TransportProfile) web.Profile {
	return web.Profile{
		Codec:          p.Codec,
		FEC:            p.FEC,
		Rate:           p.Rate,
		FramesPerChunk: p.FramesPerChunk,
		FECK:           p.FECK,
		Interleave:     p.Interleave,
	}
}

// listLocalMedia reads one folder of the node's data/ tree via stream/source
// and adapts it to the web view (08 §F.1): the playable files (with data/-
// relative slash paths, so a nested selection plays unchanged) plus the
// immediate subdirectories (so the media browser can descend). rel is the
// data/-relative browse path ("" = the root); it is sanitized against
// traversal. DurationMs/Title/Artist are left zero/empty (go-mp3 gives
// rate+frame-count but no ID3 — risk Q3 MVP fallback {file,size,rate}).
func listLocalMedia(dataDir, rel string) ([]web.MediaFile, []string, error) {
	if dataDir == "" {
		return nil, nil, nil
	}
	rel = cleanRelPath(rel)
	dir := filepath.Join(dataDir, filepath.FromSlash(rel))
	infos, err := source.List(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Unknown browse folder (including a sanitized traversal attempt
			// that collapsed onto a non-existent name): a 404, not a 500.
			return nil, nil, fmt.Errorf("%w: media folder %q", web.ErrMissingOnMaster, rel)
		}
		return nil, nil, err
	}
	out := make([]web.MediaFile, 0, len(infos))
	for _, mi := range infos {
		// MVP scope: surface only .mp3 in the media list (D14). source.List already
		// includes flac/wav; filter to the playable-by-this-API set here.
		if mi.Format != "mp3" {
			continue
		}
		out = append(out, web.MediaFile{
			File:       path.Join(rel, mi.Name), // data/-relative, slash-separated
			SizeBytes:  mi.SizeBytes,
			SampleRate: mi.SampleRate,
		})
	}
	dirs, err := source.SubDirs(dir)
	if err != nil {
		return out, nil, err
	}
	return out, dirs, nil
}

// cleanRelPath canonicalizes a data/-relative browse path: slash-separated,
// no leading slash, and ".." traversal collapsed AGAINST THE ROOT (Clean of a
// rooted path cannot escape it), so a hostile path can never leave data/.
func cleanRelPath(rel string) string {
	cleaned := path.Clean("/" + strings.ReplaceAll(rel, "\\", "/"))
	return strings.TrimPrefix(cleaned, "/")
}

// statLocalMedia reports whether the data/-relative file exists as a regular
// file under dataDir (the F.2 master-side existence check for nested paths).
func statLocalMedia(dataDir, file string) bool {
	if dataDir == "" || file == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(dataDir, filepath.FromSlash(cleanRelPath(file))))
	return err == nil && info.Mode().IsRegular()
}
