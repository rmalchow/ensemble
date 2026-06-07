package group

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"ensemble/internal/contracts"
	"ensemble/internal/dl"
	"ensemble/internal/id"
	"ensemble/internal/stream"
)

// clockWaitTimeout bounds how long Play waits for the local clock follower to
// sync before stamping pts (§7). The master follows localhost and syncs ~1 s.
const clockWaitTimeout = 2 * time.Second

// clockWaitPoll is the retry cadence while waiting for clock sync.
const clockWaitPoll = 50 * time.Millisecond

// Play starts playback of uri to this node's group (§6/§8.2). Master-only:
// returns ErrNotMaster if this node is a follower. Opens the media source via
// the factory, validates codec/opus capability, bumps the generation, starts the
// source session, writes playing status, and spawns the release loop. Replaces
// any running session first (§8.6).
func (e *Engine) Play(uri string) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	snap := e.p.Cluster.Snapshot()
	mv := myGroup(snap, e.self)
	if !mv.found || mv.role == roleFollower {
		e.mu.Unlock()
		return ErrNotMaster
	}
	groupID := mv.group.ID
	settings := fillDefaults(mv.group.Settings)

	// Opus capability gating BEFORE consuming a generation (§8.3/D33).
	if settings.Codec == "opus" {
		if err := e.validateOpusGroup(snap, mv); err != nil {
			e.mu.Unlock()
			return err
		}
	}
	e.mu.Unlock()

	// Open the media source (no lock — may block on http/file). On error: no gen,
	// no status.
	e.log.Info("opening source", "uri", uri, "scheme", uriScheme(uri), "codec", settings.Codec, "transport", settings.Transport)
	src, err := e.p.Media.Open(uri)
	if err != nil {
		e.log.Warn("source open failed", "uri", uri, "scheme", uriScheme(uri), "err", err)
		return err
	}
	live := src.Live()
	pacing := "pull"
	if live {
		pacing = "live"
	}
	e.log.Info("source opened", "uri", uri, "scheme", uriScheme(uri), "pacing", pacing)

	// Opus encoder (D33): master encodes once for all subscribers.
	var enc OpusEncoder
	if settings.Codec == "opus" {
		if e.p.Opus == nil {
			_ = src.Close()
			return ErrNoOpus
		}
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
		enc:         enc,
		startMaster: startMaster + int64(e.p.LeadMs)*1_000_000,
		startedUnix: e.now().Unix(),
		transport:   settings.Transport,
		bufferMs:    settings.BufferMs,
		leadMs:      e.p.LeadMs,
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		onEnd:       e.onSessionEnd,
		now:         e.now,
	}
	sess.gen.Store(gen)
	e.sess = sess

	e.p.Source.StartSession(gen, stream.ParseTransport(settings.Transport), settings.BufferMs)
	e.log.Info("playback started",
		"uri", uri, "gen", gen, "codec", settings.Codec, "transport", settings.Transport,
		"bufferMs", settings.BufferMs, "leadMs", e.p.LeadMs, "live", live)

	// Re-point THIS node's own plumbing at itself as master for gen so the master
	// hears its own stream immediately (§8.2 — no special handling).
	e.repointLocked(mv.master, gen, settings.Transport, true)

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

// waitForClock returns LocalToMaster(now) once the clock is synced, retrying up
// to clockWaitTimeout. ok=false if it never syncs (transient ErrNotSynced).
func (e *Engine) waitForClock() (masterNanos int64, ok bool) {
	deadline := e.now().Add(clockWaitTimeout)
	for {
		if m, k := e.p.Clock.LocalToMaster(time.Now().UnixNano()); k {
			return m, true
		}
		if !e.now().Before(deadline) {
			return 0, false
		}
		time.Sleep(clockWaitPoll)
	}
}

// validateOpusGroup checks every current group member reports the opus codec
// capability (§8.3/D33), rejecting with ErrNoOpus naming the lacking nodes.
// Caller holds e.mu.
func (e *Engine) validateOpusGroup(snap contracts.Snapshot, mv myView) error {
	byID := make(map[id.ID]contracts.NodeView, len(snap.Nodes))
	for _, n := range snap.Nodes {
		byID[n.ID] = n
	}
	var lacking []string
	for _, m := range mv.group.Members {
		n, ok := byID[m]
		if !ok || !hasCodec(n.Capabilities.Codecs, "opus") {
			name := m.String()
			if ok && n.Name != "" {
				name = n.Name
			}
			lacking = append(lacking, name)
		}
	}
	if len(lacking) > 0 {
		return fmt.Errorf("%w: %s", ErrNoOpus, strings.Join(lacking, ", "))
	}
	return nil
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
