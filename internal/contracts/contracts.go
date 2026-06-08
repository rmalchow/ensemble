// Package contracts holds the cross-piece interfaces (Backend, Sink, Clock,
// StateStore, FollowClient) and the replicated-state snapshot DTOs that more
// than one piece references. It is a leaf package — it imports only
// internal/id and the standard library — so any of those pieces can depend on
// it without forming an import cycle (S-skeleton §1 rationale).
package contracts

import (
	"context"

	"ensemble/internal/id"
)

// ---- Output backend (sink piece E owns the implementations) -----------------

// Backend is a PCM output device. Audio is raw canonical PCM (48 kHz stereo
// s16le) written as 20 ms frames (stream.FrameBytes each). Write must consume
// the whole frame or error.
type Backend interface {
	// Write plays one canonical PCM frame. Blocks at most until buffered.
	Write(frame []byte) error
	// Close stops the device and releases resources.
	Close() error
}

// OutputDevice is one enumerated ALSA playback device (§8.5, D37). ID is an
// ALSA device spec ("default" or "hw:C,D"); Desc is a human label. Enumerated
// once at startup from /proc/asound/pcm and reported per node so the UI can
// offer a selection.
type OutputDevice struct {
	ID   string `json:"id"`
	Desc string `json:"desc"`
}

// InputDevice is one enumerated capture device (D48). ID is what the capture
// tool selects ("" = system default; a PipeWire source node name for pw-record,
// or an ALSA "hw:C,D" for arecord); Desc is a human label. Enumerated once at
// startup and reported per node so the UI can pick a microphone for calibration
// or for playing an `input:` source.
type InputDevice struct {
	ID   string `json:"id"`
	Desc string `json:"desc"`
}

// DelayReporter is an OPTIONAL Backend extension (type-asserted by the rate
// servo in E, §8.5): the exact amount of queued audio between a Write and the
// speaker, in nanoseconds. alsa implements it (snd_pcm_delay); exec/null/file
// do not — the servo then falls back to backpressure inference.
type DelayReporter interface {
	DeviceDelay() (nanos int64, ok bool)
}

// Flusher is an OPTIONAL Backend extension (type-asserted by the sink): drop
// any audio queued inside the device/player when a playout session ends.
// Without it, device layers that retain their buffer across an underrun
// (PipeWire's alsa plugin, pipe players) audibly REPLAY stale audio when the
// node rejoins a stream.
type Flusher interface {
	Flush()
}

// ---- Frame sink (playout; sink piece E owns it, group H feeds it) -----------

// Sink is what the receiver/group hands decoded frames to for scheduled
// playout (§8.5). One Sink per active session; Reset on generation change.
type Sink interface {
	// Push enqueues a frame for playout. seq/pts/gen come from the wire header;
	// payload is canonical PCM. Non-blocking: late or stale-gen frames are
	// dropped+counted inside the sink, never block.
	Push(gen uint32, seq uint64, pts int64, payload []byte)
	// Reset arms the sink for a new session generation, discarding queued
	// frames from older generations and re-zeroing per-session counters.
	Reset(gen uint32)
	// Disarm cleanly ends the local playout session (group idle / session
	// stopped): discards buffered frames and stops the scheduler WITHOUT the
	// starvation-watchdog warnings. Idempotent; a later Reset re-arms.
	Disarm()
	// Stats returns a snapshot of playout counters for /api/status (§9.1).
	Stats() SinkStats
	// SetGain sets the live software volume (0.0–1.0, D35) with a one-frame
	// linear ramp; safe from any goroutine, effective on the next frame.
	SetGain(g float64)
	// SetDelayOffset sets the node's output-delay calibration (D36) in
	// nanoseconds (positive = device chain is late, write earlier). The sink
	// re-anchors playout: discards buffered frames and fires the restart hook.
	SetDelayOffset(nanos int64)
	// Close stops the playout loop and the underlying Backend.
	Close() error
}

// SinkStats is surfaced via /api/status and used by the e2e smoke test (K).
type SinkStats struct {
	Played   uint64  // frames written to the backend
	Silence  uint64  // silent frames inserted for gaps
	LateDrop uint64  // frames dropped for arriving past their deadline
	StaleGen uint64  // frames dropped for an old generation
	Synced   bool    // clock follower has a usable offset (gates playout)
	RatePPM  float64 // current rate-servo correction (0 until settled)
	Buffered int     // jitter-buffer depth, frames
}

// SourceStats is surfaced by a node running an audio source (§8.2/§9.1).
type SourceStats struct {
	Clients  int    `json:"clients"`  // current live subscribers
	Connects uint64 `json:"connects"` // total HELLO-subscribes accepted
	Restarts uint64 `json:"restarts"` // RESTART re-prime requests served
	Primes   uint64 `json:"primes"`   // burst primes sent (connect + restart)
	Released uint64 `json:"released"` // frames released (seq high-water) this session
	Parity   uint64 `json:"parity"`   // FEC parity datagrams emitted this session
}

// ---- Clock (clock piece F owns it; playout E + source H consume it) ---------

// Clock translates between local and master time. Until the follower has a
// sample, Synced/ok is false and playout must not start (§7).
type Clock interface {
	// MasterNow returns the current master-clock time in nanoseconds and
	// whether the estimate is usable (>=1 good sample).
	MasterNow() (masterNanos int64, ok bool)
	// MasterToLocal / LocalToMaster convert a specific instant using the
	// current offset. ok mirrors MasterNow.
	MasterToLocal(masterNanos int64) (localNanos int64, ok bool)
	LocalToMaster(localNanos int64) (masterNanos int64, ok bool) // D10
}

// ---- Replicated state snapshot (cluster piece C produces; API I reads) ------

// StateStore is the read side of the replicated cluster doc (§4). Writes go
// through C's own setters, NOT through this interface, so the read contract
// stays small and side-effect-free.
type StateStore interface {
	// Self returns this node's own ID.
	Self() id.ID
	// Snapshot returns an immutable, resolved view of the whole cluster.
	Snapshot() Snapshot
	// Subscribe returns a channel signaled (coalesced) on every state change.
	Subscribe() <-chan struct{}
}

// Snapshot is the resolved cluster view behind GET /api/cluster and the WS
// "cluster" event (§9.1/§9.2). Plain JSON-serializable data, no methods.
type Snapshot struct {
	Nodes  []NodeView  `json:"nodes"`
	Groups []GroupView `json:"groups"`
}

// NodeView is one node record resolved with liveness and observed addrs.
type NodeView struct {
	ID            id.ID              `json:"id"`
	Name          string             `json:"name"`
	Volume        float64            `json:"volume"`        // 0.0–1.0 software gain (D35)
	OutputDelayMs int                `json:"outputDelayMs"` // hardware latency calibration (D36)
	OutputDevice  string             `json:"outputDevice"`  // selected ALSA device id (D37); "default" by default
	OutputDevices []OutputDevice     `json:"outputDevices"` // enumerated devices on this node (D37); empty when none
	InputDevices  []InputDevice      `json:"inputDevices"`  // enumerated capture devices for calibration (D48); empty when none
	Addrs         []string           `json:"addrs"`         // self-reported CIDRs
	HTTPPort      int                `json:"httpPort"`
	StreamPort    int                `json:"streamPort"`
	SourcePort    int                `json:"sourcePort"`
	GossipPort    int                `json:"gossipPort"`
	Capabilities  Capabilities       `json:"capabilities"` // EFFECTIVE caps: probed minus disabled (D40)
	Disabled      []string           `json:"disabled"`     // operator-disabled features (D40); subset of {playback,opus,input}
	Following     id.ID              `json:"following"`    // Zero == solo master
	Observed      map[id.ID]Observed `json:"observed"`     // peerID -> last observation
	Alive         bool               `json:"alive"`        // from memberlist liveness
	LastSeenUnix  int64              `json:"lastSeen"`
	Stale         bool               `json:"stale"` // not updated recently (UI hint)
	UpdatedAt     int64              `json:"updatedAt"`
	Version       uint64             `json:"version"`
}

// Capabilities mirror §1.
type Capabilities struct {
	Playback bool     `json:"playback"`
	Codecs   []string `json:"codecs"`   // ["pcm"] (+ "opus" when libopus loads, D32/D33)
	Backends []string `json:"backends"` // sink backends usable on this host (§8.5)
	Sources  []string `json:"sources"`  // media-source schemes (§6.1)
	Formats  []string `json:"formats"`  // ["wav","mp3","flac"]
}

// Observed is one observed-address entry (§3.1).
type Observed struct {
	IP           string `json:"ip"`
	LastSeenUnix int64  `json:"lastSeen"`
}

// GroupView is one derived group (§5) with its name + playback status.
//
// ID is the MASTER's node id (D42): playback + settings records are keyed by it,
// so membership churn no longer orphans them. A solo group's ID equals the node's
// own id.
//
// Name is the resolved display label: the EXPLICIT override (the XOR-of-members-
// keyed name map, §4) when one exists for the current member set, else a DERIVED
// label computed server-side from the member NAMES (DeriveGroups). NameIsDerived
// reports which: true = no override, the derived label; false = an operator
// override.
type GroupView struct {
	ID            id.ID         `json:"id"`          // master node id (D42)
	Name          string        `json:"name"`        // override, else derived label
	NameIsDerived bool          `json:"nameDerived"` // true when Name is the derived label (no override)
	Master        id.ID         `json:"master"`
	Members       []id.ID       `json:"members"`
	Playback      Playback      `json:"playback"`
	Settings      GroupSettings `json:"settings"`
}

// Playback mirrors the replicated playback-status record (§4), written only by
// the group's master. Source stats ride along.
type Playback struct {
	State       string      `json:"state"` // "idle" | "playing"
	URI         string      `json:"uri"`   // media-source URI (§6)
	StartedUnix int64       `json:"startedAt"`
	PositionSec float64     `json:"positionSec"`
	Codec       string      `json:"codec"`     // "pcm" | "opus"
	Transport   string      `json:"transport"` // "udp" | "tcp"
	Source      SourceStats `json:"source"`    // master's source stats (§8.2)
}

// GroupSettings mirrors the per-group settings record (§8.3/§8.4/§9.1).
type GroupSettings struct {
	Codec     string `json:"codec"`     // default "pcm"
	Transport string `json:"transport"` // default "udp"
	BufferMs  int    `json:"bufferMs"`  // default 150
}

// Defaults for group settings (single source of truth, §8.5).
//
// DefaultCodec is opus (§8.3): a 20 ms opus packet at 128 kbps is ~320 B, so a
// stream datagram (header+payload ≈ 344 B) stays under one MTU and never IP-
// fragments — raw PCM is 3864 B/frame and fragments into ~3 packets, which loses
// catastrophically on lossy Wi-Fi. Opus is the default; a group whose members
// don't all support opus is transparently downgraded to pcm at play (group.Play).
const (
	DefaultCodec     = "opus"
	DefaultTransport = "udp"
	DefaultBufferMs  = 150
	DefaultLeadMs    = 50 // source release lead over the clock (§8.2)
)

// FollowClient is the small HTTP client the group piece (H) uses to drive
// takeover (§5.2). The API piece (I) injects a concrete implementation;
// defined here to avoid an H→I import cycle.
type FollowClient interface {
	Follow(ctx context.Context, peer id.ID, target id.ID) error
	Unfollow(ctx context.Context, peer id.ID) error
}
