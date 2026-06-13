package cluster

import (
	"net/netip"

	"ensemble/internal/contracts"
	"ensemble/internal/discovery"
	"ensemble/internal/id"
)

// own returns this node's own record (always present after New). Caller holds mu.
func (c *Cluster) own() *NodeRecord {
	return c.doc.Nodes[c.self]
}

// bumpOwn increments our record version and stamps UpdatedAt. Caller holds mu.
func (c *Cluster) bumpOwn() *NodeRecord {
	r := c.own()
	r.Version++
	r.UpdatedAt = c.clock().Unix()
	return r
}

// broadcastOwn enqueues a node delta for our own record + notifies. Caller must
// NOT hold mu. snapshot is the record state to send (cloned under the lock).
func (c *Cluster) broadcastOwn(rec *NodeRecord) {
	c.log.Debug("broadcast own record", "version", rec.Version)
	c.enqueueBroadcast(kindNodeDelta, c.self, delta{Node: rec})
	c.notify()
}

// SetName renames this node (§1). No-op when unchanged.
func (c *Cluster) SetName(name string) {
	c.mu.Lock()
	if c.closed || c.own().Name == name {
		c.mu.Unlock()
		return
	}
	r := c.bumpOwn()
	r.Name = name
	snap := cloneNode(r)
	c.mu.Unlock()
	c.broadcastOwn(snap)
}

// SetSpotifyEndpoints replaces this node's Spotify Connect presets (D57) and
// gossips the change. The caller (api) supplies the already-normalized list from
// the config store, so C stores it verbatim (deep-cloned so the doc never aliases
// the caller's slices).
func (c *Cluster) SetSpotifyEndpoints(eps []contracts.SpotifyEndpoint) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	r := c.bumpOwn()
	r.SpotifyEndpoints = cloneEndpoints(eps)
	snap := cloneNode(r)
	c.mu.Unlock()
	c.broadcastOwn(snap)
}

// SetVolume sets this node's playback gain (D35). Caller clamps; C stores
// verbatim. No-op when unchanged.
func (c *Cluster) SetVolume(v float64) {
	c.mu.Lock()
	if c.closed || c.own().Volume == v {
		c.mu.Unlock()
		return
	}
	r := c.bumpOwn()
	r.Volume = v
	snap := cloneNode(r)
	c.mu.Unlock()
	c.broadcastOwn(snap)
}

// SetOutputDelayMs sets this node's output-delay calibration (D36). Caller
// clamps; C stores verbatim. No-op when unchanged.
func (c *Cluster) SetOutputDelayMs(ms int) {
	c.mu.Lock()
	if c.closed || c.own().OutputDelayMs == ms {
		c.mu.Unlock()
		return
	}
	r := c.bumpOwn()
	r.OutputDelayMs = ms
	snap := cloneNode(r)
	c.mu.Unlock()
	c.broadcastOwn(snap)
}

// SetOutputDevice sets this node's selected ALSA output device (D37). Caller
// validates against the enumerated list; C stores verbatim. No-op when unchanged.
func (c *Cluster) SetOutputDevice(device string) {
	c.mu.Lock()
	if c.closed || c.own().OutputDevice == device {
		c.mu.Unlock()
		return
	}
	r := c.bumpOwn()
	r.OutputDevice = device
	snap := cloneNode(r)
	c.mu.Unlock()
	c.broadcastOwn(snap)
}

// SetOutputBackend records this node's CHOSEN sink backend ("alsa"|"exec"|"null",
// §8.5), set once after the backend opens at boot. No-op when unchanged.
func (c *Cluster) SetOutputBackend(backend string) {
	c.mu.Lock()
	if c.closed || c.own().OutputBackend == backend {
		c.mu.Unlock()
		return
	}
	r := c.bumpOwn()
	r.OutputBackend = backend
	snap := cloneNode(r)
	c.mu.Unlock()
	c.broadcastOwn(snap)
}

// SetDisabled replaces this node's operator-disabled feature list (D40, a D14
// extension). Caller validates the subset; C stores verbatim and re-projects
// effective caps (probed − disabled) in the NodeView. No-op when unchanged.
func (c *Cluster) SetDisabled(disabled []string) {
	c.mu.Lock()
	if c.closed || equalStrings(c.own().Disabled, disabled) {
		c.mu.Unlock()
		return
	}
	r := c.bumpOwn()
	r.Disabled = append([]string(nil), disabled...)
	snap := cloneNode(r)
	c.mu.Unlock()
	c.broadcastOwn(snap)
}

// SetFollowing sets this node's following target (§5). id.Zero == solo master.
func (c *Cluster) SetFollowing(target id.ID) {
	c.mu.Lock()
	if c.closed || c.own().Following == target {
		c.mu.Unlock()
		return
	}
	r := c.bumpOwn()
	r.Following = target
	snap := cloneNode(r)
	c.mu.Unlock()
	c.broadcastOwn(snap)
}

// SetCapabilities replaces this node's reported capabilities (boot-time).
func (c *Cluster) SetCapabilities(caps contracts.Capabilities) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	r := c.bumpOwn()
	r.Caps = caps
	snap := cloneNode(r)
	c.mu.Unlock()
	c.broadcastOwn(snap)
}

// SetAddrs replaces self-reported interface CIDRs (§3.1).
func (c *Cluster) SetAddrs(cidrs []string) {
	c.mu.Lock()
	if c.closed || equalStrings(c.own().Addrs, cidrs) {
		c.mu.Unlock()
		return
	}
	r := c.bumpOwn()
	r.Addrs = append([]string(nil), cidrs...)
	snap := cloneNode(r)
	c.mu.Unlock()
	c.broadcastOwn(snap)
}

// SetPlayback writes the playback-status record for group (§4). Caller (H, the
// master) only writes groups it masters; C does not police. An empty
// Playback{State:"idle"} clears.
func (c *Cluster) SetPlayback(group id.ID, pb contracts.Playback) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	cur := c.doc.Playback[group]
	var ver uint64
	if cur != nil {
		ver = cur.Version
	}
	rec := &PlaybackRecord{
		State:       pb.State,
		URI:         pb.URI,
		StartedUnix: pb.StartedUnix,
		PositionSec: pb.PositionSec,
		Codec:       pb.Codec,
		Transport:   pb.Transport,
		Source:      pb.Source,
		Metadata:    pb.Metadata,
		QueueLen:    pb.QueueLen,
		QueueRev:    pb.QueueRev,
		Seekable:    pb.Seekable,
		Version:     ver + 1,
		UpdatedAt:   c.clock().Unix(),
		Writer:      c.self,
	}
	c.doc.Playback[group] = rec
	snap := *rec
	c.mu.Unlock()
	c.enqueueBroadcast(kindPlayback, group, delta{Group: group, Playback: &snap})
	c.notify()
}

// SetGroupName writes/renames a group (§4; any node may write, LWW).
func (c *Cluster) SetGroupName(group id.ID, name string) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	cur := c.doc.Groups[group]
	if cur != nil && cur.Name == name {
		c.mu.Unlock()
		return
	}
	var ver uint64
	if cur != nil {
		ver = cur.Version
	}
	rec := &GroupNameRecord{
		Name:      name,
		Version:   ver + 1,
		UpdatedAt: c.clock().Unix(),
		Writer:    c.self,
	}
	c.doc.Groups[group] = rec
	snap := *rec
	c.mu.Unlock()
	c.enqueueBroadcast(kindGroupName, group, delta{Group: group, Name: &snap})
	c.markDirty() // D41/D42: persist the override-names table
	c.notify()
}

// SetGroupSettings writes per-group codec/transport/bufferMs (LWW). Empty fields
// get contracts defaults.
func (c *Cluster) SetGroupSettings(group id.ID, s contracts.GroupSettings) {
	s = fillSettingsDefaults(s)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	cur := c.doc.Settings[group]
	var ver uint64
	if cur != nil {
		ver = cur.Version
	}
	rec := &GroupSettingsRecord{
		Codec:     s.Codec,
		Transport: s.Transport,
		BufferMs:  s.BufferMs,
		Version:   ver + 1,
		UpdatedAt: c.clock().Unix(),
		Writer:    c.self,
	}
	c.doc.Settings[group] = rec
	snap := *rec
	isOwn := group == c.self
	c.mu.Unlock()
	c.enqueueBroadcast(kindSettings, group, delta{Group: group, Settings: &snap})
	// D47: persist this node's OWN group-settings record (key == self id, D44:
	// group id == master id) so a restarting master re-forms its group with its
	// last settings. Other groups' settings are master-keyed live state owned by
	// other nodes — not ours to persist.
	if isOwn {
		c.markDirty()
	}
	c.notify()
}

// Observe records that we saw `peer` sending from IP `ip` now (§3.1). Fed by the
// HTTP layer and gossip events. Updates our own observed map; re-broadcasts at
// most once per observeBroadcastInterval per (peer, ip) to avoid churn.
func (c *Cluster) Observe(peer id.ID, ip netip.Addr) {
	if peer == c.self || !ip.IsValid() {
		return
	}
	ipStr := ip.String()
	now := c.clock()
	nowUnix := now.Unix()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	thr, seen := c.observed[peer]
	throttled := seen && thr.ip == ipStr && now.Sub(thr.lastBroadcast) < observeBroadcastInterval

	// Always keep our own NodeRecord.Observed map current for the resolved view.
	r := c.own()
	if r.Observed == nil {
		r.Observed = map[id.ID]obsEntry{}
	}
	r.Observed[peer] = obsEntry{IP: ipStr, LastSeenUnix: nowUnix}

	if throttled {
		// Update the in-memory map but do not bump version / broadcast.
		c.mu.Unlock()
		return
	}
	c.observed[peer] = obsThrottle{ip: ipStr, lastBroadcast: now}
	r.Version++
	r.UpdatedAt = nowUnix
	snap := cloneNode(r)
	c.mu.Unlock()
	c.broadcastOwn(snap)
}

// UpsertPlaybackNode injects/updates the proxied record for a discovered,
// non-gossiping playback node (D50/D59), from its mDNS advert. It is content-gated:
// the version bumps only when the advertised identity (control port, address,
// codecs) changes, so re-discovery on every mDNS hit does not churn. The
// operator-set Following (assignment, D59) and Name are preserved across updates.
//
// Slice-A scope: the proxied record is LOCAL to this master (not gossiped) — every
// master sees the same LAN mDNS, so they converge independently, and this sidesteps
// the multi-master proxy-version conflict until cross-master assignment lands.
// UpdatedAt is always refreshed so the record stays fresh against purge.
func (c *Cluster) UpsertPlaybackNode(p discovery.Peer) {
	if p.ID == c.self || p.ControlPort == 0 || !p.Addr.IsValid() {
		return
	}
	cidr := p.Addr.String() + "/32"
	if p.Addr.Is6() {
		cidr = p.Addr.String() + "/128"
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	now := c.clock().Unix()
	cur := c.doc.Nodes[p.ID]
	rec := &NodeRecord{
		ID:           p.ID,
		PlaybackNode: true,
		ControlPort:  p.ControlPort,
		Addrs:        []string{cidr},
		Name:         p.Name, // advertised name (the node's node.json) — first-discovery default
		Caps:         contracts.Capabilities{Playback: true, Codecs: append([]string(nil), p.Caps.Codecs...)},
		AppVersion:   p.AppVersion, // build version from mDNS advert (UI / version-skew check)
	}
	if cur != nil {
		rec.Following = cur.Following // preserve operator assignment (D59)
		rec.Name = cur.Name           // preserve a master-set label across re-discovery
		rec.Volume = cur.Volume
		rec.OutputDelayMs = cur.OutputDelayMs
		rec.Version = cur.Version
		if !playbackIdentityChanged(cur, rec) {
			cur.UpdatedAt = now // freshen against purge; no version bump, no notify
			c.mu.Unlock()
			return
		}
	}
	rec.Version++
	rec.UpdatedAt = now
	c.doc.Nodes[p.ID] = rec
	c.mu.Unlock()

	c.log.Info("playback node discovered", "id", p.ID, "control", p.ControlPort, "addr", p.Addr.String())
	c.notify()
}

// AssignPlaybackNode sets (target != Zero) or clears (Zero) the group assignment of
// a non-gossiping playback node (D59): it writes the proxied record's Following,
// which the driver (drive) and DeriveGroups both consume. Returns false (no-op) if
// the node is unknown, is NOT a playback node, or already has this assignment — a
// gossiping node owns its own Following and must use the follow API instead. Like
// the proxied record itself (Slice A), this stays local to the master.
func (c *Cluster) AssignPlaybackNode(node, target id.ID) bool {
	c.mu.Lock()
	r := c.doc.Nodes[node]
	if c.closed || r == nil || !r.PlaybackNode || r.Following == target {
		c.mu.Unlock()
		return false
	}
	r.Following = target
	r.Version++
	r.UpdatedAt = c.clock().Unix()
	c.mu.Unlock()
	c.log.Info("playback node assignment", "id", node, "target", target)
	c.notify()
	return true
}

// TouchPlaybackNode refreshes a proxied playback node's liveness (UpdatedAt) with no
// version bump or notify — called when the master receives the node's STATUS (D60).
// An actively-driven node sends STATUS ~1 Hz, so it stays alive even if its mDNS
// re-announce lapses past the freshness TTL. No-op for unknown / non-playback ids.
func (c *Cluster) TouchPlaybackNode(node id.ID) {
	c.mu.Lock()
	if r := c.doc.Nodes[node]; r != nil && r.PlaybackNode {
		r.UpdatedAt = c.clock().Unix()
	}
	c.mu.Unlock()
}

// PatchPlaybackNode mutates a proxied (non-gossiping) playback node's record
// master-side (D59): any of name / volume / output-delay / group assignment. A
// playback node has no HTTP API (D56), so the master owns these fields; the control
// driver pushes volume/delay (and ATTACH for the assignment) to the node over the
// control plane. Returns false only when the node is unknown or not a playback node;
// an in-range no-op still returns true. Stays local to the master (Slice A).
func (c *Cluster) PatchPlaybackNode(node id.ID, name *string, volume *float64, delayMs *int, following *id.ID) bool {
	c.mu.Lock()
	r := c.doc.Nodes[node]
	if c.closed || r == nil || !r.PlaybackNode {
		c.mu.Unlock()
		return false
	}
	changed := false
	if name != nil && r.Name != *name {
		r.Name = *name
		changed = true
	}
	if volume != nil && r.Volume != *volume {
		r.Volume = *volume
		changed = true
	}
	if delayMs != nil && r.OutputDelayMs != *delayMs {
		r.OutputDelayMs = *delayMs
		changed = true
	}
	if following != nil && r.Following != *following {
		r.Following = *following
		changed = true
	}
	if changed {
		r.Version++
		r.UpdatedAt = c.clock().Unix()
	}
	c.mu.Unlock()
	if changed {
		c.log.Info("playback node patched", "id", node)
		c.notify()
	}
	return true
}

// playbackIdentityChanged reports whether the advertised identity of a proxied
// playback node differs (control port, address, codecs). Following/Name/Version are
// proxy bookkeeping, not advertised identity, so they are excluded.
func playbackIdentityChanged(a, b *NodeRecord) bool {
	return a.ControlPort != b.ControlPort ||
		a.AppVersion != b.AppVersion || // an upgraded node re-advertises a new ver= → propagate
		!equalStrings(a.Addrs, b.Addrs) ||
		!equalStrings(a.Caps.Codecs, b.Caps.Codecs)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func fillSettingsDefaults(s contracts.GroupSettings) contracts.GroupSettings {
	if s.Codec == "" {
		s.Codec = contracts.DefaultCodec
	}
	if s.Transport == "" {
		s.Transport = contracts.DefaultTransport
	}
	if s.BufferMs == 0 {
		s.BufferMs = contracts.DefaultBufferMs
	}
	return s
}
