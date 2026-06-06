package web

// Web-owned JSON view types (the seam's vocabulary). They are flat,
// JSON-friendly projections of internal types the web layer must not import
// (state.ConfigDoc, stream status, cluster discovery). cmd adapts the internal
// types into these. Codec/FEC are STRING enums on the wire (README §6.5):
// "pcm"|"opus" and "none"|"xorParity"|"duplicate"; integer ids live only at the
// wire layer, never here.

// ConfigView is the web projection of the replicated ConfigDoc.
type ConfigView struct {
	Version uint64      `json:"version"`
	Nodes   []NodeView  `json:"nodes"`
	Groups  []GroupView `json:"groups"`
}

// ClusterInfoView is the GET /api/v1/cluster/info body (the dashboard's cluster
// header). It carries the cluster's public identity only — never ClusterSecrets
// (the CA private key / shared secret are never projected, doc 09 §2.8). Version
// mirrors the ConfigDoc version so the SPA can show staleness; it is also set as
// the ETag.
type ClusterInfoView struct {
	ClusterName   string `json:"clusterName"`
	CAFingerprint string `json:"caFingerprint"`
	Created       string `json:"created"` // RFC3339
	NodeCount     int    `json:"nodeCount"`
	Version       uint64 `json:"version"`
}

// NodeView is one node in the ConfigView.
type NodeView struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Addrs     []string `json:"addrs"`
	HWDelayUs int      `json:"hwDelayUs"`
	Channel   string   `json:"channel"` // "stereo" | "left" | "right"
	GainDB    float64  `json:"gainDb"`
	Device    string   `json:"device"` // audio-output device override ("" = auto)
	// AudioDevices is the node's self-probed playback device list (the UI's
	// selectable choices for Device). Always non-nil.
	AudioDevices []AudioDeviceView `json:"audioDevices"`
	// Caps mirrors README §6.5 Capabilities as a structured object, not a flat
	// array.
	Caps Capabilities `json:"caps"`
}

// AudioDeviceView is one selectable playback device on a node.
type AudioDeviceView struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

// MemberView is one cluster-members row (GET /api/v1/discovery members[]): the
// NodeView record joined with gossip liveness and, when the member is live, its
// observed control endpoint (host:port) so the UI shows reachable addresses.
type MemberView struct {
	NodeView
	Online   bool `json:"online"`
	IsMaster bool `json:"isMaster,omitempty"`
}

// NodeDetailView is the full GET /api/v1/nodes/{id} projection (08 §D.2): the
// NodeView record joined with the cert/liveness/group facts the Node-detail
// screen renders. The embedded NodeView fields are inlined in the JSON body.
type NodeDetailView struct {
	NodeView
	Fingerprint    string `json:"fingerprint,omitempty"` // "sha256:<hex>" of the node cert
	CertSignedByCA bool   `json:"certSignedByCa"`
	Online         bool   `json:"online"`            // gossip liveness (self is always online)
	GroupID        string `json:"groupId,omitempty"` // the group this node belongs to
	IsMaster       bool   `json:"isMaster"`          // elected master of its group
}

// GroupView is one group in the ConfigView.
type GroupView struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	MemberNodeIDs []string `json:"memberNodeIds"`
	Profile       Profile  `json:"profile"`
	Media         Media    `json:"media"`
	Playing       bool     `json:"playing"`
}

// Profile is a group's stream profile (README §6.5).
type Profile struct {
	Codec          string `json:"codec"` // "pcm" | "opus"
	FEC            string `json:"fec"`   // "none" | "xorParity" | "duplicate"
	Rate           int    `json:"rate"`
	FramesPerChunk int    `json:"framesPerChunk"`
	FECK           int    `json:"fecK"`
	Interleave     int    `json:"interleave"`
}

// Media is a group's current media selection.
type Media struct {
	File string `json:"file"`
	Loop bool   `json:"loop"`
}

// Capabilities mirrors README §6.5 Capabilities as a structured object.
type Capabilities struct {
	Render       bool     `json:"render"`
	Sinks        []string `json:"sinks"`
	EncodeCodecs []string `json:"encode"`
	DecodeCodecs []string `json:"decode"`
	FEC          []string `json:"fec"`
	MaxRate      int      `json:"maxRate"`
}

// Discovered is a node seen on the LAN but not (yet) adopted into this cluster.
type Discovered struct {
	NodeID          string   `json:"nodeId"`
	Name            string   `json:"name"`
	Addrs           []string `json:"addrs"`
	Fingerprint     string   `json:"fingerprint"`
	State           string   `json:"state"` // "uninitialized" | "foreign"
	SoftwareVersion string   `json:"softwareVersion"`
}

// TranscodeStatus is the per-group stream/transcode status the UI surfaces. It
// is web-owned (populated via Deps.Transcodes) so web never imports stream/*.
type TranscodeStatus struct {
	GroupID string `json:"groupId"`
	Playing bool   `json:"playing"`
	Codec   string `json:"codec"`
	Status  string `json:"status"` // "idle" | "streaming" | "error"
	Err     string `json:"err,omitempty"`
}

// NodePatch is the partial node config update (PATCH semantics): a nil pointer
// leaves that field unchanged.
type NodePatch struct {
	Name      *string  `json:"name,omitempty"`
	Channel   *string  `json:"channel,omitempty"` // "stereo" | "left" | "right"
	HWDelayUs *int     `json:"hwDelayUs,omitempty"`
	GainDB    *float64 `json:"gainDb,omitempty"`
	Device    *string  `json:"device,omitempty"` // "" clears back to auto
}

// CalibrateSel selects calibration targets: exactly one of GroupID / NodeIDs.
type CalibrateSel struct {
	GroupID string   `json:"groupId,omitempty"`
	NodeIDs []string `json:"nodeIds,omitempty"`
}

// MediaFile is one playable item in a node's data/ folder (08 §F.1). It is the
// web projection of stream/source.MediaInfo; cmd adapts that into this so web
// never imports stream/*. Title/Artist are best-effort (go-mp3 gives no ID3, so
// they are usually empty — P4.9 risk Q3); the MVP fields are file/sizeBytes plus
// the header-derived sampleRate/durationMs when cheap to read.
type MediaFile struct {
	File       string `json:"file"`
	Title      string `json:"title,omitempty"`
	Artist     string `json:"artist,omitempty"`
	DurationMs int    `json:"durationMs,omitempty"`
	SizeBytes  int64  `json:"sizeBytes"`
	SampleRate int    `json:"sampleRate,omitempty"`
}

// GroupStatus is the live per-group telemetry returned by GET /groups/{id}/status
// (08 §G.2). It is a fan-out read aggregation: the master is authoritative for
// MasterNodeID/Profile/StreamGen/Playing; each member reports its own live sync
// fields. It is web-owned so web never imports group/audio.
type GroupStatus struct {
	GroupID      string         `json:"groupId"`
	MasterNodeID string         `json:"masterNodeId"`
	Profile      Profile        `json:"profile"`
	StreamGen    uint64         `json:"streamGen"`
	Playing      bool           `json:"playing"`
	Members      []MemberStatus `json:"members"`
}

// MemberStatus is one member's live sync state within a GroupStatus (08 §G.2).
// A member that is offline / not reporting carries Online=false and zeroed live
// fields (the handler does not treat a single down member as a top-level error).
type MemberStatus struct {
	NodeID       string  `json:"nodeId"`
	SyncErrorUs  int64   `json:"syncErrorUs"`
	OffsetUs     int64   `json:"offsetUs"`
	DriftRatio   float64 `json:"driftRatio"`
	Underruns    int64   `json:"underruns"`
	ClockQuality string  `json:"clockQuality"` // "good" | "fair" | "poor"
	Online       bool    `json:"online"`
}

// NodeStatus is the web view of this node's current role and sync state. It is
// a flat JSON mirror of the node's internal status (which web must not import);
// cmd supplies it via Deps.Status, flattening internal types (e.g. an Offset
// time.Duration to OffsetMs).
type NodeStatus struct {
	Role     string  `json:"role"` // "master" | "follower" | "solo" | "starting"
	MasterID string  `json:"masterId"`
	Members  int     `json:"members"`
	OffsetMs int64   `json:"offsetMs"`
	HaveSync bool    `json:"haveSync"`
	ErrorSec float64 `json:"errorSec"`
}

// SetupStatus is the payload of GET /api/v1/setup: whether this node is
// configured (has joined a cluster) plus its identity. The frontend reads it on
// load to decide between the setup wizard (unconfigured) and the app.
type SetupStatus struct {
	Configured bool   `json:"configured"`
	Name       string `json:"name"`
	NodeID     string `json:"nodeID"`
}

// SetupResult is the web-owned projection of the genesis act (POST /api/v1/setup,
// 08 §B.1). cmd's Setup closure returns it; the handler renders the B.1 body from
// it. Created is RFC3339. It carries no secret material (the CA private key and
// gossip key stay inside cmd/pki/state, never on the wire).
type SetupResult struct {
	ClusterName   string
	CAFingerprint string
	Created       string // RFC3339
	NodeID        string
	NodeName      string
	Version       uint64
}

// StatusView is the web projection of GET /api/v1/status (08 §G.1). It is flat
// and JSON-friendly so web never imports the engine's status type; cmd fills it
// via Deps.StatusView. Initialized=false drives the SPA to the Setup Wizard
// (P1.3 §4.7) — it is the one field readable on a raw, uninitialized node.
type StatusView struct {
	NodeID    string `json:"nodeId"`
	Online    bool   `json:"online"`
	UptimeSec int64  `json:"uptimeSec"`
	Sink      struct {
		Kind     string `json:"kind"`
		Rate     int    `json:"rate"`
		Channels int    `json:"channels"`
		Running  bool   `json:"running"`
	} `json:"sink"`
	Group struct {
		ID   string `json:"id"`
		Role string `json:"role"`
	} `json:"group"`
	Clock struct {
		Synced   bool   `json:"synced"`
		OffsetUs int    `json:"offsetUs"`
		Quality  string `json:"quality"`
	} `json:"clock"`
	ConfigVersion uint64 `json:"configVersion"`
	Initialized   bool   `json:"initialized"` // false => SPA shows the Setup Wizard
}

// BootstrapInfo is the GET /bootstrap/info body (08 §A.1): the uninitialized
// node's identity, self-signed cert fingerprint (to pin before sending the PIN),
// init state, software/protocol versions, and probed capabilities. It is served
// OUTSIDE mTLS on an uninitialized node and closes (403) once a member.
type BootstrapInfo struct {
	NodeID          string       `json:"nodeId"`
	Name            string       `json:"name"`
	Fingerprint     string       `json:"fingerprint"` // "sha256:<hex>" of the self-signed cert DER
	State           string       `json:"state"`       // "uninitialized" | "foreign" | "member"
	SoftwareVersion string       `json:"softwareVersion"`
	ProtocolEpoch   int          `json:"protocolEpoch"`
	Caps            Capabilities `json:"caps"`
}

// StateSnapshot is the full UI state pushed over the websocket at 3 Hz.
type StateSnapshot struct {
	T       string            `json:"t"` // always "state"
	Self    string            `json:"self"`
	Master  string            `json:"master"`
	Status  NodeStatus        `json:"status"`
	Config  ConfigView        `json:"config"`
	Streams []TranscodeStatus `json:"streams,omitempty"`
}
