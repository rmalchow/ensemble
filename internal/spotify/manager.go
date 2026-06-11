package spotify

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"

	"ensemble/internal/audio"
	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// Engine is the slice of the group engine the manager drives (D57). Implemented
// by *group.Engine via main's adapter; structurally satisfied here so the spotify
// package needn't import group.
type Engine interface {
	Play(uri string) error
	Stop() error
	RefreshPlayback()
	// Follow/Unfollow drive THIS node's own player (D49) — used to (un)join the
	// master's own speaker to its group when it's a selected preset player.
	Follow(target id.ID) error
	Unfollow() error
}

// Cluster is the slice of the cluster the manager reads + writes to regroup a
// preset's players. Implemented by *cluster.Cluster.
type Cluster interface {
	Self() id.ID
	Snapshot() contracts.Snapshot
	AssignPlaybackNode(node, target id.ID) bool
}

// Manager owns every go-librespot bridge on this node (D57): the implicit default
// endpoint ("ensemble <node>") plus one bridge per configured preset ("ensemble
// <node>: <name>"). All bridges run concurrently (every Connect device is
// discoverable), but only ONE endpoint plays at a time — the active one drives the
// node's group membership and the engine's single session, preempting any other.
type Manager struct {
	bin     string
	dataDir string
	apiBase int
	engine  Engine
	cluster Cluster
	log     *slog.Logger

	mu        sync.Mutex
	ctx       context.Context
	started   bool
	nodeName  string
	bridges   map[string]*managed // by endpoint id; "" == default
	usedPorts map[int]bool
	active    string // endpoint id currently playing
	hasActive bool
}

type managed struct {
	ep     contracts.SpotifyEndpoint // zero value for the default endpoint
	bridge *Bridge
	port   int
}

// NewManager builds the manager (no processes yet — call Start). bin is the
// resolved go-librespot path; dataDir is the node data dir (the default endpoint's
// go-librespot auth lives directly under it, presets under dataDir/spotify/<id>).
func NewManager(bin, dataDir, nodeName string, engine Engine, cluster Cluster, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		bin:       bin,
		dataDir:   dataDir,
		apiBase:   DefaultAPIPort,
		engine:    engine,
		cluster:   cluster,
		log:       log.With("comp", "spotify-mgr"),
		nodeName:  nodeName,
		bridges:   map[string]*managed{},
		usedPorts: map[int]bool{},
	}
}

// Start launches the default endpoint plus the given presets. Non-blocking.
func (m *Manager) Start(ctx context.Context, endpoints []contracts.SpotifyEndpoint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ctx = ctx
	m.started = true
	// Default endpoint first — same behavior as before this feature.
	if _, ok := m.bridges[""]; !ok {
		m.startLocked("", contracts.SpotifyEndpoint{})
	}
	m.reconcilePresetsLocked(endpoints)
}

// Reconcile updates the running presets to match endpoints (add/remove/rename;
// player-set changes apply on the next play, no restart). Safe before Start (it
// just records intent until Start runs).
func (m *Manager) Reconcile(endpoints []contracts.SpotifyEndpoint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started {
		return
	}
	m.reconcilePresetsLocked(endpoints)
}

// reconcilePresetsLocked diffs the desired preset set against running bridges.
// The default endpoint ("") is never touched here.
func (m *Manager) reconcilePresetsLocked(endpoints []contracts.SpotifyEndpoint) {
	want := map[string]contracts.SpotifyEndpoint{}
	for _, ep := range endpoints {
		if ep.ID == "" {
			continue // defensive: normalized configs always carry an id
		}
		want[ep.ID] = ep
	}
	// Stop bridges no longer wanted.
	for eid, mg := range m.bridges {
		if eid == "" {
			continue
		}
		if _, ok := want[eid]; !ok {
			m.stopLocked(eid)
			_ = mg
		}
	}
	// Add new / rename changed / update player set.
	for eid, ep := range want {
		if mg, ok := m.bridges[eid]; ok {
			if mg.ep.Name != ep.Name {
				// Restart so the new Connect name is re-advertised over zeroconf —
				// go-librespot's set_device_name does not refresh the live advert,
				// and the config dir persists auth so there is no re-login.
				m.stopLocked(eid)
				m.startLocked(eid, ep)
			} else {
				mg.ep = ep // player-set change only — applies on the next play, no restart
			}
			continue
		}
		m.startLocked(eid, ep)
	}
}

// startLocked creates, registers and runs one bridge. Caller holds mu.
func (m *Manager) startLocked(eid string, ep contracts.SpotifyEndpoint) {
	port := m.allocPortLocked()
	stateDir := m.dataDir
	if eid != "" {
		stateDir = filepath.Join(m.dataDir, "spotify", eid)
	}
	b, err := New(Config{
		BinPath:    m.bin,
		DeviceName: m.deviceName(eid, ep.Name),
		StateDir:   stateDir,
		APIPort:    port,
		Log:        m.log,
		OnPlay:     func() { m.onPlay(eid) },
		OnStop:     func() { m.onStop(eid) },
		OnMetadata: func() { m.onMetadata(eid) },
	})
	if err != nil {
		m.log.Warn("spotify endpoint disabled", "endpoint", eid, "err", err)
		m.usedPorts[port] = false
		return
	}
	audio.RegisterSpotifyEndpoint(eid, b.Attach, b.Latest, b.Position)
	if err := b.Run(m.ctx); err != nil {
		m.log.Warn("spotify endpoint start failed", "endpoint", eid, "err", err)
		audio.UnregisterSpotifyEndpoint(eid)
		m.usedPorts[port] = false
		return
	}
	m.bridges[eid] = &managed{ep: ep, bridge: b, port: port}
}

// stopLocked tears down one bridge. Caller holds mu.
func (m *Manager) stopLocked(eid string) {
	mg, ok := m.bridges[eid]
	if !ok {
		return
	}
	audio.UnregisterSpotifyEndpoint(eid)
	_ = mg.bridge.Close()
	delete(m.bridges, eid)
	delete(m.usedPorts, mg.port)
	if m.hasActive && m.active == eid {
		m.hasActive = false
	}
}

// Rename updates the node name → restarts every bridge so each Connect device
// re-advertises its new "ensemble <node>[: <preset>]" name over zeroconf (a live
// set_device_name doesn't refresh the advert; the config dir keeps auth, so no
// re-login). Node renames are rare, so the brief re-advertise blip is acceptable.
func (m *Manager) Rename(nodeName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started {
		m.nodeName = nodeName
		return
	}
	m.nodeName = nodeName
	ids := make([]string, 0, len(m.bridges))
	for eid := range m.bridges {
		ids = append(ids, eid)
	}
	for _, eid := range ids {
		ep := m.bridges[eid].ep
		m.stopLocked(eid)
		m.startLocked(eid, ep)
	}
}

// Close stops every bridge. Idempotent.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for eid := range m.bridges {
		audio.UnregisterSpotifyEndpoint(eid)
		_ = m.bridges[eid].bridge.Close()
	}
	m.bridges = map[string]*managed{}
	m.usedPorts = map[int]bool{}
	m.hasActive = false
	m.started = false
	return nil
}

// Deactivate pushes ensemble's own stop / source-switch back to the controlling
// phone: it pauses (disconnect=false, used when another source is selected) or
// stops+disconnects (disconnect=true, used on an explicit Stop) the currently
// active endpoint's go-librespot. No-op when no Spotify endpoint is active.
//
// Without this, go-librespot keeps its Connect session alive after ensemble
// leaves it: the phone keeps auto-advancing tracks, each re-emitting a "playing"
// event that bounces ensemble straight back to Spotify. Clearing hasActive BEFORE
// sending is what breaks the loop — the resulting paused/stopped event reaches
// onStop with hasActive already false, so it issues no second engine.Stop (which
// could kill the source the user just switched to), and no further "playing"
// events flow once go-librespot is paused/stopped.
func (m *Manager) Deactivate(disconnect bool) {
	m.mu.Lock()
	if !m.hasActive {
		m.mu.Unlock()
		return
	}
	mg := m.bridges[m.active]
	m.hasActive = false
	m.mu.Unlock()
	if mg == nil {
		return
	}
	action := "pause"
	if disconnect {
		action = "stop"
	}
	if err := mg.bridge.playerCommand(action); err != nil {
		m.log.Debug("spotify deactivate failed", "action", action, "err", err)
	}
}

// ---- orchestration callbacks (fired from each bridge's event goroutine) -------

func (m *Manager) onPlay(eid string) {
	m.mu.Lock()
	mg, ok := m.bridges[eid]
	if !ok {
		m.mu.Unlock()
		return
	}
	players := append([]id.ID(nil), mg.ep.Players...)
	switching := m.hasActive && m.active != eid
	m.active = eid
	m.hasActive = true
	m.mu.Unlock()

	// The default endpoint preserves the legacy behavior: play to whatever group
	// this node already masters, no regrouping. engine.Play stops any running
	// session itself, so the default path is a plain (re)start.
	if eid == "" {
		_ = m.engine.Play("spotify:")
		return
	}

	// Preset: do a CLEAN HANDOFF when switching from another endpoint. Stop +
	// disconnect the running stream FIRST (so its subscribers BYE immediately),
	// THEN regroup to this preset's players, THEN start attached to the new
	// stream. Regrouping between stop and start prevents the old and new streams
	// from briefly overlapping on shared/moved speakers ("two streams fighting").
	if switching {
		_ = m.engine.Stop()
	}
	m.setGroupMembers(players)
	_ = m.engine.Play("spotify:" + eid)
}

func (m *Manager) onStop(eid string) {
	m.mu.Lock()
	stop := m.hasActive && m.active == eid
	if stop {
		m.hasActive = false
	}
	m.mu.Unlock()
	if stop {
		_ = m.engine.Stop() // membership left intact ("stay grouped")
	}
}

func (m *Manager) onMetadata(eid string) {
	m.mu.Lock()
	refresh := m.hasActive && m.active == eid
	m.mu.Unlock()
	if refresh {
		m.engine.RefreshPlayback()
	}
}

// setGroupMembers sets this master's playback-node membership EXACTLY to players:
// the listed playback nodes join this node's group; any playback node currently in
// it that isn't listed leaves. Gossiping nodes self-own their membership and are
// left untouched (the UI picker leads with playback nodes).
func (m *Manager) setGroupMembers(players []id.ID) {
	self := m.cluster.Self()
	snap := m.cluster.Snapshot()
	want := make(map[id.ID]bool, len(players))
	for _, p := range players {
		want[p] = true
	}

	// This node's OWN player is a gossiping node, not a playback node — it joins
	// its group by following itself (the engine's canonical path), not via
	// AssignPlaybackNode. Only unfollow when WE currently follow ourselves, so a
	// crosswise follow of another group is left undisturbed.
	if want[self] {
		_ = m.engine.Follow(self)
	} else if nv, ok := nodeByID(snap, self); ok && nv.Following == self {
		_ = m.engine.Unfollow()
	}

	// Join wanted non-gossiping playback nodes.
	for _, p := range players {
		if p == self {
			continue
		}
		if nv, ok := nodeByID(snap, p); ok && nv.PlaybackNode {
			m.cluster.AssignPlaybackNode(p, self)
		}
	}
	// Unjoin playback nodes in our group that aren't wanted.
	for _, nv := range snap.Nodes {
		if nv.PlaybackNode && nv.Following == self && !want[nv.ID] {
			m.cluster.AssignPlaybackNode(nv.ID, id.Zero)
		}
	}
}

// deviceName builds the advertised Connect device name. Caller holds mu (reads
// m.nodeName) for the non-Rename callers; Rename reads it under its own lock.
func (m *Manager) deviceName(eid, name string) string {
	base := "ensemble " + m.nodeName
	if eid == "" {
		return base
	}
	return base + ": " + name
}

// allocPortLocked returns the lowest free localhost API port at/after apiBase.
func (m *Manager) allocPortLocked() int {
	for p := m.apiBase; ; p++ {
		if !m.usedPorts[p] {
			m.usedPorts[p] = true
			return p
		}
	}
}

func nodeByID(snap contracts.Snapshot, nid id.ID) (contracts.NodeView, bool) {
	for _, n := range snap.Nodes {
		if n.ID == nid {
			return n, true
		}
	}
	return contracts.NodeView{}, false
}
