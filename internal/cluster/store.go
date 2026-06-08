package cluster

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// Snapshot returns a deep-copied, resolved, JSON-ready view: every node record
// joined with liveness + staleness, plus derived groups (§5). Holds the mutex
// only for the copy; derives outside it. Lock order: copy doc under c.mu,
// release, then read liveness (never both at once).
func (c *Cluster) Snapshot() contracts.Snapshot {
	c.mu.Lock()
	doc := c.doc.clone()
	c.mu.Unlock()

	alive, seen := c.live.snapshot()
	nowUnix := c.clock().Unix()

	nodes := make([]contracts.NodeView, 0, len(doc.Nodes))
	for nid, r := range doc.Nodes {
		nv := nodeView(nid, r, alive, seen, nowUnix)
		if nid == c.self {
			// This process IS the node: memberlist never gossips us to
			// ourselves, so liveness/staleness must not depend on it.
			nv.Alive = true
			nv.LastSeenUnix = nowUnix
			nv.Stale = false
		}
		nodes = append(nodes, nv)
	}
	sort.Slice(nodes, func(i, j int) bool { return idLess(nodes[i].ID, nodes[j].ID) })

	groups := DeriveGroups(doc.Nodes, doc.Groups, doc.Playback, doc.Settings, alive, c.self)

	return contracts.Snapshot{Nodes: nodes, Groups: groups}
}

func nodeView(nid id.ID, r *NodeRecord, alive map[id.ID]bool, seen map[id.ID]int64, nowUnix int64) contracts.NodeView {
	obs := make(map[id.ID]contracts.Observed, len(r.Observed))
	for k, v := range r.Observed {
		obs[k] = contracts.Observed{IP: v.IP, LastSeenUnix: v.LastSeenUnix}
	}
	lastSeen := seen[nid]
	return contracts.NodeView{
		ID:            r.ID,
		Name:          r.Name,
		Volume:        r.Volume,
		OutputDelayMs: r.OutputDelayMs,
		OutputDevice:  r.OutputDevice,
		OutputDevices: append([]contracts.OutputDevice(nil), r.OutputDevices...),
		InputDevices:  append([]contracts.InputDevice(nil), r.InputDevices...),
		Addrs:         append([]string(nil), r.Addrs...),
		HTTPPort:      r.HTTPPort,
		StreamPort:    r.StreamPort,
		SourcePort:    r.SourcePort,
		GossipPort:    r.GossipPort,
		Capabilities:  effectiveCaps(r.Caps, r.Disabled),
		Disabled:      append([]string(nil), r.Disabled...),
		Following:     r.Following,
		Observed:      obs,
		Alive:         alive[nid],
		LastSeenUnix:  lastSeen,
		Stale:         lastSeen != 0 && nowUnix-lastSeen > int64(staleAfter.Seconds()),
		UpdatedAt:     r.UpdatedAt,
		Version:       r.Version,
	}
}

// effectiveCaps subtracts the operator-disabled features (D40) from the node's
// PROBED capabilities — the single place the subtraction happens. Disabling:
//   - "playback" → Playback:false (the sink swaps to the null backend live, K);
//   - "opus"     → "opus" removed from Codecs (master-side D33 validation then
//     rejects opus sessions including this node; the local constructors refuse);
//   - "input"    → "input" removed from Sources.
//
// Backends/Formats are unaffected. The probed caps are never mutated (a copy is
// returned) so re-enabling restores them.
func effectiveCaps(probed contracts.Capabilities, disabled []string) contracts.Capabilities {
	if len(disabled) == 0 {
		return probed
	}
	off := map[string]bool{}
	for _, d := range disabled {
		off[d] = true
	}
	eff := probed
	if off["playback"] {
		eff.Playback = false
	}
	if off["opus"] {
		eff.Codecs = without(probed.Codecs, "opus")
	}
	if off["input"] {
		eff.Sources = without(probed.Sources, "input")
	}
	return eff
}

// without returns a copy of in with every occurrence of v removed.
func without(in []string, v string) []string {
	out := make([]string, 0, len(in))
	for _, x := range in {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// DeriveGroups projects derived groups (§5) from a document snapshot and a
// liveness view. Pure; no writes. Exported so the group engine (H) reuses the
// exact same rule C uses for Snapshot. self is included as always-alive.
func DeriveGroups(
	nodes map[id.ID]*NodeRecord,
	names map[id.ID]*GroupNameRecord,
	playback map[id.ID]*PlaybackRecord,
	settings map[id.ID]*GroupSettingsRecord,
	alive map[id.ID]bool,
	self id.ID,
) []contracts.GroupView {
	isAlive := func(n id.ID) bool { return n == self || alive[n] }

	// A node is a master iff alive and (Following == Zero, or Following points
	// at a dead/unknown node, or at a node that is itself following someone).
	isMaster := func(n id.ID, r *NodeRecord) bool {
		if !isAlive(n) {
			return false
		}
		if r.Following.IsZero() {
			return true
		}
		tgt, ok := nodes[r.Following]
		if !ok || !isAlive(r.Following) {
			return true // dead/unknown master → behaves as solo
		}
		if !tgt.Following.IsZero() {
			return true // following a follower → behaves as solo
		}
		return false
	}

	// Build member sets keyed by master.
	members := map[id.ID][]id.ID{}
	for nid, r := range nodes {
		if !isAlive(nid) {
			continue // dead nodes are not part of any derived group (§5)
		}
		if isMaster(nid, r) {
			if _, ok := members[nid]; !ok {
				members[nid] = []id.ID{nid}
			} else {
				// already seeded as a follower-master collision; ensure self present
				members[nid] = appendUnique(members[nid], nid)
			}
			continue
		}
		// follower: attach to its master if that master is itself a master.
		m := r.Following
		mr, ok := nodes[m]
		if !ok || !isAlive(m) || !isMaster(m, mr) {
			// stale follow → projected solo
			members[nid] = appendUnique(members[nid], nid)
			continue
		}
		members[m] = appendUnique(members[m], m, nid)
	}

	groups := make([]contracts.GroupView, 0, len(members))
	for master, mem := range members {
		sort.Slice(mem, func(i, j int) bool { return idLess(mem[i], mem[j]) })
		// D42: the group id is the MASTER's node id — playback + settings records
		// are keyed by it, so membership churn no longer orphans them. The explicit
		// name OVERRIDE map stays keyed by the member-set XOR (an override names a
		// specific COMBINATION of rooms; survives master changes + re-forming).
		gid := master
		memberXOR := id.XOR(mem...)
		gv := contracts.GroupView{
			ID:       gid,
			Master:   master,
			Members:  mem,
			Settings: resolveSettings(settings[gid]),
			Playback: resolvePlayback(playback[gid]),
		}
		if nm := names[memberXOR]; nm != nil && nm.Name != "" {
			gv.Name = nm.Name
			gv.NameIsDerived = false
		} else {
			gv.Name = derivedLabel(mem, nodes)
			gv.NameIsDerived = true
		}
		groups = append(groups, gv)
	}
	sort.Slice(groups, func(i, j int) bool { return idLess(groups[i].ID, groups[j].ID) })
	return groups
}

// derivedLabelMax caps how many member names a derived label spells out before
// summarising the remainder as " +N more" (§5).
const derivedLabelMax = 3

// derivedLabel computes the server-side DERIVED group label (§5/D42): the sorted
// member NAMES joined with " + ". A member missing from the snapshot falls back
// to its 8-char short id. A solo group (one member) is just that node's name.
// More than derivedLabelMax names are truncated to the first few then " +N more"
// (e.g. "bedroom + kitchen + living room +2 more"). mem is already id-sorted, so
// the label is stable; the NAMES themselves are then sorted for a deterministic,
// master-independent label.
func derivedLabel(mem []id.ID, nodes map[id.ID]*NodeRecord) string {
	names := make([]string, 0, len(mem))
	for _, m := range mem {
		if r, ok := nodes[m]; ok && r.Name != "" {
			names = append(names, r.Name)
		} else {
			names = append(names, m.String()[:8])
		}
	}
	sort.Strings(names)
	if len(names) <= derivedLabelMax {
		return strings.Join(names, " + ")
	}
	shown := strings.Join(names[:derivedLabelMax], " + ")
	return fmt.Sprintf("%s +%d more", shown, len(names)-derivedLabelMax)
}

func appendUnique(s []id.ID, vs ...id.ID) []id.ID {
	for _, v := range vs {
		found := false
		for _, x := range s {
			if x == v {
				found = true
				break
			}
		}
		if !found {
			s = append(s, v)
		}
	}
	return s
}

func resolveSettings(r *GroupSettingsRecord) contracts.GroupSettings {
	if r == nil {
		return contracts.GroupSettings{
			Codec:     contracts.DefaultCodec,
			Transport: contracts.DefaultTransport,
			BufferMs:  contracts.DefaultBufferMs,
		}
	}
	return fillSettingsDefaults(contracts.GroupSettings{
		Codec:     r.Codec,
		Transport: r.Transport,
		BufferMs:  r.BufferMs,
	})
}

func resolvePlayback(r *PlaybackRecord) contracts.Playback {
	if r == nil {
		return contracts.Playback{State: "idle"}
	}
	state := r.State
	if state == "" {
		state = "idle"
	}
	return contracts.Playback{
		State:       state,
		URI:         r.URI,
		StartedUnix: r.StartedUnix,
		PositionSec: r.PositionSec,
		Codec:       r.Codec,
		Transport:   r.Transport,
		Source:      r.Source,
	}
}

// DialCandidates returns candidate IPs for reaching peer, best-first (§3.1): the
// peer's self-reported CIDR IPs INTERSECTED with the IPs ANY node has observed
// for it, most-recently-observed first. If the intersection is empty (cold
// peer), falls back to the peer's self-reported IPs (D6). Caller appends the
// relevant port from the same NodeView.
func (c *Cluster) DialCandidates(peer id.ID) []netip.Addr {
	c.mu.Lock()
	doc := c.doc.clone()
	c.mu.Unlock()

	pr, ok := doc.Nodes[peer]
	if !ok {
		return nil
	}

	// Self-reported IPs from the peer's CIDR list.
	selfIPs := map[string]netip.Addr{}
	var selfOrder []netip.Addr
	for _, cidr := range pr.Addrs {
		pfx, err := netip.ParsePrefix(cidr)
		if err != nil {
			continue
		}
		a := pfx.Addr()
		s := a.String()
		if _, dup := selfIPs[s]; !dup {
			selfIPs[s] = a
			selfOrder = append(selfOrder, a)
		}
	}

	// Observations of this peer across ALL nodes: ip -> most recent lastSeen.
	type obs struct {
		addr     netip.Addr
		lastSeen int64
	}
	bestObs := map[string]obs{}
	for _, nr := range doc.Nodes {
		oe, ok := nr.Observed[peer]
		if !ok {
			continue
		}
		a, err := netip.ParseAddr(oe.IP)
		if err != nil {
			continue
		}
		s := a.String()
		if cur, exists := bestObs[s]; !exists || oe.LastSeenUnix > cur.lastSeen {
			bestObs[s] = obs{addr: a, lastSeen: oe.LastSeenUnix}
		}
	}

	// Intersection: observed ∩ self-reported, most-recent first.
	var inter []obs
	for s, o := range bestObs {
		if _, ok := selfIPs[s]; ok {
			inter = append(inter, o)
		}
	}
	if len(inter) > 0 {
		sort.Slice(inter, func(i, j int) bool { return inter[i].lastSeen > inter[j].lastSeen })
		out := make([]netip.Addr, 0, len(inter))
		for _, o := range inter {
			out = append(out, o.addr)
		}
		return out
	}

	// Cold-peer fallback: self-reported IPs in declared order (D6).
	return selfOrder
}
