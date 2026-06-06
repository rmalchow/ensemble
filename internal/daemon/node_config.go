package daemon

// node_config.go implements the per-node Deps closures behind the 08 §D.2/§D.3
// surface: the full node-detail projection (record + cert fingerprint + gossip
// liveness + group membership/mastership) and the optimistic node-config patch.
// Both operate on the daemon's single persistent store; the patch gossips like
// every other ConfigDoc write, and the owning node's renderer picks the new
// lane (channel/gain/hwDelay) off the doc on its next control tick.

import (
	"errors"
	"net"
	"strconv"

	sink "gitlab.rand0m.me/ruben/go/ensemble/internal/audio/sink"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/cluster"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/web"
)

// membersView backs Deps.Members (C.2 discovery members[]): every ConfigDoc
// node joined with gossip liveness and — when live — the member's observed
// control endpoint as host:port (the address an operator can actually open).
// Offline members fall back to their durable record addrs (their control port
// is unknowable while they are gone). Self is always online and lists its own
// routable addrs with the actual bound web port.
func (n *Node) membersView() []web.MemberView {
	doc := n.store.Get()
	n.sessMu.Lock()
	cp := n.plane
	n.sessMu.Unlock()

	live := map[string]cluster.Member{}
	if cp != nil && cp.mem != nil {
		for _, m := range cp.mem.Members() {
			live[m.Meta.NodeID] = m
		}
	}

	out := make([]web.MemberView, 0, len(doc.Nodes))
	for _, nr := range doc.Nodes {
		v := web.MemberView{NodeView: nodeView(nr)}
		switch {
		case nr.ID == n.options.NodeID:
			v.Online = true
			// Older docs (pre-addr-seeding genesis) may have no record addrs for
			// the founder; fall back to the live host IPs so the row is never "—".
			ips := nr.Addrs
			if len(ips) == 0 {
				ips = nonLoopbackIPStrings()
			}
			if n.webPort > 0 {
				v.Addrs = addrsWithPort(ips, n.webPort)
			} else if len(nr.Addrs) == 0 {
				v.Addrs = nonNil(ips)
			}
		default:
			if m, ok := live[nr.ID]; ok {
				v.Online = true
				if m.Meta.WebPort > 0 {
					v.Addrs = []string{m.WebAddr()}
				}
			}
		}
		out = append(out, v)
	}
	return out
}

// syncSelfAddrs keeps THIS node's own NodeRecord.Addrs current with the host's
// actual routable IPs (cur): a node that reappears on a different address
// (DHCP renumber, interface change, move) updates its own durable record and
// gossips it; peers' allowlists and fallback addressing follow, and the
// allowlist's LIVE half covers the transition window. Each node owns only its
// own addr facts — no peer ever rewrites another's record, so there is no
// write contention. Best-effort: a version conflict is simply retried on the
// next loop tick. No-op when the record is absent or already current.
func (n *Node) syncSelfAddrs(cur []string) {
	if len(cur) == 0 {
		return // no usable interface info; never wipe the record
	}
	doc := n.store.Get()
	i := indexNode(doc.Nodes, n.options.NodeID)
	if i < 0 || equalStringSets(doc.Nodes[i].Addrs, cur) {
		return
	}
	doc.Nodes[i].Addrs = cur
	_, _ = n.store.Apply(doc)
}

// syncSelfDevices keeps THIS node's own NodeRecord.AudioDevices current with
// the host's enumerated playback devices (cur) — the selectable choices any
// member's UI offers for this node's Device override. Pure file-read
// enumeration (no device opens), published like syncSelfAddrs: each node owns
// its own hardware facts, gossip distributes them. A host with NO devices
// publishes the empty list (an unplugged USB card disappears from the picker).
func (n *Node) syncSelfDevices(cur []state.AudioDevice) {
	doc := n.store.Get()
	i := indexNode(doc.Nodes, n.options.NodeID)
	if i < 0 || equalAudioDevices(doc.Nodes[i].AudioDevices, cur) {
		return
	}
	doc.Nodes[i].AudioDevices = cur
	_, _ = n.store.Apply(doc)
}

// equalAudioDevices reports whether a and b list the same devices in order
// (enumeration order is deterministic, so a plain compare suffices).
func equalAudioDevices(a, b []state.AudioDevice) bool {
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

// probedAudioDevices adapts the sink enumeration to the state record shape.
func probedAudioDevices() []state.AudioDevice {
	devs := sink.ListPlaybackDevices()
	out := make([]state.AudioDevice, 0, len(devs))
	for _, d := range devs {
		out = append(out, state.AudioDevice{ID: d.ID, Label: d.Label})
	}
	return out
}

// syncSelfRender is the 06 §1.5 LAST-RESORT control-only fallback: when none of
// the audio sink tiers (alsalib → alsa → exec players) is usable on this host,
// the node AUTO-disables its own Caps.Render (and marks RenderAutoOff) so the
// group engine runs it as a control/media-only member instead of silently
// failing every render start. When a sink becomes usable again, an auto-
// disabled node flips Render back on. An operator's explicit Render=false
// (RenderAutoOff unset) is never overridden. usable=true short-circuits with no
// doc read when the record already agrees.
func (n *Node) syncSelfRender(usable bool) {
	doc := n.store.Get()
	i := indexNode(doc.Nodes, n.options.NodeID)
	if i < 0 {
		return
	}
	nr := &doc.Nodes[i]
	switch {
	case !usable && nr.Caps.Render:
		nr.Caps.Render = false
		nr.RenderAutoOff = true
		logf(n.options.Log, "audio: no usable sink — falling back to control-only (Caps.Render=false)")
	case usable && !nr.Caps.Render && nr.RenderAutoOff:
		nr.Caps.Render = true
		nr.RenderAutoOff = false
		logf(n.options.Log, "audio: sink usable again — re-enabling render")
	default:
		return // already consistent (or operator-forced off)
	}
	_, _ = n.store.Apply(doc)
}

// sinkUsable probes the backend chain for THIS node's resolved device. A test
// node with an injected OpenSink is always usable (the fake IS the device).
func (n *Node) sinkUsable() bool {
	if n.options.OpenSink != nil {
		return true
	}
	device := n.options.Device
	if nr := nodeRecord(n.store.Get(), n.options.NodeID); nr != nil && nr.Device != "" {
		device = nr.Device
	}
	return len(sink.Probe(sink.ProbeConfig{Device: device})) > 0
}

// equalStringSets reports whether a and b hold the same strings (order-blind).
func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, s := range a {
		set[s] = true
	}
	for _, s := range b {
		if !set[s] {
			return false
		}
	}
	return true
}

// addrsWithPort renders each IP with the given port (host:port). An empty input
// yields an empty slice (never nil — JSON []).
func addrsWithPort(ips []string, port int) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, net.JoinHostPort(ip, strconv.Itoa(port)))
	}
	return out
}

// nodeDetail backs Deps.NodeDetail (08 §D.2): the NodeView record joined with
// the cert fingerprint, gossip liveness and group facts. ok=false => unknown id.
func (n *Node) nodeDetail(id string) (web.NodeDetailView, bool) {
	doc := n.store.Get()
	nr := nodeRecord(doc, id)
	if nr == nil {
		return web.NodeDetailView{}, false
	}
	v := web.NodeDetailView{NodeView: nodeView(*nr)}
	if fp := certFingerprint(nr.CertPEM); fp != "" {
		v.Fingerprint = "sha256:" + fp
	}
	v.CertSignedByCA = nr.CertPEM != ""
	for _, g := range doc.Groups {
		for _, mid := range g.MemberNodeIDs {
			if mid == id {
				v.GroupID = g.ID
				break
			}
		}
	}

	// Liveness + mastership from the live plane; the doc-derived election is the
	// solo fallback. Self is always online (we are answering this request).
	v.Online = id == n.options.NodeID
	n.sessMu.Lock()
	cp := n.plane
	n.sessMu.Unlock()
	master := ""
	if cp != nil {
		if cp.mem != nil {
			for _, m := range cp.mem.Members() {
				if m.Meta.NodeID == id {
					v.Online = true
					break
				}
			}
			master = cp.elections.Master(v.GroupID)
		}
	}
	if master == "" && v.GroupID != "" {
		master = masterOf(doc, v.GroupID, n.options.NodeID)
	}
	v.IsMaster = master != "" && master == id
	return v, true
}

// setNodeConfig backs Deps.SetNodeConfig (08 §D.3): apply a partial node-config
// patch under optimistic concurrency at ifMatch. The handler has validated the
// channel enum; this owns the doc write. A version conflict surfaces as
// web.ErrVersionConflict (409), an unknown id as web.ErrNotFound (404).
func (n *Node) setNodeConfig(nodeID string, patch web.NodePatch, ifMatch uint64) error {
	doc := n.store.Get()
	if doc.Version != ifMatch {
		return web.ErrVersionConflict
	}
	i := indexNode(doc.Nodes, nodeID)
	if i < 0 {
		return web.ErrNotFound
	}
	nr := &doc.Nodes[i]
	if patch.Name != nil && *patch.Name != "" {
		nr.Name = *patch.Name
	}
	if patch.Channel != nil {
		nr.Channel = *patch.Channel
	}
	if patch.HWDelayUs != nil {
		nr.HWDelayUs = *patch.HWDelayUs
	}
	if patch.GainDB != nil {
		nr.GainDB = *patch.GainDB
	}
	if patch.Device != nil {
		nr.Device = *patch.Device // "" clears back to auto
	}
	if _, err := n.store.Apply(doc); err != nil {
		if errors.Is(err, state.ErrConflict) {
			return web.ErrVersionConflict
		}
		return err
	}
	return nil
}
