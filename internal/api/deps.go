package api

import (
	"context"
	"net/netip"
	"time"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// Cluster is the subset of the cluster store (piece C) the API depends on.
// C's concrete *cluster.Cluster satisfies this. Reads come from the embedded
// StateStore; the extra methods are C-owned writes + address resolution.
type Cluster interface {
	contracts.StateStore // Self() id.ID, Snapshot() contracts.Snapshot, Subscribe() <-chan struct{}

	// SetName renames THIS node (PATCH /api/node). Bumps version, broadcasts.
	SetName(name string)
	// SetVolume sets THIS node's playback gain (PATCH /api/node {volume}, D35).
	SetVolume(v float64)
	// SetOutputDelayMs sets THIS node's output-delay calibration (D36).
	SetOutputDelayMs(ms int)
	// SetOutputDevice sets THIS node's selected ALSA output device (D37).
	SetOutputDevice(device string)
	// SetDisabled sets THIS node's operator-disabled feature list (D40).
	SetDisabled(disabled []string)
	// Observe records that we received traffic from peer at ip (§3.1).
	Observe(peer id.ID, ip netip.Addr)
	// DialCandidates returns dial IPs for peer, ordered best-first per §3.1.
	// The caller appends the peer's HTTP port from the snapshot.
	DialCandidates(peer id.ID) []netip.Addr
}

// Group is the subset of the group engine (piece H) the API depends on. Every
// method is a mutation the spec routes to "this node" or "the master". H's
// concrete *group.Engine satisfies this. Each returns a typed error the handler
// maps to an HTTP status + JSON error body (§4).
type Group interface {
	// Follow makes THIS node follow target (§5.1).
	Follow(ctx context.Context, target id.ID) error
	// Unfollow makes THIS node solo master (§5.1).
	Unfollow(ctx context.Context) error
	// MakeMaster runs takeover so node becomes master of its group (§5.2).
	MakeMaster(ctx context.Context, node id.ID) error
	// NameGroup sets a group's display name (LWW, any node, §4/§9.1).
	NameGroup(ctx context.Context, group id.ID, name string) error
	// Play starts playback of a media-source URI on THIS node's group (master
	// only, §6/§6.1).
	Play(ctx context.Context, uri string) error
	// Stop stops THIS node's group playback; master only.
	Stop(ctx context.Context) error
	// Pause freezes THIS node's group playback; master only (D39).
	Pause(ctx context.Context) error
	// Resume un-freezes THIS node's paused group playback; master only (D39).
	Resume(ctx context.Context) error
	// Settings returns this node's group's settings (GET /api/group/settings).
	Settings() contracts.GroupSettings
	// SetSettings updates this node's group's settings; master only (POST).
	// Applies live via RECONFIG (§8.7, D23).
	SetSettings(ctx context.Context, s contracts.GroupSettings) error
}

// Media lists this node's local playable files (§6). Injected as an interface
// so the API need not import the scanner concretely. The package's own
// fsLister (media.go) satisfies it.
type Media interface {
	List() ([]MediaFile, error)
}

// NodeConfig is the on-disk persistence side of PATCH /api/node (§9.1). Piece A
// (config) owns it; *config.Config satisfies it. The handler persists FIRST,
// then replicates via the Cluster setters and applies live via SinkControl.
type NodeConfig interface {
	Rename(name string) error
	SetVolume(v float64) error           // D35
	SetOutputDelayMs(ms int) error       // D36
	SetOutputDevice(d string) error      // D37
	SetDisabled(disabled []string) error // D40
}

// SinkControl is the live-apply side of PATCH /api/node for volume/output-delay
// (§8.5, D35/D36). Piece E (sink) owns it; the local sink satisfies it. A nil
// SinkControl makes the live-apply step a no-op (persistence + replication still
// happen).
type SinkControl interface {
	SetGain(g float64)          // D35: g in [0.0, 1.0]
	SetDelayOffset(nanos int64) // D36: outputDelayMs converted to ns
	// TestTone plays a short local tone through the output backend (UI
	// bring-up aid). Errors when a session is active or a tone is playing.
	TestTone(d time.Duration) error
}

// StatusStats is the per-node sink/clock/source snapshot for GET /api/status
// (§9.1, D19). Provided by a closure main (K) wires from the sink (E), the
// clock follower (F), and — only while this node runs a source — G.
type StatusStats struct {
	Sink   contracts.SinkStats    // §8.5 servo + jitter stats
	Clock  ClockStat              // follower offset/rtt
	Source *contracts.SourceStats // non-nil only on an active source (D19/D28)
}

// ClockStat is the clock-follower portion of GET /api/status (§7).
type ClockStat struct {
	Synced   bool  `json:"synced"`
	OffsetNs int64 `json:"offsetNs"`
	RTTNs    int64 `json:"rttNs"`
}
