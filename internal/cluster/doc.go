package cluster

import (
	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// Document is the whole replicated state (§4). In-memory only; never persisted.
type Document struct {
	Nodes    map[id.ID]*NodeRecord          `json:"nodes"`
	Groups   map[id.ID]*GroupNameRecord     `json:"groups"`   // group names map
	Playback map[id.ID]*PlaybackRecord      `json:"playback"` // per-group playback
	Settings map[id.ID]*GroupSettingsRecord `json:"settings"` // per-group settings
}

// newDocument returns an empty document with initialised maps.
func newDocument() *Document {
	return &Document{
		Nodes:    map[id.ID]*NodeRecord{},
		Groups:   map[id.ID]*GroupNameRecord{},
		Playback: map[id.ID]*PlaybackRecord{},
		Settings: map[id.ID]*GroupSettingsRecord{},
	}
}

// NodeRecord — owned and only ever written by the node it describes (§4).
type NodeRecord struct {
	ID            id.ID                    `json:"id"`
	Name          string                   `json:"name"`
	Volume        float64                  `json:"volume"`        // playback gain 0.0–1.0 (D35)
	OutputDelayMs int                      `json:"outputDelayMs"` // hardware latency calibration, ±500 (D36)
	OutputDevice  string                   `json:"outputDevice"`  // selected ALSA device id (D37)
	OutputDevices []contracts.OutputDevice `json:"outputDevices"` // enumerated devices on this node (D37)
	Addrs         []string                 `json:"addrs"`         // self-reported CIDRs (§3.1)
	HTTPPort      int                      `json:"httpPort"`
	StreamPort    int                      `json:"streamPort"`
	SourcePort    int                      `json:"sourcePort"`
	GossipPort    int                      `json:"gossipPort"`
	Caps          contracts.Capabilities   `json:"caps"`      // PROBED caps (host reality); effective = caps − disabled (D40)
	Disabled      []string                 `json:"disabled"`  // operator-disabled features (D40); subset of {playback,opus,input}
	Following     id.ID                    `json:"following"` // id.Zero == solo
	Observed      map[id.ID]obsEntry       `json:"observed"`  // peerID -> {ip,lastSeen}
	Version       uint64                   `json:"version"`
	UpdatedAt     int64                    `json:"updatedAt"` // unix seconds, LWW timestamp
}

// obsEntry is one observed-IP record inside a NodeRecord (§3.1).
type obsEntry struct {
	IP           string `json:"ip"`
	LastSeenUnix int64  `json:"lastSeen"`
}

// GroupNameRecord — group names map value (§4, LWW, any writer).
type GroupNameRecord struct {
	Name      string `json:"name"`
	Version   uint64 `json:"version"`
	UpdatedAt int64  `json:"updatedAt"`
	Writer    id.ID  `json:"writer"`
}

// PlaybackRecord — per-group playback status (§4, written by group master).
type PlaybackRecord struct {
	State       string                `json:"state"` // "idle" | "playing"
	URI         string                `json:"uri"`
	StartedUnix int64                 `json:"startedAt"`
	PositionSec float64               `json:"positionSec"`
	Codec       string                `json:"codec"`
	Transport   string                `json:"transport"`
	Source      contracts.SourceStats `json:"source"`
	Version     uint64                `json:"version"`
	UpdatedAt   int64                 `json:"updatedAt"`
	Writer      id.ID                 `json:"writer"`
}

// GroupSettingsRecord — per-group codec/transport/bufferMs (§8.3/§8.4, LWW).
type GroupSettingsRecord struct {
	Codec     string `json:"codec"`
	Transport string `json:"transport"`
	BufferMs  int    `json:"bufferMs"`
	Version   uint64 `json:"version"`
	UpdatedAt int64  `json:"updatedAt"`
	Writer    id.ID  `json:"writer"`
}

// idLess reports whether a < b lexicographically over the 16 ID bytes.
func idLess(a, b id.ID) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// versionedLater reports whether (bVer, bWriter) wins over (aVer, aWriter) under
// the LWW rule: higher Version wins; on equal Version the larger writer id
// (lexicographic) wins. Equal version AND equal writer → not later (idempotent).
func versionedLater(aVer uint64, aWriter id.ID, bVer uint64, bWriter id.ID) bool {
	if bVer != aVer {
		return bVer > aVer
	}
	return idLess(aWriter, bWriter)
}

// mergeNode merges remote node record r into the document. The node's own id is
// the writer. Returns true if the local doc changed. Our own record is never
// overwritten by a remote merge (§4: sole-writer) — see SetName monotonicity;
// reconciliation at Start handles a stale higher-versioned peer copy.
func (d *Document) mergeNode(self id.ID, r *NodeRecord) bool {
	if r == nil {
		return false
	}
	if r.ID == self {
		return false
	}
	cur, ok := d.Nodes[r.ID]
	if ok && !versionedLater(cur.Version, cur.ID, r.Version, r.ID) {
		return false
	}
	d.Nodes[r.ID] = cloneNode(r)
	return true
}

func (d *Document) mergeGroupName(g id.ID, r *GroupNameRecord) bool {
	if r == nil {
		return false
	}
	cur, ok := d.Groups[g]
	if ok && !versionedLater(cur.Version, cur.Writer, r.Version, r.Writer) {
		return false
	}
	cp := *r
	d.Groups[g] = &cp
	return true
}

func (d *Document) mergePlayback(g id.ID, r *PlaybackRecord) bool {
	if r == nil {
		return false
	}
	cur, ok := d.Playback[g]
	if ok && !versionedLater(cur.Version, cur.Writer, r.Version, r.Writer) {
		return false
	}
	cp := *r
	d.Playback[g] = &cp
	return true
}

func (d *Document) mergeSettings(g id.ID, r *GroupSettingsRecord) bool {
	if r == nil {
		return false
	}
	cur, ok := d.Settings[g]
	if ok && !versionedLater(cur.Version, cur.Writer, r.Version, r.Writer) {
		return false
	}
	cp := *r
	d.Settings[g] = &cp
	return true
}

// mergeAll merges an entire remote Document (push/pull path). Returns true if
// anything changed locally.
func (d *Document) mergeAll(self id.ID, remote *Document) bool {
	changed, _ := d.mergeAllTracked(self, remote)
	return changed
}

// mergeAllTracked is mergeAll that additionally reports whether the persisted
// LOOKUP table changed (D41/D47): the group override-NAMES map (any key) and this
// node's OWN settings record (key == self). Only those records are written to
// cluster.json, so only their change triggers a save; peers' settings records are
// master-keyed live state and are NOT persisted.
func (d *Document) mergeAllTracked(self id.ID, remote *Document) (changed, lookupChanged bool) {
	if remote == nil {
		return false, false
	}
	for _, r := range remote.Nodes {
		if d.mergeNode(self, r) {
			changed = true
		}
	}
	for g, r := range remote.Groups {
		if d.mergeGroupName(g, r) {
			changed = true
			lookupChanged = true
		}
	}
	for g, r := range remote.Playback {
		if d.mergePlayback(g, r) {
			changed = true
		}
	}
	for g, r := range remote.Settings {
		if d.mergeSettings(g, r) {
			changed = true
			if g == self {
				lookupChanged = true // D47: only our own settings record persists
			}
		}
	}
	return changed, lookupChanged
}

// cloneNode deep-copies a NodeRecord (its Addrs slice and Observed map).
func cloneNode(r *NodeRecord) *NodeRecord {
	cp := *r
	if r.Addrs != nil {
		cp.Addrs = append([]string(nil), r.Addrs...)
	}
	if r.OutputDevices != nil {
		cp.OutputDevices = append([]contracts.OutputDevice(nil), r.OutputDevices...)
	}
	if r.Disabled != nil {
		cp.Disabled = append([]string(nil), r.Disabled...)
	}
	if r.Caps.Codecs != nil {
		cp.Caps.Codecs = append([]string(nil), r.Caps.Codecs...)
	}
	if r.Caps.Backends != nil {
		cp.Caps.Backends = append([]string(nil), r.Caps.Backends...)
	}
	if r.Caps.Sources != nil {
		cp.Caps.Sources = append([]string(nil), r.Caps.Sources...)
	}
	if r.Caps.Formats != nil {
		cp.Caps.Formats = append([]string(nil), r.Caps.Formats...)
	}
	if r.Observed != nil {
		cp.Observed = make(map[id.ID]obsEntry, len(r.Observed))
		for k, v := range r.Observed {
			cp.Observed[k] = v
		}
	}
	return &cp
}

// clone deep-copies the document (for Snapshot and LocalState encoding).
func (d *Document) clone() *Document {
	out := newDocument()
	for k, v := range d.Nodes {
		out.Nodes[k] = cloneNode(v)
	}
	for k, v := range d.Groups {
		cp := *v
		out.Groups[k] = &cp
	}
	for k, v := range d.Playback {
		cp := *v
		out.Playback[k] = &cp
	}
	for k, v := range d.Settings {
		cp := *v
		out.Settings[k] = &cp
	}
	return out
}

// purge deletes any record older than maxAgeUnix, except self's node record and
// any node currently alive. Returns true if anything was removed.
//
// D41: the group NAMES and SETTINGS maps are the persisted lookup table and are
// exempt from the purge entirely — kept indefinitely so a node that rejoins (or
// re-forms a specific member combination) still knows every group name/setting it
// ever saw. Only node records and playback status are aged out.
func (d *Document) purge(self id.ID, maxAgeUnix int64, alive map[id.ID]bool) bool {
	removed := false
	for k, v := range d.Nodes {
		if k == self || alive[k] {
			continue
		}
		if v.UpdatedAt < maxAgeUnix {
			delete(d.Nodes, k)
			removed = true
		}
	}
	for k, v := range d.Playback {
		if v.UpdatedAt < maxAgeUnix {
			delete(d.Playback, k)
			removed = true
		}
	}
	return removed
}
