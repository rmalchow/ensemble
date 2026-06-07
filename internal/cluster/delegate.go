package cluster

import (
	"encoding/json"
	"net/netip"

	"github.com/hashicorp/memberlist"

	"ensemble/internal/id"
)

// delegate implements memberlist.Delegate and memberlist.EventDelegate. All
// methods run on memberlist goroutines and are treated as external callers of
// c.mu (and c.live's mutex).
type delegate struct{ c *Cluster }

// --- memberlist.Delegate ---

// NodeMeta carries our 16-byte node id so events can recover the peer id without
// parsing the name. (Name is also set to id.String() in mlCfg.)
func (d *delegate) NodeMeta(limit int) []byte {
	self := d.c.self
	if limit < len(self) {
		return nil
	}
	return append([]byte(nil), self[:]...)
}

// NotifyMsg applies a single-record delta broadcast.
func (d *delegate) NotifyMsg(msg []byte) {
	if len(msg) == 0 {
		return
	}
	kind, dl, err := decodeDelta(msg)
	if err != nil {
		d.c.log.Debug("bad gossip delta", "err", err)
		return
	}
	c := d.c
	changed := false
	c.mu.Lock()
	switch kind {
	case kindNodeDelta:
		changed = c.doc.mergeNode(c.self, dl.Node)
	case kindGroupName:
		changed = c.doc.mergeGroupName(dl.Group, dl.Name)
	case kindPlayback:
		changed = c.doc.mergePlayback(dl.Group, dl.Playback)
	case kindSettings:
		changed = c.doc.mergeSettings(dl.Group, dl.Settings)
	}
	c.mu.Unlock()
	if changed {
		c.notify()
	}
}

// GetBroadcasts pulls queued single-record deltas for gossip.
func (d *delegate) GetBroadcasts(overhead, limit int) [][]byte {
	return d.c.queue.GetBroadcasts(overhead, limit)
}

// LocalState returns the whole document for a TCP push/pull.
func (d *delegate) LocalState(join bool) []byte {
	d.c.mu.Lock()
	doc := d.c.doc.clone()
	d.c.mu.Unlock()
	b, _ := json.Marshal(doc)
	return b
}

// MergeRemoteState merges a remote document (push/pull anti-entropy). Also
// performs the D7 own-version reconciliation.
func (d *delegate) MergeRemoteState(buf []byte, join bool) {
	var remote Document
	if err := json.Unmarshal(buf, &remote); err != nil {
		d.c.log.Debug("bad remote state", "err", err)
		return
	}
	c := d.c

	// D7: if a peer holds a copy of OUR record at version >= ours, reconcile.
	if rr, ok := remote.Nodes[c.self]; ok {
		c.reconcileOwnVersion(rr.Version)
	}

	c.mu.Lock()
	changed := c.doc.mergeAll(c.self, &remote)
	c.mu.Unlock()
	if changed {
		c.notify()
	}
}

// --- memberlist.EventDelegate ---

func (d *delegate) NotifyJoin(n *memberlist.Node) {
	peer, ok := peerID(n)
	if !ok {
		return
	}
	now := d.c.clock().Unix()
	d.c.live.join(peer, now)
	d.observeNode(peer, n)
	d.c.notify()
}

func (d *delegate) NotifyLeave(n *memberlist.Node) {
	peer, ok := peerID(n)
	if !ok {
		return
	}
	d.c.live.leave(peer)
	d.c.notify()
}

func (d *delegate) NotifyUpdate(n *memberlist.Node) {
	peer, ok := peerID(n)
	if !ok {
		return
	}
	now := d.c.clock().Unix()
	d.c.live.update(peer, now)
	d.observeNode(peer, n)
	d.c.notify()
}

// observeNode records the peer's memberlist remote IP as a §3.1 observation.
func (d *delegate) observeNode(peer id.ID, n *memberlist.Node) {
	if a, ok := addrFromNode(n); ok {
		d.c.Observe(peer, a)
	}
}

// addrFromNode converts a memberlist node's net.IP to a netip.Addr (unmapped).
func addrFromNode(n *memberlist.Node) (netip.Addr, bool) {
	a, ok := netip.AddrFromSlice(n.Addr)
	if !ok {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// peerID extracts the ensemble id from a memberlist node: Name (id.String()) is
// authoritative; Meta (16 raw bytes) is the robust backup.
func peerID(n *memberlist.Node) (id.ID, bool) {
	if pid, err := id.Parse(n.Name); err == nil {
		return pid, true
	}
	if len(n.Meta) >= len(id.Zero) {
		var pid id.ID
		copy(pid[:], n.Meta[:len(pid)])
		return pid, true
	}
	return id.Zero, false
}
