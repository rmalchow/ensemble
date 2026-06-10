package group

import (
	"errors"
	"strings"
	"time"

	"ensemble/internal/contracts"
	"ensemble/internal/dl"
	"ensemble/internal/id"
	"ensemble/internal/stream"
)

// clockWaitTimeout bounds how long Play waits for the local clock follower to
// sync before stamping pts (§7). A master follows its own clock server over the
// network like any member (its real advertised address), syncing in ~1 s.
const clockWaitTimeout = 2 * time.Second

// clockWaitPoll is the retry cadence while waiting for clock sync.
const clockWaitPoll = 50 * time.Millisecond

// Play starts playback of uri on this node's group (§6/§8.2). Master-only.
//
// A FILE uri plays through the gapless play queue: if a queue session is already
// running it is a front-switch (the current track is dropped, the new one plays
// now, upcoming tracks are kept — §queue); otherwise a fresh queue session starts
// seeded with the one track. Any OTHER scheme (http/input/spotify) plays as a
// single source and clears the queue. Either way any running session is replaced
// first (§8.6).
func (e *Engine) Play(uri string) error {
	if uriScheme(uri) == "file" {
		return e.playFile(uri)
	}
	e.log.Info("opening source", "uri", uri, "scheme", uriScheme(uri))
	src, err := e.p.Media.Open(uri)
	if err != nil {
		e.log.Warn("source open failed", "uri", uri, "scheme", uriScheme(uri), "err", err)
		return err
	}
	return e.installSession(uri, src)
}

// Enqueue appends uris to the END of the file-source queue. On a live queue
// session they are appended; otherwise (idle, or a non-file source playing) a
// fresh queue session starts and begins playing at once (the first add to an idle
// queue auto-plays). Tags are probed up front (off-lock) to pre-fill metadata.
// Master-only.
func (e *Engine) Enqueue(uris []string) error {
	items := e.queueItems(uris)
	if len(items) == 0 {
		return nil
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	if qs := e.currentQueueLocked(); qs != nil {
		qs.Append(items)
		e.republishLocked()
		e.mu.Unlock()
		e.log.Info("queue appended", "count", len(items))
		return nil
	}
	e.mu.Unlock()
	return e.startQueue(items)
}

// Next skips to the next upcoming queued track (gaplessly). No-op when no queue
// session is running. Master-only.
func (e *Engine) Next() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	qs := e.currentQueueLocked()
	if qs == nil {
		return nil
	}
	qs.Next() // takes effect on the next ReadFrame, which re-publishes via onChange
	e.log.Info("queue next")
	return nil
}

// RemoveFromQueue removes the upcoming item at index (0 == the next track).
// uriGuard, when non-empty, must match that item's URI (guards an index race with
// a concurrent snapshot). No-op when no queue session is running. Master-only.
func (e *Engine) RemoveFromQueue(index int, uriGuard string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	qs := e.currentQueueLocked()
	if qs == nil {
		return nil
	}
	qs.RemoveUpcoming(index, uriGuard)
	e.republishLocked()
	e.log.Info("queue item removed", "index", index)
	return nil
}

// playFile routes a file URI into the queue: a gapless front-switch on a live
// queue session, else a fresh single-track queue session.
func (e *Engine) playFile(uri string) error {
	item := e.queueItem(uri)
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	if qs := e.currentQueueLocked(); qs != nil {
		qs.PlayNow(item)
		e.republishLocked()
		e.mu.Unlock()
		e.log.Info("queue play-now", "uri", uri)
		return nil
	}
	e.mu.Unlock()
	return e.startQueue([]contracts.QueueItem{item})
}

// startQueue opens the first item (validating it, so a bad file surfaces as an
// error) and installs a gapless queue session over items. items must be non-empty.
func (e *Engine) startQueue(items []contracts.QueueItem) error {
	qs := newQueueSource(items, e.p.Media.Open, e.RefreshPlayback)
	if err := qs.prime(); err != nil {
		e.log.Warn("source open failed", "uri", items[0].URI, "scheme", uriScheme(items[0].URI), "err", err)
		return err
	}
	return e.installSession(items[0].URI, qs)
}

// queueItem builds a queue entry for uri, probing embedded tags (best-effort).
func (e *Engine) queueItem(uri string) contracts.QueueItem {
	it := contracts.QueueItem{URI: uri}
	if md, ok := e.p.Media.Probe(uri); ok {
		it.Metadata = &md
	}
	return it
}

// queueItems builds queue entries for uris (tag probing happens here, off-lock).
func (e *Engine) queueItems(uris []string) []contracts.QueueItem {
	items := make([]contracts.QueueItem, 0, len(uris))
	for _, u := range uris {
		items = append(items, e.queueItem(u))
	}
	return items
}

// currentQueueLocked returns the running session's queue source, or nil when no
// session is running or the session is a single (non-queue) source. Caller holds e.mu.
func (e *Engine) currentQueueLocked() *queueSource {
	if e.sess == nil {
		return nil
	}
	qs, _ := e.sess.src.(*queueSource)
	return qs
}

// republishLocked re-publishes the current session's playback record immediately
// (after a queue mutation). Caller holds e.mu. No-op when idle.
func (e *Engine) republishLocked() {
	if e.sess == nil {
		return
	}
	e.p.Cluster.SetPlayback(e.sess.groupID, e.sess.playbackRecord(e.now(), e.p.Source.Stats()))
	e.lastBeat = e.now()
}

// installSession negotiates the codec, builds the opus encoder, waits for clock
// sync, then installs src as the running session under e.mu, replacing any prior
// one (§8.6). src is already open (and, for a queue, primed). uri is the session
// label (a queue's now-playing URI is taken from the source per frame).
func (e *Engine) installSession(uri string, src MediaSource) error {
	live := src.Live()
	pacing := "pull"
	if live {
		pacing = "live"
	}

	// Codec negotiation (§8.3/D33): the master picks the EFFECTIVE codec — the
	// wanted codec iff EVERY current member's effective caps support it AND this
	// master can encode it, else pcm (always universal, never IP-fragments).
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		_ = src.Close()
		return ErrClosed
	}
	snap := e.p.Cluster.Snapshot()
	mv := myGroup(snap, e.self)
	if !mv.found {
		e.mu.Unlock()
		_ = src.Close()
		return ErrNotSynced // self not derived yet; transient, retry
	}
	// Every node masters its OWN group (1:1) and may always source it (D49+).
	groupID := mv.group.ID // == e.self
	settings := fillDefaults(mv.group.Settings)
	settings.Codec = e.negotiateCodecLocked(snap, mv, settings.Codec)
	e.mu.Unlock()

	e.log.Info("source opened", "uri", uri, "scheme", uriScheme(uri), "codec", settings.Codec, "transport", settings.Transport, "pacing", pacing)

	// Opus encoder (D33): master encodes once for all subscribers.
	var enc OpusEncoder
	if settings.Codec == "opus" {
		if e.p.Opus == nil {
			_ = src.Close()
			return ErrNoOpus
		}
		var err error
		enc, err = e.p.Opus.NewEncoder()
		if err != nil {
			_ = src.Close()
			if errors.Is(err, dl.ErrUnavailable) {
				e.log.Warn("opus encoder unavailable", "err", err)
				return ErrNoOpus
			}
			e.log.Error("opus encoder creation failed", "err", err)
			return err
		}
		e.log.Info("opus encoder created")
	}

	// Clock readiness: stamp startMaster in master time (§7). Retry-wait.
	startMaster, ok := e.waitForClock()
	if !ok {
		_ = src.Close()
		if enc != nil {
			_ = enc.Close()
		}
		return ErrNotSynced
	}

	// Install the new session (under lock). Replace any running one.
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		_ = src.Close()
		if enc != nil {
			_ = enc.Close()
		}
		return ErrClosed
	}
	e.stopLocked() // halts + clears any prior session (no status write)

	e.gen++
	gen := e.gen

	sess := &session{
		uri:         uri,
		groupID:     groupID,
		codec:       settings.Codec,
		live:        live,
		src:         src,
		srv:         e.p.Source,
		startedUnix: e.now().Unix(),
		transport:   settings.Transport,
		bufferMs:    settings.BufferMs,
		leadMs:      e.p.LeadMs,
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		onEnd:       e.onSessionEnd,
		now:         e.now,
	}
	sess.startMaster.Store(startMaster + int64(e.p.LeadMs)*1_000_000)
	sess.gen.Store(gen)
	sess.setEnc(enc) // nil for pcm
	e.sess = sess

	e.p.Source.StartSession(gen, stream.ParseTransport(settings.Transport), settings.BufferMs)
	e.log.Info("playback started",
		"uri", uri, "gen", gen, "codec", settings.Codec, "transport", settings.Transport,
		"bufferMs", settings.BufferMs, "leadMs", e.p.LeadMs, "live", live)

	// Drive THIS node's player: if it follows its own group it hears the new session
	// immediately; if its player is elsewhere (crosswise), it stays there and the
	// reconcile loop drives it. drivePlayerLocked reads the live session for the
	// self-target case.
	e.drivePlayerLocked(mv)

	e.p.Cluster.SetPlayback(groupID, sess.playbackRecord(e.now(), e.p.Source.Stats()))
	e.lastBeat = e.now()

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		sess.run()
	}()
	e.mu.Unlock()
	return nil
}

// Stop stops the running session, broadcasts RECONFIG/stop, and clears playback
// status (§8.6). Master-only. No-op (nil) if nothing is playing.
func (e *Engine) Stop() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	if e.sess == nil {
		return nil
	}
	groupID := e.sess.groupID
	uri := e.sess.uri
	e.stopLocked()
	e.p.Cluster.SetPlayback(groupID, contracts.Playback{State: "idle"})
	e.log.Info("playback stopped", "uri", uri, "reason", "user")
	return nil
}

// RefreshPlayback re-publishes the current session's playback record immediately,
// outside the heartbeat cadence (watch.go). The Spotify bridge calls this when
// track metadata changes (D57 channel) so the now-playing UI updates at once
// instead of waiting up to one heartbeat. No-op when idle. Master-only.
func (e *Engine) RefreshPlayback() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.sess == nil {
		return
	}
	e.p.Cluster.SetPlayback(e.sess.groupID, e.sess.playbackRecord(e.now(), e.p.Source.Stats()))
	e.lastBeat = e.now()
}

// Pause freezes the running session (D39). Master-only. The media source and the
// session/gen stay alive; the release loop stops emitting (position frozen). The
// replicated playback record flips to state="paused", which the member-side
// session gating (watch.go) treats as NOT playing — so members BYE the source and
// Disarm their sinks, and the master leaves its own subscription too. The
// release ticker keeps ticking purely to keep the goroutine alive; no frames flow.
// 409 ErrNotPlaying if nothing is playing or it is already paused.
func (e *Engine) Pause() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	if e.sess == nil || e.sess.paused.Load() {
		return ErrNotPlaying
	}
	s := e.sess
	s.pausedSec = float64(e.now().Unix() - s.startedUnix)
	if s.pausedSec < 0 {
		s.pausedSec = 0
	}
	s.paused.Store(true)
	// Re-drive our own player now (the session is paused → if we play our own group,
	// detach immediately rather than waiting for a reconcile).
	snap := e.p.Cluster.Snapshot()
	mv := myGroup(snap, e.self)
	if mv.found {
		e.drivePlayerLocked(mv)
	}
	e.p.Source.StopSession() // tell any still-attached subscribers to drop (RECONFIG/stop)
	e.p.Cluster.SetPlayback(s.groupID, s.playbackRecord(e.now(), contracts.SourceStats{}))
	e.lastBeat = e.now()
	e.log.Info("playback paused", "uri", s.uri, "positionSec", s.pausedSec)
	return nil
}

// Resume un-freezes a paused session (D39). Master-only. It bumps the generation
// and re-anchors sessionStart to LocalToMaster(now)+lead — pts restart contiguous-
// monotonic under the NEW gen (the frame index resets; the source continues from
// where it stopped, so audio is contiguous though pts restart with the gen). For
// LIVE sources the readahead already dropped what arrived while paused, so resume
// returns at the live edge. Re-arms the source session, re-points local plumbing,
// resumes the ticker, and writes state="playing". 409 ErrNotPaused if not paused.
func (e *Engine) Resume() error {
	// Clock readiness for the fresh sessionStart (master follows its own clock
	// server over the network; ~ms once synced).
	startMaster, ok := e.waitForClock()
	if !ok {
		return ErrNotSynced
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	if e.sess == nil || !e.sess.paused.Load() {
		return ErrNotPaused
	}
	s := e.sess
	snap := e.p.Cluster.Snapshot()
	mv := myGroup(snap, e.self)
	if !mv.found {
		return ErrNotSynced
	}

	e.gen++
	gen := e.gen
	s.gen.Store(gen)
	s.startMaster.Store(startMaster + int64(e.p.LeadMs)*1_000_000)
	s.anchorSeq.Add(1) // signal run() to reset its frame index to 0 under the new gen
	s.paused.Store(false)

	e.p.Source.StartSession(gen, stream.ParseTransport(s.transport), s.bufferMs)
	e.drivePlayerLocked(mv)
	e.p.Cluster.SetPlayback(s.groupID, s.playbackRecord(e.now(), e.p.Source.Stats()))
	e.lastBeat = e.now()
	e.log.Info("playback resumed", "uri", s.uri, "gen", gen)
	return nil
}

// stopLocked halts the current session (if any), broadcasts RECONFIG/stop, and
// clears e.sess. Does NOT write playback status (the caller decides). Caller
// holds e.mu; it is released across halt() and re-taken, per the no-deadlock
// rule (halt waits on the run goroutine whose onEnd re-takes e.mu).
func (e *Engine) stopLocked() {
	s := e.sess
	if s == nil {
		return
	}
	e.sess = nil
	e.mu.Unlock()
	s.halt()
	s.srv.StopSession()
	s.closeSrc()
	e.mu.Lock()
}

// onSessionEnd is the run goroutine's exit callback. For a natural EOF it ends
// the session itself (clears e.sess + idle status); for endStop the caller
// (Stop/replace/teardown) already owns the lifecycle, so this is a no-op.
func (e *Engine) onSessionEnd(s *session, reason endReason) {
	if reason != endEOF {
		return
	}
	e.mu.Lock()
	if e.sess != s {
		e.mu.Unlock()
		return // already replaced/stopped
	}
	groupID := s.groupID
	e.sess = nil
	e.mu.Unlock()

	s.srv.StopSession()
	s.closeSrc()
	e.p.Cluster.SetPlayback(groupID, contracts.Playback{State: "idle"})
	e.log.Info("playback ended (EOF)", "uri", s.uri)
}

// waitForClock returns the master-clock "now" once the clock is synced,
// retrying up to clockWaitTimeout. ok=false if it never syncs (transient
// ErrNotSynced). Uses Clock.MasterNow() — NEVER LocalToMaster(wall-clock):
// the offset is measured against the follower's own monotonic clock, so
// injecting any other timebase shifts every pts by the inter-process
// start-delta (the same-host lag-by-|offset| bug).
func (e *Engine) waitForClock() (masterNanos int64, ok bool) {
	deadline := e.now().Add(clockWaitTimeout)
	for {
		if m, k := e.p.Clock.MasterNow(); k {
			return m, true
		}
		if !e.now().Before(deadline) {
			return 0, false
		}
		time.Sleep(clockWaitPoll)
	}
}

// negotiateCodecLocked computes the EFFECTIVE codec for a session over the group
// (§8.3/D33 negotiation, supersedes reject-behavior): returns wanted iff every
// CURRENT member's effective caps include it AND (for opus) this master can
// encode it, else "pcm" — always universal. Logs an INFO downgrade line naming
// the lacking members. Caller holds e.mu.
func (e *Engine) negotiateCodecLocked(snap contracts.Snapshot, mv myView, wanted string) string {
	if wanted == "" {
		wanted = contracts.DefaultCodec
	}
	if wanted == "pcm" {
		return "pcm" // universal; nothing to negotiate
	}
	// opus: this master must have an encoder.
	if wanted == "opus" && e.p.Opus == nil {
		e.log.Info("codec negotiated", "wanted", wanted, "got", "pcm", "lacking", "[no opus encoder on master]")
		return "pcm"
	}
	lacking := membersLackingCodec(snap, mv, wanted)
	if len(lacking) == 0 {
		return wanted
	}
	e.log.Info("codec negotiated", "wanted", wanted, "got", "pcm", "lacking", "["+strings.Join(lacking, " ")+"]")
	return "pcm"
}

// membersLackingCodec returns the display names of the current group members
// whose effective caps do NOT include codec (§8.3). Caller holds e.mu.
func membersLackingCodec(snap contracts.Snapshot, mv myView, codec string) []string {
	byID := make(map[id.ID]contracts.NodeView, len(snap.Nodes))
	for _, n := range snap.Nodes {
		byID[n.ID] = n
	}
	var lacking []string
	for _, m := range mv.group.Members {
		n, ok := byID[m]
		if ok && n.PlaybackNode {
			continue // playback nodes take no part in codec negotiation (D51)
		}
		if !ok || !hasCodec(n.Capabilities.Codecs, codec) {
			name := m.String()
			if ok && n.Name != "" {
				name = n.Name
			}
			lacking = append(lacking, name)
		}
	}
	return lacking
}

// renegotiateLocked checks the running session's effective codec against the
// CURRENT membership and downgrades it to pcm in place when a member no longer
// supports it (D33 mid-session renegotiation). Only opus→pcm downgrades happen
// automatically; upgrades wait for the next play/settings change. The restart
// mirrors a live settings change: bump gen, drop the encoder, re-arm the source
// session, broadcast RECONFIG (members reconnect), re-point local plumbing, and
// rewrite the playback record — all resuming from the current position (the media
// source keeps reading where it was; only pts restart under the new gen). Caller
// holds e.mu; e.sess is non-nil and not paused.
func (e *Engine) renegotiateLocked(snap contracts.Snapshot, mv myView) {
	s := e.sess
	if s.codec != "opus" {
		return // only opus can lose support mid-session; pcm is universal
	}
	lacking := membersLackingCodec(snap, mv, "opus")
	if len(lacking) == 0 {
		return // still supported by all members
	}
	e.log.Info("codec negotiated", "wanted", "opus", "got", "pcm",
		"lacking", "["+strings.Join(lacking, " ")+"]", "midSession", true)

	// Downgrade in place: drop the encoder (run() publishes raw PCM next tick),
	// bump gen, re-arm the source + local plumbing, broadcast RECONFIG.
	if prev := s.setEnc(nil); prev != nil {
		_ = prev.Close()
	}
	s.codec = "pcm"
	e.gen++
	gen := e.gen
	s.gen.Store(gen)
	e.p.Source.StartSession(gen, stream.ParseTransport(s.transport), s.bufferMs)
	// The player is driven by reconcile (drivePlayerLocked) right after this returns,
	// picking up the new gen if we play our own group.
	e.p.Cluster.SetPlayback(s.groupID, s.playbackRecord(e.now(), e.p.Source.Stats()))
	e.lastBeat = e.now()
	e.log.Info("session renegotiated to pcm", "uri", s.uri, "gen", gen)
}

// uriScheme returns the lowercased scheme prefix of a media URI ("file" when
// none), for operator logging only.
func uriScheme(uri string) string {
	i := strings.IndexByte(uri, ':')
	if i <= 0 {
		return "file"
	}
	return strings.ToLower(uri[:i])
}

func hasCodec(codecs []string, want string) bool {
	for _, c := range codecs {
		if c == want {
			return true
		}
	}
	return false
}
