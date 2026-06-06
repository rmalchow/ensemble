package cluster

import (
	"sync"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// delegate supplies this node's Meta and bridges the shared ConfigDoc into
// memberlist's push/pull anti-entropy (A.14.2). LocalState ships our doc (plus
// our id, so the receiver can break a version tie); MergeRemoteState folds a
// peer's doc in via the state store's last-writer-wins + grow-only RevokedSet
// union (that LWW logic lives in internal/state, P2.1, not here — the delegate
// is a pure pass-through). NotifyMsg/GetBroadcasts (best-effort user messages)
// stay unused: the periodic push/pull at the LAN PushPullInterval (~30s) is
// sufficient for an infrequently-edited doc (02 §4.1).
type delegate struct {
	metaMu sync.Mutex
	meta   []byte
	state  *state.Store
}

func (d *delegate) setMeta(b []byte) {
	d.metaMu.Lock()
	d.meta = b
	d.metaMu.Unlock()
}

// NodeMeta returns this node's gossiped Meta, truncated to memberlist's limit.
func (d *delegate) NodeMeta(limit int) []byte {
	d.metaMu.Lock()
	meta := d.meta
	d.metaMu.Unlock()
	if len(meta) > limit {
		return meta[:limit]
	}
	return meta
}

func (d *delegate) NotifyMsg([]byte)                {}
func (d *delegate) GetBroadcasts(int, int) [][]byte { return nil }

// LocalState returns our ConfigDoc gossip envelope for anti-entropy push/pull.
func (d *delegate) LocalState(bool) []byte {
	if d.state == nil {
		return nil
	}
	return d.state.MarshalGossip()
}

// MergeRemoteState merges a peer's ConfigDoc gossip envelope into our store.
func (d *delegate) MergeRemoteState(buf []byte, _ bool) {
	if d.state == nil {
		return
	}
	d.state.MergeGossip(buf)
}
