// Package state holds the replicated, versioned ConfigDoc: gossip-merged,
// last-writer-wins, persisted to disk. The one cross-node document and the
// merge/persistence engine. Near-leaf: imports only the Go stdlib.
//
// Concurrency model: optimistic, last-writer-wins (07 §4, A.6). A logical
// writer (normally the admin via the web API, behind If-Match) submits the
// version it based an edit on; Apply rejects a stale version with ErrConflict
// so the client refetches the authoritative snapshot. Merge reconciles two
// replicas during memberlist anti-entropy gossip by picking the higher Version
// (tie broken by the gossip sender id) PLUS a grow-only union of the RevokedSet
// on every merge regardless of who wins the doc (A.6, 07 §4.3) — no Raft, no
// vector clocks; acceptable because config edits are rare and human-driven.
//
// The doc carries the plaintext CA private key and credential verifiers
// (ClusterSecrets, AuthConfig), so it is persisted 0600 (07 §5.3, D18) and
// web must never serialize Secrets to a read endpoint (09 §2.8 redaction).
//
// The authentication credential types (AuthConfig, Argon2id, APIKey) are
// declared in credentials.go (P1.2) and embedded here unchanged.
package state

// ConfigDoc is the single replicated, version-stamped cluster document (07 §2).
// Field order below is the canonical marshal order. Version is the optimistic
// concurrency stamp, bumped on every accepted Apply.
type ConfigDoc struct {
	Version   uint64         `json:"version"`
	Cluster   ClusterInfo    `json:"cluster"`
	Secrets   ClusterSecrets `json:"secrets"`
	Auth      AuthConfig     `json:"auth"`
	Nodes     []NodeRecord   `json:"nodes"`
	Groups    []GroupRecord  `json:"groups"`
	Revoked   RevokedSet     `json:"revoked"`
	UpdatedBy string         `json:"updatedBy"`           // node id that produced this version (advisory provenance)
	UpdatedAt string         `json:"updatedAt,omitempty"` // RFC3339; human/debug only, never a merge input (07 §2.1)
}

// ClusterInfo is the cluster's public identity and the self-signed CA cert that
// anchors every node certificate (03 §3, 07 §2.2).
type ClusterInfo struct {
	Name        string `json:"name"`
	CACertPEM   string `json:"caCertPem"`   // PEM-encoded cluster CA certificate (public)
	Created     string `json:"created"`     // RFC3339
	Fingerprint string `json:"fingerprint"` // SHA-256 of the CA cert DER, hex
}

// ClusterSecrets holds the replicated secret material (07 §2.2, D18). The CA
// private key lives here so any admitted node can mint adoption certs; the doc
// is therefore persisted 0600 and never served to the UI.
type ClusterSecrets struct {
	CAKeyPEM     string `json:"caKeyPem"`               // PEM-encoded CA private key (PLAINTEXT secret)
	SharedSecret string `json:"sharedSecret,omitempty"` // optional cluster pre-shared secret (HKDF base)
}

// NodeRecord is one cluster member's replicated record (07 §2.5). Addrs feeds
// the source-IP allowlist derivation (07 §3, computed in internal/allowlist).
type NodeRecord struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	CertPEM   string       `json:"certPem"`
	Addrs     []string     `json:"addrs"`
	HWDelayUs int          `json:"hwDelayUs"`
	Channel   string       `json:"channel"` // "stereo"|"left"|"right"
	GainDB    float64      `json:"gainDb"`
	// Device is the node's persisted audio-output device override (e.g. an ALSA
	// "hw:1" or an exec sink device); "" = the backend default / the node-local
	// --device flag. Per-node config, gossiped like channel/gain (07 §2).
	Device string `json:"device,omitempty"`
	// AudioDevices is the node's SELF-probed playback device list (a fact, like
	// Addrs — each node publishes its own and gossips it so any member's UI can
	// offer the choices). Refreshed by the owning node's role loop.
	AudioDevices []AudioDevice `json:"audioDevices,omitempty"`
	Caps      Capabilities `json:"caps"`
	// RenderAutoOff marks that Caps.Render was AUTO-disabled because the owning
	// node found no usable audio sink (06 §1.5 last-resort control-only). When a
	// sink becomes usable again the node flips Render back on and clears this —
	// an operator's explicit Render=false (flag unset) is never overridden.
	RenderAutoOff bool   `json:"renderAutoOff,omitempty"`
	LastSeen      string `json:"lastSeen,omitempty"` // RFC3339; best-effort hint
}

// AudioDevice is one enumerated playback device on a node (ID is the string
// the sink registry opens, e.g. "hw:1,0"; Label the human stream name).
type AudioDevice struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

// Capabilities is a node's probed render/encode/decode profile (06, P2.6).
type Capabilities struct {
	Render       bool     `json:"render"`
	Sinks        []string `json:"sinks"`  // "alsa","exec:aplay",...
	EncodeCodecs []string `json:"encode"` // "pcm","opus"
	DecodeCodecs []string `json:"decode"` // "pcm","opus"
	FEC          []string `json:"fec"`    // "none","xorParity","duplicate"
	MaxRate      int      `json:"maxRate"`
}

// GroupRecord is one synchronized playback group (02 §5, 04, 07 §2.6).
type GroupRecord struct {
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	MemberNodeIDs []string         `json:"memberNodeIds"`
	Media         MediaSelection   `json:"media"`
	Playing       bool             `json:"playing"`
	Profile       TransportProfile `json:"profile"`
	MasterHint    string           `json:"masterHint,omitempty"`
}

// MediaSelection is the media a group is playing (value struct).
type MediaSelection struct {
	File string `json:"file"`
	Loop bool   `json:"loop"`
}

// TransportProfile is the negotiated audio transport for a group (04, 05, A.12).
// All enums are string-typed here; integer CodecID/FECID live only at the §6.4
// wire layer and never appear in this package (07 §2.4).
type TransportProfile struct {
	Codec          string `json:"codec"`          // "pcm"|"opus"
	FEC            string `json:"fec"`            // "none"|"xorParity"|"duplicate"
	Rate           int    `json:"rate"`           // Hz (canonical 48000)
	FramesPerChunk int    `json:"framesPerChunk"` // default 480 (10 ms @ 48k)
	FECK           int    `json:"fecK"`           // XOR k (8); 0 if FEC "none"
	Interleave     int    `json:"interleave"`     // interleave depth D (4); 0/1 = none
	Transport      string `json:"transport"`      // "udp"|"tcp"
}

// RevokedSet is the grow-only set of revoked certificates (07 §4.3, A.6). It is
// unioned (never reduced) on every Merge so a forgotten/taken-over node can
// never resurrect via a stale replica. Compaction is an out-of-band admin op.
type RevokedSet struct {
	Entries []RevokedCert `json:"entries"`
}

// RevokedCert is one revoked node certificate. Fingerprint is the set identity.
type RevokedCert struct {
	Fingerprint string `json:"fingerprint"` // SHA-256 of cert DER, hex
	NodeID      string `json:"nodeId,omitempty"`
	Reason      string `json:"reason"` // "forget"|"takeover"|"rotate"
	At          string `json:"at"`     // RFC3339
}
