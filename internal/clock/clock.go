// Package clock implements the master-anchored clock server and per-node
// follower over the STREAM_PORT UDP mux (§7). NTP-style: the master answers
// requests (0x10) with replies (0x11) stamping receive/send times; every member
// (including the master against localhost) runs a follower that probes once per
// second, computes offset ((t2-t1)+(t3-t4))/2, and keeps the median of the 5
// best-RTT samples of the last 30. Implemented by piece F.
package clock

import (
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"ensemble/internal/stream"
)

// nowFunc returns monotonic-derived nanoseconds. Production uses monoNow; tests
// inject a fake. It must be monotonic and is the SAME clock the Follower uses
// for t4 when following localhost (so master-vs-self offset ~ 0).
type nowFunc func() int64

// monoEpoch anchors monoNow: WALL time at process start plus a monotonic
// reading since. The clock ticks monotonically (immune to NTP steps — only
// the start anchor comes from the wall), but cross-node offsets become the
// real wall skew between hosts (µs–ms on a synced LAN) instead of arbitrary
// process-start deltas (which read as alarming multi-second "offsets" in
// logs while being perfectly harmless to the translation math).
var (
	monoEpoch = time.Now()
	monoWall0 = monoEpoch.UnixNano()
)

// monoNow is the production clock: wall-anchored monotonic nanoseconds.
func monoNow() int64 {
	return monoWall0 + int64(time.Since(monoEpoch))
}

// MonoNow exposes the package's monotonic clock. EVERY local-time value that
// flows through the offset translation (pts stamping in H, playout deadlines
// in E) MUST come from this one clock: the follower measures offsets between
// ITS now() and the master's — feeding wall-clock time into LocalToMaster /
// comparing MasterToLocal output against wall time silently adds the
// inter-process start-delta to the playout path (the "same-host offset"
// lag-by-|offset| bug).
func MonoNow() int64 { return monoNow() }

// defaultProbeInterval is the production 1 Hz probe cadence (§7).
const defaultProbeInterval = time.Second

// pendingTTL bounds the pending map: probes older than this (lost replies) are
// pruned each tick so the map can't grow unbounded under sustained loss.
const pendingTTL = 5 * time.Second

// ---- Server -----------------------------------------------------------------

// Server answers clock requests (type 0x10) with replies (type 0x11) on the
// shared UDP mux (§7). One per node; runs entirely on the mux read goroutine
// (the handler is cheap and non-blocking, honoring the Mux contract).
type Server struct {
	mux *stream.Mux
	now nowFunc
	log *slog.Logger
}

// NewServer creates the server bound to mux. It does NOT register yet.
func NewServer(mux *stream.Mux, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{mux: mux, now: monoNow, log: log.With("comp", "clock-server")}
}

// Start registers the 0x10 handler on the mux. Idempotent; safe before or after
// mux.Run (S allows Register any time).
func (s *Server) Start() {
	s.mux.Register(stream.TypeClockReq, s.handle)
}

// handle decodes the request, stamps t2 on entry and t3 just before send, and
// writes a 0x11 reply to from. Malformed packets are dropped. Echoes Gen and
// Seq unchanged; does not filter by generation (the master is the time source).
func (s *Server) handle(pkt []byte, from netip.AddrPort) {
	t2 := s.now() // stamp receive ASAP
	h, t1, _, _, err := decodeClock(pkt)
	if err != nil || h.Type != stream.TypeClockReq {
		s.log.Debug("dropping malformed clock request", "err", err, "from", from)
		return
	}
	var buf [packetSize]byte
	t3 := s.now() // stamp send just before WriteTo
	encodeClock(buf[:], stream.TypeClockRsp, h.Gen, h.Seq, t1, t2, t3)
	if _, err := s.mux.WriteTo(buf[:], from); err != nil {
		s.log.Debug("clock reply write failed", "err", err, "to", from)
	}
}

// ---- Follower ---------------------------------------------------------------

// Follower probes a master's clock once per second and maintains the offset
// estimate (§7). Implements contracts.Clock.
type Follower struct {
	mux      *stream.Mux
	now      nowFunc
	log      *slog.Logger
	interval time.Duration

	mu   sync.Mutex     // the one mutex: guards everything below
	est  estimator      // offset estimator (last 30, best 5)
	gen  uint32         // current session generation; other-gen replies ignored
	seq  uint64         // next probe sequence
	dst  netip.AddrPort // master clock endpoint (mux UDP addr)
	have bool           // dst has been set at least once

	pending map[uint64]int64 // probe seq -> t1 (local send time)

	probes  uint64 // probes sent
	replies uint64 // replies accepted
	synced  bool   // last-observed sync state (for sync acquired/lost logging)

	done    chan struct{}
	wg      sync.WaitGroup
	started bool
	closed  bool
}

// NewFollower creates a follower bound to mux. now MUST be the same monotonic
// clock the local Server uses, so a master following itself sees ~0 offset.
func NewFollower(mux *stream.Mux, log *slog.Logger) *Follower {
	return newFollowerInterval(mux, log, defaultProbeInterval)
}

// newFollowerInterval is the test seam: same as NewFollower but with a
// caller-chosen probe interval (production uses defaultProbeInterval).
func newFollowerInterval(mux *stream.Mux, log *slog.Logger, d time.Duration) *Follower {
	if log == nil {
		log = slog.Default()
	}
	return &Follower{
		mux:      mux,
		now:      monoNow,
		log:      log.With("comp", "clock-follower"),
		interval: d,
		pending:  make(map[uint64]int64),
		done:     make(chan struct{}),
	}
}

// Start registers the 0x11 reply handler and launches the 1 Hz probe loop.
// Call SetMaster before relying on MasterNow. Idempotent.
func (f *Follower) Start() {
	f.mu.Lock()
	if f.started {
		f.mu.Unlock()
		return
	}
	f.started = true
	f.mu.Unlock()

	f.mux.Register(stream.TypeClockRsp, f.handleReply)
	f.wg.Add(1)
	go f.loop()
}

// SetMaster points the follower at a master clock endpoint and resyncs: it sets
// dst, bumps to the given generation, resets the estimator and pending map.
// Used on every mastership change (§7) and at startup. Calling it with the same
// (dst, gen) is a no-op (no spurious resync). The master follows ITSELF by
// passing mux.LocalAddr() here; an unspecified host is rewritten to loopback.
func (f *Follower) SetMaster(dst netip.AddrPort, gen uint32) {
	dst = dialable(dst)
	f.mu.Lock()
	if f.have && f.dst == dst && f.gen == gen {
		f.mu.Unlock()
		return // no-op: same target, do not wipe the window
	}
	prev := f.dst
	hadPrev := f.have
	sameEndpoint := hadPrev && prev == dst
	f.dst = dst
	f.gen = gen
	f.have = true
	if !sameEndpoint {
		// Only an ENDPOINT change invalidates the offset estimate — the master
		// process (and so its clock) is the same across a mere generation bump
		// (new session / settings change). Wiping samples on gen bumps held
		// playout unsynced for up to a probe interval at every session start.
		f.est.reset()
		f.synced = false
	}
	clear(f.pending)
	f.mu.Unlock()

	if sameEndpoint {
		f.log.Debug("master clock gen bumped (offset kept)", "endpoint", dst.String(), "gen", gen)
	} else if hadPrev {
		f.log.Info("master clock re-pointed", "from", prev.String(), "to", dst.String(), "gen", gen)
	} else {
		f.log.Info("master clock set", "endpoint", dst.String(), "gen", gen)
	}

	// Don't wait for the next 1 Hz tick: a freshly (re-)pointed follower should
	// sync ASAP — playout is gated on it.
	if !sameEndpoint {
		go f.probe()
	}
}

// dialable rewrites a wildcard/unspecified host to loopback so a self-dial
// against mux.LocalAddr() (often 0.0.0.0:P or [::]:P) actually reaches the local
// server.
func dialable(a netip.AddrPort) netip.AddrPort {
	if a.Addr().IsUnspecified() {
		loop := netip.AddrFrom4([4]byte{127, 0, 0, 1})
		if a.Addr().Is6() {
			loop = netip.IPv6Loopback()
		}
		return netip.AddrPortFrom(loop, a.Port())
	}
	return a
}

// loop sends one probe per interval until Close.
func (f *Follower) loop() {
	defer f.wg.Done()
	t := time.NewTicker(f.interval)
	defer t.Stop()
	for {
		select {
		case <-f.done:
			return
		case <-t.C:
			f.probe()
		}
	}
}

// probe sends one clock request to the current master, if any.
func (f *Follower) probe() {
	f.mu.Lock()
	if !f.have {
		f.mu.Unlock()
		return
	}
	t1 := f.now()
	seq := f.seq
	f.seq++
	gen := f.gen
	dst := f.dst
	f.pending[seq] = t1
	f.prunePendingLocked(t1)
	f.probes++
	f.mu.Unlock()

	var buf [packetSize]byte
	encodeClock(buf[:], stream.TypeClockReq, gen, seq, t1, 0, 0)
	if _, err := f.mux.WriteTo(buf[:], dst); err != nil {
		f.log.Debug("clock probe write failed", "err", err, "to", dst)
	}
}

// prunePendingLocked drops probes older than pendingTTL (lost replies). Caller
// holds f.mu.
func (f *Follower) prunePendingLocked(nowNs int64) {
	cutoff := nowNs - int64(pendingTTL)
	for seq, t1 := range f.pending {
		if t1 < cutoff {
			delete(f.pending, seq)
		}
	}
}

// handleReply runs on the mux read goroutine; it must not block. It stamps t4
// on entry, matches the reply to a pending probe, and feeds the estimator.
func (f *Follower) handleReply(pkt []byte, from netip.AddrPort) {
	t4 := f.now() // stamp arrival ASAP
	h, _, t2, t3, err := decodeClock(pkt)
	if err != nil || h.Type != stream.TypeClockRsp {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if h.Gen != f.gen {
		return // stale generation, drop (resync gate §8.4)
	}
	t1, ok := f.pending[h.Seq]
	if !ok {
		return // unknown / duplicate / late, drop
	}
	delete(f.pending, h.Seq)
	f.est.add(newSample(t1, t2, t3, t4))
	f.replies++

	// First-sync transition (unsynced → synced): log offset + best RTT once.
	if !f.synced {
		if off, rtt, ok := f.est.estimate(); ok {
			f.synced = true
			f.log.Info("clock sync acquired", "gen", f.gen, "master", f.dst.String(),
				"offsetNs", off, "rttNs", rtt)
		}
	}
}

// MasterNow returns master-clock ns and whether synced (contracts.Clock).
func (f *Follower) MasterNow() (masterNanos int64, ok bool) {
	f.mu.Lock()
	off, ok := f.est.offset()
	f.mu.Unlock()
	if !ok {
		return 0, false
	}
	return f.now() + off, true
}

// MasterToLocal converts a master-clock instant to local ns (contracts.Clock).
func (f *Follower) MasterToLocal(masterNanos int64) (localNanos int64, ok bool) {
	f.mu.Lock()
	off, ok := f.est.offset()
	f.mu.Unlock()
	if !ok {
		return 0, false
	}
	return masterNanos - off, true
}

// LocalToMaster converts a local instant to master-clock ns (D10).
func (f *Follower) LocalToMaster(localNanos int64) (masterNanos int64, ok bool) {
	f.mu.Lock()
	off, ok := f.est.offset()
	f.mu.Unlock()
	if !ok {
		return 0, false
	}
	return localNanos + off, true
}

// FollowerStats is a snapshot for diagnostics / /api/status.
type FollowerStats struct {
	Synced   bool   // MasterNow ok
	OffsetNs int64  // current estimate (0 if unsynced)
	RTTNs    int64  // smallest RTT in the window (0 if unsynced)
	Samples  int    // samples currently in window
	Gen      uint32 // current generation
	Master   string // dst.String()
	Probes   uint64 // probes sent
	Replies  uint64 // replies accepted
}

// Stats reports follower state for /api/status.
func (f *Follower) Stats() FollowerStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	off, rtt, ok := f.est.estimate()
	master := ""
	if f.have {
		master = f.dst.String()
	}
	return FollowerStats{
		Synced:   ok,
		OffsetNs: off,
		RTTNs:    rtt,
		Samples:  f.est.len(),
		Gen:      f.gen,
		Master:   master,
		Probes:   f.probes,
		Replies:  f.replies,
	}
}

// Close stops the probe loop. Idempotent. The reply handler stays registered on
// the mux (S has no Unregister); the mux itself is closed by K.
func (f *Follower) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	started := f.started
	close(f.done)
	f.mu.Unlock()
	if started {
		f.wg.Wait()
	}
	return nil
}
