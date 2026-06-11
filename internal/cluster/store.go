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

	// D60: a non-gossiping playback node is not in memberlist; its liveness is mDNS
	// freshness — its proxied record's UpdatedAt is refreshed on every mDNS hit
	// (~30 s re-emit). Fresh within the TTL ⇒ alive. (STATUS recency folds in here
	// too once the control driver lands.)
	for nid, r := range doc.Nodes {
		if r.PlaybackNode && nowUnix-r.UpdatedAt < playbackLivenessTTLSec {
			alive[nid] = true
			if seen[nid] == 0 {
				seen[nid] = r.UpdatedAt
			}
		}
	}

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
		ID:               r.ID,
		Name:             r.Name,
		Volume:           r.Volume,
		OutputDelayMs:    r.OutputDelayMs,
		OutputDevice:     r.OutputDevice,
		OutputDevices:    append([]contracts.OutputDevice(nil), r.OutputDevices...),
		OutputBackend:    r.OutputBackend,
		InputDevices:     append([]contracts.InputDevice(nil), r.InputDevices...),
		Addrs:            append([]string(nil), r.Addrs...),
		HTTPPort:         r.HTTPPort,
		StreamPort:       r.StreamPort,
		SourcePort:       r.SourcePort,
		GossipPort:       r.GossipPort,
		Capabilities:     effectiveCaps(r.Caps, r.Disabled),
		Disabled:         append([]string(nil), r.Disabled...),
		PlaybackNode:     r.PlaybackNode,
		ControlPort:      r.ControlPort,
		Following:        r.Following,
		SpotifyEndpoints: cloneEndpoints(r.SpotifyEndpoints),
		Observed:         obs,
		Alive:            alive[nid],
		LastSeenUnix:     lastSeen,
		Stale:            lastSeen != 0 && nowUnix-lastSeen > int64(staleAfter.Seconds()),
		UpdatedAt:        r.UpdatedAt,
		Version:          r.Version,
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

// playbackLivenessTTLSec is how long a proxied playback node stays "alive" after
// its last mDNS refresh (D60). Discovery re-emits an unchanged peer every ~30 s, so
// 90 s tolerates two missed refreshes before the node is treated as gone.
const playbackLivenessTTLSec = 90

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

	// A master is any alive GOSSIPING node — master:group is strictly 1:1, group id
	// == node id (D44). Playback nodes (and dead/unknown nodes) never master. Every
	// node masters its OWN group, always; mastership is intrinsic, not derived.
	isMaster := func(n id.ID) bool {
		r, ok := nodes[n]
		return ok && isAlive(n) && !r.PlaybackNode
	}

	// `following` is the PLAYER's target group (D49+): a node's player joins
	// group(Following) iff Following points at a LIVE master. Following == Zero, or a
	// dead / unknown / playback-node target ⇒ the player is IDLE (in no group, no
	// fallback). Players are grouped by their target master.
	players := map[id.ID][]id.ID{}
	for nid, r := range nodes {
		if !isAlive(nid) {
			continue
		}
		m := r.Following
		if m.IsZero() || !isMaster(m) {
			continue // idle player — attaches to no group
		}
		players[m] = append(players[m], nid)
	}

	// One group per alive master node (even with zero players — an idle group is a
	// valid, assignable zone). Members are the master's PLAYERS; the master is a
	// member of its OWN group only when it follows itself (plays its own stream).
	groups := make([]contracts.GroupView, 0, len(nodes))
	for nid, r := range nodes {
		if !isAlive(nid) || r.PlaybackNode {
			continue
		}
		mem := players[nid]
		if mem == nil {
			mem = []id.ID{} // an empty group serializes as [], not null (stable API contract)
		}
		sort.Slice(mem, func(i, j int) bool { return idLess(mem[i], mem[j]) })
		gv := contracts.GroupView{
			ID:       nid, // group id == master id (D44)
			Master:   nid,
			Members:  mem,
			Settings: resolveSettings(settings[nid]),
			Playback: resolvePlayback(playback[nid]),
		}
		// Name: explicit override (keyed by the master/group id — stable across
		// player churn), else derived from the player NAMES. An empty group (no
		// players) is labelled by the master's own room name.
		switch {
		case names[nid] != nil && names[nid].Name != "":
			gv.Name = names[nid].Name
			gv.NameIsDerived = false
		case len(mem) == 0:
			gv.Name = nodeLabel(nodes, nid)
			gv.NameIsDerived = true
		default:
			gv.Name = derivedLabel(mem, nodes)
			gv.NameIsDerived = true
		}
		groups = append(groups, gv)
	}
	sort.Slice(groups, func(i, j int) bool { return idLess(groups[i].ID, groups[j].ID) })
	return groups
}

// nodeLabel is a node's display name, or its 8-char short id when unnamed.
func nodeLabel(nodes map[id.ID]*NodeRecord, n id.ID) string {
	if r, ok := nodes[n]; ok && r.Name != "" {
		return r.Name
	}
	return n.String()[:8]
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
		Metadata:    r.Metadata,
		QueueLen:    r.QueueLen,
		QueueRev:    r.QueueRev,
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
