package group

import (
	"net/netip"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
	"ensemble/internal/stream"
)

// This file declares the minimal consumer-side interfaces H needs from its
// sibling pieces. The real concrete types (cluster.Cluster, audio media
// sources, source.Server, stream.Client, clock.Follower) satisfy these
// structurally; tests supply fakes. Method names/signatures follow the real
// exported APIs (D29), not the original H-group.md sketches.

// Cluster is the slice of the cluster piece (C) that H needs: the read side
// (contracts.StateStore — Self/Snapshot/Subscribe) plus the owner-only setters
// (D14). Implemented by *cluster.Cluster.
//
// Group settings are NOT fetched via a separate getter — they ride the derived
// Snapshot (GroupView.Settings, resolved with defaults by C). SetGroupSettings
// writes the per-group LWW record. SetPlayback writes the per-group playback
// record. H only ever calls those two when this node is the group master
// (H enforces master-only, not C). DialCandidates resolves the master's
// address per §3.1 (D6) — the ONLY dialing H does (D22).
type Cluster interface {
	contracts.StateStore // Self() id.ID; Snapshot() contracts.Snapshot; Subscribe() <-chan struct{}

	SetFollowing(target id.ID)                               // Zero == unfollow/solo (§5.1)
	SetPlayback(group id.ID, p contracts.Playback)           // master-only (§4/§8.6/D28)
	SetGroupSettings(group id.ID, s contracts.GroupSettings) // master-only LWW (§8.3/D23)
	DialCandidates(peer id.ID) []netip.Addr                  // best-first (§3.1/D6)
}

// MediaSource is one media source as a stream of canonical 20 ms PCM frames
// (§6.1/§8.1, D26). Implemented by the audio piece (D)'s audio.Source. D9 EOF
// semantics: ReadFrame fills exactly stream.FrameBytes into caller-owned dst;
// the final (padded) pull frame returns nil, the NEXT call returns io.EOF.
// Live-paced sources (http/input) never return io.EOF before Close — momentary
// underflow yields a silence frame and nil (D30: there is no ErrUnderflow).
type MediaSource interface {
	ReadFrame(dst []byte) error
	Live() bool
	Close() error
}

// MetadataSource is the optional now-playing channel (D57): a MediaSource that can
// describe the current track implements it. playbackRecord type-asserts the active
// source and folds the result into the replicated Playback record. Sources without
// metadata (e.g. line-in) simply don't implement it.
type MetadataSource interface {
	Metadata() (contracts.TrackMetadata, bool)
}

// QueueProgress is the optional play-queue channel: a MediaSource backed by a
// queue (the file-source queueSource) implements it. playbackRecord type-asserts
// the active source and, when present, takes the now-playing URI/metadata + the
// per-track position + the upcoming queue from it instead of the single-source
// fields. Single sources (http/input/spotify) don't implement it.
type QueueProgress interface {
	Now() (uri string, meta *contracts.TrackMetadata, positionSec float64, upcoming []contracts.QueueItem)
}

// MediaFactory opens a URI into a MediaSource by scheme (§6.1/D26). The concrete
// implementation (K's adapter) binds audio.Open's ctx + mediaDir so H's seam is
// just Open(uri). mediaDir scoping / path-traversal rejection live in D; an
// unsupported scheme surfaces as the factory's error.
type MediaFactory interface {
	Open(uri string) (MediaSource, error)
	// Probe reads a file URI's embedded tags (title/artist/album) without opening
	// a decoder/session, to pre-fill a queue entry's metadata at enqueue time.
	// ok=false for non-file schemes or on any resolution/IO failure (the caller
	// then leaves the entry's metadata nil and the UI uses the filename).
	Probe(uri string) (contracts.TrackMetadata, bool)
}

// OpusEncoder compresses one canonical PCM frame into one opus packet (D33).
// Implemented by *audio.OpusEncoder. The returned slice aliases the encoder's
// reused buffer (valid until the next Encode) — H copies before fan-out.
type OpusEncoder interface {
	Encode(pcm []byte) ([]byte, error)
	Close() error
}

// OpusFactory builds an OpusEncoder, returning dl.ErrUnavailable when libopus
// is not loadable on this host (D33). The concrete impl wraps
// audio.NewOpusEncoder.
type OpusFactory interface {
	NewEncoder() (OpusEncoder, error)
}

// SourceServer is the master-side audio source server on SOURCE_PORT
// (§8.2/§8.7/D22–D24), implemented by *source.Server. Method names follow the
// real server (D29): StartSession arms a generation + transport + bufferMs;
// ReleaseFrame fans one released frame out (stamping seq internally) and folds
// it into the ring; Reconfig broadcasts a non-stop RECONFIG (D23, settings
// change); StopSession broadcasts RECONFIG/stop and disarms the session (§8.6);
// Stats feeds the playback heartbeat (D28).
type SourceServer interface {
	StartSession(gen uint32, t stream.Transport, bufferMs int)
	ReleaseFrame(pts int64, payload []byte) uint64
	Reconfig()
	StopSession()
	Stats() contracts.SourceStats
}

// Subscriber is this node's member-side stream client (G internal/stream): it
// subscribes to a master's SOURCE_PORT and delivers received frames to the
// local sink (deliver wiring is K's, not H's). Method names follow *stream.Client
// (D29). Subscribe is idempotent for an unchanged (addr,gen,transport).
type Subscriber interface {
	Subscribe(sourceAddr netip.AddrPort, gen uint32, t stream.Transport) error
	Unsubscribe()
}

// ClockControl re-points the local clock follower at the current master clock
// endpoint + generation (§7/D17). Implemented by *clock.Follower. The follower
// discards samples and resyncs on any change; a same-target call is a no-op.
type ClockControl interface {
	SetMaster(dst netip.AddrPort, gen uint32)
}

// FollowClient (contracts.FollowClient, D16) drives takeover (§5.2): POST
// /api/follow|/unfollow on peers. Concrete impl injected by the API piece (I).
type FollowClient = contracts.FollowClient
