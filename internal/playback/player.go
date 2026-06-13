// Package playback is the single playout component (D49/D61): the verb-driven
// "join a stream and play it in sync" unit, shared by every node that plays audio.
// Two front-ends drive the SAME Player: the group engine (H) drives the local
// player in-process for a gossiping member, and the control listener (D58, master→
// playback wire commands) drives it on a non-gossiping playback-only node. Both hit
// the identical interface, so playout behaves the same for Go and MCU (D49).
//
// localPlayer wraps the existing trio — a clock follower (F), a member-side stream
// subscriber (G), and a sink (E) — behind that interface; it is a faithful
// re-expression of what the group engine's repointLocked did inline before the
// split, with volume/delay/cap/status verbs added for the control plane.
package playback

import (
	"net/netip"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
	"ensemble/internal/stream"
)

// Player is the verb interface every playout front-end drives (D54/D61,
// PLAYER.md §6). All methods are safe to call repeatedly with the same
// arguments — they are idempotent, matching the control plane's soft-state model
// (D58). Implementations are not required to be goroutine-safe; callers serialize
// (the group engine under its mutex, the control listener on its read goroutine).
type Player interface {
	// Attach joins a stream: points the clock at a.Clock under a.Gen, arms the
	// sink for a.Gen, and subscribes to a.Source on a.Transport. "Join + play."
	Attach(a Attach)
	// Detach leaves the source (Unsubscribe → BYE) and disarms playout (no
	// starvation warnings). It does NOT unpoint the clock; a later Attach/Sync
	// re-points it.
	Detach()
	// Sync keeps the clock follower warm WITHOUT playing — a member, while idle,
	// stays synced so the next play starts instantly. A playback-only node does
	// not call this (it is idle until ATTACHed). Idempotent for an unchanged
	// (dst, gen).
	Sync(clock netip.AddrPort, gen uint32)
	// SetVolume sets software volume 0..100 + mute (the master-driven knob, D54).
	SetVolume(pct uint8, mute bool)
	// SetDelay sets the output-delay calibration in milliseconds, signed (D36/D54).
	SetDelay(ms int)
	// SetEqualize sets the master-driven cross-room equalization delay in
	// milliseconds, unsigned (D65). Separate from SetDelay (the node-owned acoustic
	// offset): the sink sums both. A faster room (smaller device buffer) is delayed
	// to match the slowest so speaker_time aligns. Idempotent (soft-state); only a
	// changed value re-anchors playout.
	SetEqualize(ms int)
	// SetCap toggles a runtime capability by id (D54). A localPlayer (Go member)
	// has no per-cap toggle here — capability disable rides the cluster record —
	// so this is a no-op for it; a firmware player acts on it.
	SetCap(capID uint8, on bool)
	// Status snapshots telemetry for the STATUS packet (D55) and the per-room UI.
	Status() stream.StatusPayload
}

// Attach is the parameter set for Player.Attach, mirroring the ATTACH wire payload
// (PLAYER.md §6.1). Codec and BufferMs are carried for front-ends that own
// their own decode/sink construction (a standalone playback node); the localPlayer
// ignores them because the gossiping member's decode + sink lead are wired by K and
// the group, exactly as before the split.
type Attach struct {
	Source    netip.AddrPort // master SOURCE_PORT (HELLO/subscribe target)
	Clock     netip.AddrPort // master STREAM_PORT (clock follower target)
	Gen       uint32         // session generation to arm for
	Codec     stream.Codec   // informational for localPlayer (see note above)
	Transport stream.Transport
	BufferMs  int // informational for localPlayer (see note above)
}

// Clock is the slice of the clock follower (F) the player re-points. Implemented by
// *clock.Follower; matches group.ClockControl so the engine's existing dep value
// assigns straight through.
type Clock interface {
	SetMaster(dst netip.AddrPort, gen uint32)
}

// Subscriber is the member-side stream client (G) the player drives. Implemented by
// *stream.Client; matches group.Subscriber.
type Subscriber interface {
	Subscribe(sourceAddr netip.AddrPort, gen uint32, t stream.Transport) error
	Unsubscribe()
}

// ClockStatsFunc reports the current clock estimate for Status (offset/rtt/synced).
// Optional: a localPlayer built without one reports zeros for those fields (its
// stats flow through /api/status instead). A standalone playback node passes a
// closure over its clock.Follower.Stats so STATUS carries real timing.
type ClockStatsFunc func() (offsetNs, rttNs int64, synced bool)

// Config wires a localPlayer to its sibling pieces. Clock/Sub/Sink are required;
// ClockStats is optional.
type Config struct {
	Self       id.ID
	Clock      Clock
	Sub        Subscriber
	Sink       contracts.Sink
	ClockStats ClockStatsFunc // optional
}

// localPlayer is the in-process Player: clock follower + stream subscriber + sink.
type localPlayer struct {
	self       id.ID
	clock      Clock
	sub        Subscriber
	sink       contracts.Sink
	clockStats ClockStatsFunc

	lastVolPct uint8 // remembered so unmute restores the prior level
	muted      bool
	playing    bool
}

// NewLocal builds the in-process Player from the existing clock/sub/sink trio.
func NewLocal(cfg Config) Player {
	return &localPlayer{
		self:       cfg.Self,
		clock:      cfg.Clock,
		sub:        cfg.Sub,
		sink:       cfg.Sink,
		clockStats: cfg.ClockStats,
		lastVolPct: 100,
	}
}

func (p *localPlayer) Attach(a Attach) {
	// Same order as the pre-split repointLocked: point clock, arm sink, subscribe.
	p.clock.SetMaster(a.Clock, a.Gen)
	p.sink.Reset(a.Gen)
	_ = p.sub.Subscribe(a.Source, a.Gen, a.Transport)
	p.playing = true
}

func (p *localPlayer) Detach() {
	p.sub.Unsubscribe()
	p.sink.Disarm()
	p.playing = false
}

func (p *localPlayer) Sync(clock netip.AddrPort, gen uint32) {
	p.clock.SetMaster(clock, gen)
}

func (p *localPlayer) SetVolume(pct uint8, mute bool) {
	if pct > 100 {
		pct = 100
	}
	p.lastVolPct = pct
	p.muted = mute
	if mute {
		p.sink.SetGain(0)
		return
	}
	p.sink.SetGain(float64(pct) / 100.0)
}

func (p *localPlayer) SetDelay(ms int) {
	p.sink.SetDelayOffset(int64(ms) * 1_000_000)
}

// SetEqualize forwards the master's cross-room equalization delay to the sink (D65).
// The sink method is optional (not on contracts.Sink): a backend/sink that doesn't
// implement it simply ignores equalization. ms is unsigned upstream; guard anyway.
func (p *localPlayer) SetEqualize(ms int) {
	if ms < 0 {
		ms = 0
	}
	if s, ok := p.sink.(interface{ SetEqualizeDelay(int64) }); ok {
		s.SetEqualizeDelay(int64(ms) * 1_000_000)
	}
}

// SetCap is a no-op for a gossiping member: capability disable rides the cluster
// record (D40), not the sink. A firmware player overrides this behavior.
func (p *localPlayer) SetCap(capID uint8, on bool) {}

func (p *localPlayer) Status() stream.StatusPayload {
	st := p.sink.Stats()
	s := stream.StatusPayload{
		NodeID:          [16]byte(p.self),
		Synced:          st.Synced,
		Playing:         p.playing,
		Buffered:        clampU16(st.Buffered),
		RatePPMx1000:    int32(st.RatePPM * 1000),
		Played:          st.Played,
		Silence:         st.Silence,
		Late:            st.LateDrop,
		DeviceDelayNs:   st.DeviceDelayNs,
		PhaseErrNs:      st.PhaseErrNs,
		Calibrated:      st.Calibrated,
		SamplesInjected: st.SamplesInjected,
		SamplesDropped:  st.SamplesDropped,
	}
	if p.clockStats != nil {
		off, rtt, synced := p.clockStats()
		s.OffsetNs, s.RTTNs = off, rtt
		s.Synced = synced
	}
	return s
}

func clampU16(n int) uint16 {
	if n < 0 {
		return 0
	}
	if n > 0xFFFF {
		return 0xFFFF
	}
	return uint16(n)
}
