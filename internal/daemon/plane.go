package daemon

// plane.go is the realtime MULTI-NODE control substrate the role loop drives:
// one clusterPlane per active session bundling memberlist gossip membership
// (cluster.Membership bridged to the shared persistent state.Store), the
// per-group master elections (cluster.GroupElections, A.5 / 02 §5), the
// source-IP allowlist guarding the unauthenticated realtime planes (07 §3), the
// previously-seen peer cache (peers.json, A.14.1) and the mDNS seed browse
// (02 §2). It replaces the P0.3 electedMaster/generationOf stubs: the loop calls
// electNow on every membership/store/tick signal and feeds the outcome to the
// generation-fenced roleState + the group engine.
//
// Everything here is BEST-EFFORT bring-up: a node with no gossip port (or a
// bind failure) degrades to the solo doc-elected substrate rather than failing
// activate (doc 01 §4.4 — a failed activate must stay clean, and a lone node
// must still play).

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/allowlist"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/cluster"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/discovery"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// Canonical realtime-plane ports (A.12), used when the operator override is 0.
const (
	defaultClockPort = 9000
	defaultAudioPort = 9100
)

// clusterPlane bundles the per-session multi-node substrate. Built by activate()
// (newClusterPlane + start), read by the role loop and the transport hooks,
// closed by deactivate(). mem and the mDNS pieces are nil when the node runs
// solo (no gossip port / bind failure) — every method is nil-safe for that.
type clusterPlane struct {
	n       *Node
	groupID string

	allow     *allowlist.Set          // gates clock server + audio receiver (07 §3)
	elections *cluster.GroupElections // per-group master election (02 §5)
	peers     *cluster.PeerStore      // peers.json seed cache; nil without a data dir
	mem       *cluster.Membership     // memberlist gossip; nil => solo substrate

	// Resolved realtime ports for THIS session. Several instances share a host
	// in dev/test, so the canonical ports collide; like the web listener, each
	// plane takes the next free port and ADVERTISES the actual one (gossip Meta
	// + mDNS TXT carry these, so peers always dial what was really bound).
	clockPort  int
	audioPort  int
	gossipPort int // actual memberlist bind port (set by start; 0 => solo)
}

// newClusterPlane constructs the plane's pure parts (allowlist, elections, peer
// cache) and resolves this session's actual clock/audio UDP ports (first free
// port at-or-above the configured base, mirroring listenWeb's +1-on-conflict).
// Gossip + mDNS bring-up happens in start.
func newClusterPlane(n *Node, groupID string) *clusterPlane {
	cp := &clusterPlane{
		n:         n,
		groupID:   groupID,
		allow:     allowlist.New(),
		elections: cluster.NewGroupElections(n.options.NodeID),
		clockPort: resolveFreeUDPPort(resolvedPort(n.options.ClockPort, defaultClockPort)),
		audioPort: resolveFreeUDPPort(resolvedPort(n.options.AudioPort, defaultAudioPort)),
	}
	if n.options.Paths.Peers != "" {
		cp.peers = cluster.LoadPeerStore(n.options.Paths.Peers)
	}
	return cp
}

// resolveFreeUDPPort probes base, base+1, … and returns the first UDP port that
// binds (probe-and-release; the tiny race until the real bind is acceptable on
// a LAN box). Falls back to base when nothing in the window binds.
func resolveFreeUDPPort(base int) int {
	for p := base; p < base+64; p++ {
		pc, err := net.ListenPacket("udp", ":"+strconv.Itoa(p))
		if err != nil {
			continue
		}
		_ = pc.Close()
		return p
	}
	return base
}

// start brings the gossip membership up (best-effort): memberlist on BindPort,
// encrypted with the cluster gossip key, seeded from peers.json ∪ Options.Seeds,
// its delegate bridging the shared persistent store into push/pull anti-entropy
// (A.14.2). With UseMDNS it also browses for same-cluster seeds in the
// background and Joins them. A node with BindPort<=0 (or a bind failure) keeps
// mem nil and runs the solo doc-elected substrate.
func (cp *clusterPlane) start(ctx context.Context) {
	doc := cp.n.store.Get()
	// Seed the allowlist from the durable doc before any member is seen, so the
	// clock server / receiver gates are correct from the first packet (07 §3.1).
	cp.allow.Update(doc, nil)

	opts := cp.n.options
	if opts.BindPort <= 0 {
		return // no gossip plane configured: solo substrate
	}
	seeds := append([]string{}, opts.Seeds...)
	if cp.peers != nil {
		seeds = append(cp.peers.JoinSeeds(), seeds...)
	}
	// Bind the gossip port with the same +1-on-conflict retry as the web
	// listener: a second instance on the host must not silently run solo (and
	// therefore show "offline" forever). The ACTUAL port is what peers learn —
	// memberlist advertises it live and mDNS carries it in the SRV record.
	var mem *cluster.Membership
	var err error
	for port := opts.BindPort; port < opts.BindPort+64; port++ {
		mem, err = cluster.New(cluster.Config{
			NodeID:      opts.NodeID,
			Name:        opts.Name,
			GroupID:     cp.groupID,
			ClusterFP:   doc.Cluster.Fingerprint,
			BindPort:    port,
			ControlPort: cp.n.webPort,
			ClockPort:   cp.clockPort,
			AudioPort:   cp.audioPort,
			WebPort:     cp.n.webPort,
			SecretKey:   decodeGossipKey(doc.Secrets.SharedSecret),
			Seeds:       seeds,
			State:       cp.n.store,
		})
		if err == nil {
			cp.gossipPort = port
			break
		}
		if !strings.Contains(err.Error(), "address already in use") {
			break
		}
	}
	if err != nil {
		logf(opts.Log, "cluster membership unavailable (running solo): %v", err)
		return
	}
	cp.mem = mem
	if cp.gossipPort != opts.BindPort {
		logf(opts.Log, "gossip port %d busy — bound :%d instead", opts.BindPort, cp.gossipPort)
	}
	logf(opts.Log, "gossip membership up on :%d (%d seed(s))", cp.gossipPort, len(seeds))

	if opts.UseMDNS {
		go cp.mdnsSeedLoop(ctx, doc.Cluster.Fingerprint)
	}
}

// mdnsSeedLoop periodically browses mDNS for same-cluster peers and feeds their
// gossip addresses to memberlist.Join (discovery only BOOTSTRAPS membership,
// 02 §2.1). Best-effort self-healing: it runs until the session ctx ends.
func (cp *clusterPlane) mdnsSeedLoop(ctx context.Context, clusterFP string) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		bctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		seeds, err := discovery.Browse(bctx, clusterFP, cp.n.options.NodeID)
		cancel()
		if err == nil && len(seeds) > 0 && cp.mem != nil {
			_, _ = cp.mem.Join(seeds)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// recompute is the per-signal election + allowlist refresh (02 §5.2, 07 §3.2):
// it re-runs every group's election over the live members and atomically swaps
// the allowlist snapshot to doc.Nodes[].Addrs ∪ live member addrs. It returns
// the alive members for the caller's bookkeeping (status, peer persist).
func (cp *clusterPlane) recompute(doc state.ConfigDoc) []cluster.Member {
	var alive []cluster.Member
	if cp.mem != nil {
		alive = cp.mem.Members()
	}
	cp.elections.Recompute(doc, alive)

	live := make([]allowlist.MemberAddr, 0, len(alive))
	for _, m := range alive {
		if a, ok := netip.AddrFromSlice(m.Addr); ok {
			live = append(live, allowlist.MemberAddr{Addr: a})
		}
	}
	cp.allow.Update(doc, live)
	return alive
}

// electNow resolves the current (master, generation) for the plane's group: the
// live election outcome when gossip runs, else the doc-derived solo fallback
// (masterOf — lowest member id), else self for the unconfigured skeleton (the
// P0.3 behavior TestLifecycle pins: a configured single node reports master).
func (n *Node) electNow(cp *clusterPlane) (master string, gen uint64) {
	doc := n.store.Get()
	groupID := "default"
	if cp != nil {
		groupID = cp.groupID
		alive := cp.recompute(doc)
		n.mu.Lock()
		n.members = len(alive)
		n.mu.Unlock()
		if cp.mem != nil {
			if m := cp.elections.Master(groupID); m != "" {
				return m, cp.elections.Generation(groupID)
			}
		}
	}
	// Solo / not-yet-converged fallback: doc-derived election (A.5 tiebreak).
	if m := masterOf(doc, groupID, n.options.NodeID); m != "" {
		return m, 0
	}
	if groupRecord(doc, groupID) == nil {
		return n.options.NodeID, 0 // unconfigured skeleton: solo self
	}
	return "", 0
}

// isAlive reports whether nodeID is currently a live gossip member.
func (cp *clusterPlane) isAlive(nodeID string) bool {
	if cp.mem == nil {
		return false
	}
	for _, m := range cp.mem.Members() {
		if m.Meta.NodeID == nodeID {
			return true
		}
	}
	return false
}

// claimMasterHint anchors mastership in the AGREED doc (the A.5 soft hint):
// when this node is the elected master but the hint does not name it (empty
// legacy doc, or a dead predecessor after failover), it writes itself as the
// group's MasterHint. Because the hint rides the replicated ConfigDoc, every
// node applies it identically — incumbency becomes consistent cluster state,
// not divergent local memory (the split-brain that killed naive stickiness).
// Mastership thereafter changes ONLY when an operator starts playback from a
// different source node (master-follows-source rewrites the hint) or this
// master disappears; a lower-id node joining mid-stream can no longer steal
// the role (which re-keyed the stream and permanently dropped sync). An ALIVE
// hinted node is never contested — transient view skew resolves toward it on
// the next recompute. Best-effort: a version conflict retries next tick.
func (n *Node) claimMasterHint(cp *clusterPlane, master string) {
	if cp == nil || master == "" || master != n.options.NodeID {
		return
	}
	doc := n.store.Get()
	i := indexGroup(doc.Groups, cp.groupID)
	if i < 0 || doc.Groups[i].MasterHint == master {
		return
	}
	if h := doc.Groups[i].MasterHint; h != "" && cp.isAlive(h) {
		return // never contest a live hinted node
	}
	doc.Groups[i].MasterHint = master
	if _, err := n.store.Apply(doc); err == nil {
		logf(n.options.Log, "election: claimed master hint for group %q (anchor against id-steal)", cp.groupID)
	}
}

// kickSync triggers an immediate anti-entropy push/pull with every live peer by
// re-Joining their gossip addresses (memberlist exchanges full delegate state on
// Join). The periodic push/pull interval is ~30s (A.14.2) — fine for an
// infrequently-edited doc at steady state, but a config write (play/stop/adopt)
// should converge promptly. Joins are idempotent and the doc is tiny, so this
// is cheap at ≤8 nodes. No-op when solo.
func (cp *clusterPlane) kickSync() {
	if cp.mem == nil {
		return
	}
	self := cp.n.options.NodeID
	var addrs []string
	for _, m := range cp.mem.Members() {
		if m.Meta.NodeID == self {
			continue
		}
		addrs = append(addrs, m.GossipAddr())
	}
	if len(addrs) > 0 {
		_, _ = cp.mem.Join(addrs)
	}
}

// persistPeers upserts the current live members into peers.json (A.14.1) so a
// restart rejoins fast and known-but-absent peers show as offline. Called on
// membership-changed signals only (not every tick) to avoid disk churn.
func (cp *clusterPlane) persistPeers() {
	if cp.peers == nil || cp.mem == nil {
		return
	}
	if members := cp.mem.Members(); len(members) > 0 {
		cp.peers.Upsert(members)
	}
}

// changed returns the membership-changed signal channel (nil when solo, which
// blocks forever in a select — exactly what we want).
func (cp *clusterPlane) changed() <-chan struct{} {
	if cp.mem == nil {
		return nil
	}
	return cp.mem.Changed()
}

// clockAddrOf resolves the elected master's clock-plane address (the engine's
// WithMasterAddr seam): self → the local clock listen addr; a live member → its
// gossiped Meta clock endpoint; else the doc NodeRecord addr + our canonical
// clock port (best-effort last resort).
func (cp *clusterPlane) clockAddrOf(_, masterID string) string {
	local := ":" + strconv.Itoa(cp.clockPort)
	if masterID == "" || masterID == cp.n.options.NodeID {
		return local
	}
	if cp.mem != nil {
		for _, m := range cp.mem.Members() {
			if m.Meta.NodeID == masterID {
				return m.ClockAddr()
			}
		}
	}
	doc := cp.n.store.Get()
	if nr := nodeRecord(doc, masterID); nr != nil && len(nr.Addrs) > 0 {
		return net.JoinHostPort(nr.Addrs[0], strconv.Itoa(cp.clockPort))
	}
	return local
}

// audioAddrOf resolves a member's audio-plane address for the origin's listener
// fan-out: live member Meta first (correct per-node port), doc NodeRecord addr +
// our audio port as the fallback. ok=false when the node is unknown entirely.
func (cp *clusterPlane) audioAddrOf(nodeID string) (string, bool) {
	if cp.mem != nil {
		for _, m := range cp.mem.Members() {
			if m.Meta.NodeID == nodeID {
				return m.AudioAddr(), true
			}
		}
	}
	doc := cp.n.store.Get()
	if nr := nodeRecord(doc, nodeID); nr != nil && len(nr.Addrs) > 0 {
		return net.JoinHostPort(nr.Addrs[0], strconv.Itoa(cp.audioPort)), true
	}
	return "", false
}

// close leaves the gossip cluster (graceful departure broadcast + shutdown).
// It deliberately does NOT nil cp.mem: the role loop may still be mid-iteration
// (its ctx is cancelled, not joined) and memberlist serializes Leave/Shutdown
// against Members internally, so leaving the pointer in place is the race-free
// teardown. The mDNS seed loop stops with the session ctx; the allowlist/
// elections are inert without callers. Safe on a solo plane.
func (cp *clusterPlane) close() {
	if cp.mem != nil {
		_ = cp.mem.Leave()
	}
}

// resolvedPort returns port, or def when port is unset (<=0).
func resolvedPort(port, def int) int {
	if port <= 0 {
		return def
	}
	return port
}
