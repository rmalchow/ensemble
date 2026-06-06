package daemon

// project.go holds the state.ConfigDoc -> web view-type projections the media/
// status Deps closures need (08 §0.7 / §G.2 field names). They live in daemon
// (the bridge) so web stays decoupled from state's concrete type. The
// ConfigDoc.Secrets (ClusterSecrets: CA private key + shared secret) is NEVER
// projected — web read endpoints must not serve the CA key (doc 01 §2 / 09 §2.8
// redaction); configView simply omits the field, so it cannot leak.

import (
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/source"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/web"
)

// configView projects a state.ConfigDoc into the web.ConfigView, REDACTING
// Secrets (the CA private key / shared secret are never present in a view type).
// It is the single projection used by every read closure so the redaction is in
// one place.
func configView(doc state.ConfigDoc) web.ConfigView {
	v := web.ConfigView{Version: doc.Version}
	for _, n := range doc.Nodes {
		v.Nodes = append(v.Nodes, web.NodeView{
			ID:        n.ID,
			Name:      n.Name,
			Addrs:     n.Addrs,
			HWDelayUs: n.HWDelayUs,
			Channel:   n.Channel,
			GainDB:    n.GainDB,
			Caps: web.Capabilities{
				Render:       n.Caps.Render,
				Sinks:        n.Caps.Sinks,
				EncodeCodecs: n.Caps.EncodeCodecs,
				DecodeCodecs: n.Caps.DecodeCodecs,
				FEC:          n.Caps.FEC,
				MaxRate:      n.Caps.MaxRate,
			},
		})
	}
	for _, g := range doc.Groups {
		v.Groups = append(v.Groups, web.GroupView{
			ID:            g.ID,
			Name:          g.Name,
			MemberNodeIDs: g.MemberNodeIDs,
			Profile:       profileView(g.Profile),
			Media:         web.Media{File: g.Media.File, Loop: g.Media.Loop},
			Playing:       g.Playing,
		})
	}
	return v
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

// listLocalMedia reads the node's data/ folder via stream/source.List and adapts
// each item to web.MediaFile (08 §F.1). DurationMs/Title/Artist are left zero/
// empty (go-mp3 gives rate+frame-count but no ID3; the cheap header probe in
// source.List reads only rate/channels — risk Q3 MVP fallback {file,size,rate}).
func listLocalMedia(dataDir string) ([]web.MediaFile, error) {
	if dataDir == "" {
		return nil, nil
	}
	infos, err := source.List(dataDir)
	if err != nil {
		return nil, err
	}
	out := make([]web.MediaFile, 0, len(infos))
	for _, mi := range infos {
		// MVP scope: surface only .mp3 in the media list (D14). source.List already
		// includes flac/wav; filter to the playable-by-this-API set here.
		if mi.Format != "mp3" {
			continue
		}
		out = append(out, web.MediaFile{
			File:       mi.Name,
			SizeBytes:  mi.SizeBytes,
			SampleRate: mi.SampleRate,
		})
	}
	return out, nil
}
