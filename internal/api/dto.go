package api

import "ensemble/internal/contracts"

// --- GET /api/status (DECISIONS.md D19) ------------------------------------

// StatusResp is the GET /api/status envelope, pinned to D19. Field order/names
// are part of the wire contract the SPA (J) codes against.
type StatusResp struct {
	ID      string                 `json:"id"`
	Name    string                 `json:"name"`
	Role    string                 `json:"role"`    // "master" | "follower" | "solo"
	GroupID string                 `json:"groupId"` // derived group this node is in
	Ports   PortsResp              `json:"ports"`
	Sink    SinkStatsResp          `json:"sink"`
	Clock   ClockStat              `json:"clock"`
	Source  *contracts.SourceStats `json:"source,omitempty"` // only on an active source
}

// SinkStatsResp is the lowercase-keyed §9.1/D19 projection of
// contracts.SinkStats (which carries no JSON tags — the SPA codes against these
// exact keys). Built from a contracts.SinkStats by sinkStatsResp.
type SinkStatsResp struct {
	Played   uint64  `json:"played"`
	Silence  uint64  `json:"silence"`
	LateDrop uint64  `json:"lateDrop"`
	StaleGen uint64  `json:"staleGen"`
	Synced   bool    `json:"synced"`
	RatePPM  float64 `json:"ratePPM"`
	Buffered int     `json:"buffered"`
}

// sinkStatsResp projects the contract stats onto the pinned wire shape.
func sinkStatsResp(s contracts.SinkStats) SinkStatsResp {
	return SinkStatsResp{
		Played:   s.Played,
		Silence:  s.Silence,
		LateDrop: s.LateDrop,
		StaleGen: s.StaleGen,
		Synced:   s.Synced,
		RatePPM:  s.RatePPM,
		Buffered: s.Buffered,
	}
}

// PortsResp is the actually-bound port set (§2), surfaced as ports.* (D19).
type PortsResp struct {
	HTTP   int `json:"http"`
	Stream int `json:"stream"`
	Source int `json:"source"`
	Gossip int `json:"gossip"`
}

// --- PATCH /api/node -------------------------------------------------------
// All three fields are OPTIONAL; at least one must be present. Pointers
// distinguish "absent" (leave unchanged) from a sent zero value.
type NodePatchReq struct {
	Name          *string   `json:"name,omitempty"`
	Volume        *float64  `json:"volume,omitempty"`        // 0.0–1.0 (D35)
	OutputDelayMs *int      `json:"outputDelayMs,omitempty"` // ±500 ms (D36)
	OutputDevice  *string   `json:"outputDevice,omitempty"`  // ALSA device id (D37)
	Disabled      *[]string `json:"disabled,omitempty"`      // subset of {playback,opus,input} (D40)
}

// --- GET /api/media (§6) ---------------------------------------------------

// MediaFile is one playable file, path relative to MEDIA_DIR.
type MediaFile struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes"`
	ModTime   int64  `json:"modTime"` // unix seconds
}

// --- POST /api/follow ------------------------------------------------------
type FollowReq struct {
	Target string `json:"target"` // 32-hex node id
}

// --- POST /api/group/name --------------------------------------------------
type GroupNameReq struct {
	Group string `json:"group"` // 32-hex group id
	Name  string `json:"name"`
}

// --- POST /api/group/master (§5.2) -----------------------------------------
type MasterReq struct {
	Node string `json:"node"` // 32-hex node id to become master
}

// --- POST /api/play (§6, §9.1) ---------------------------------------------
// Body is {uri}; back-compat {file} folds to a "file:" URI. uri wins.
type PlayReq struct {
	URI  string `json:"uri,omitempty"`
	File string `json:"file,omitempty"`
}

// --- error envelope (every 4xx/5xx) ----------------------------------------
type ErrorResp struct {
	Error string `json:"error"`          // machine-stable short code
	Hint  string `json:"hint,omitempty"` // human hint (§9.1)
}
