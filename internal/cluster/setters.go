package cluster

import (
	"net/netip"

	"ensemble/internal/contracts"
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
	c.markDirty() // D41: persist the names table
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
	c.mu.Unlock()
	c.enqueueBroadcast(kindSettings, group, delta{Group: group, Settings: &snap})
	c.markDirty() // D41: persist the settings table
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
