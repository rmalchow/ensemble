package group

import (
	"net/netip"
	"testing"
	"time"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

func TestRepointSubscriberOnMasterChange(t *testing.T) {
	self, a, b := idN(1), idN(2), idN(3)
	r := newRig(self, 0, false)
	r.cl.dialResults[a] = []netip.Addr{netip.AddrFrom4([4]byte{127, 0, 0, 2})}
	r.cl.dialResults[b] = []netip.Addr{netip.AddrFrom4([4]byte{127, 0, 0, 3})}

	// self follows a.
	r.cl.setSnap(masterSnap(a, defaultSettings(), self))
	r.e.reconcile()

	subs := r.sub.snapshotSubs()
	if len(subs) != 1 {
		t.Fatalf("subs after first reconcile = %d, want 1", len(subs))
	}
	if subs[0].addr.Addr() != netip.AddrFrom4([4]byte{127, 0, 0, 2}) {
		t.Fatalf("sub addr = %v", subs[0].addr)
	}

	// Idempotent: unchanged master → no new Subscribe.
	r.e.reconcile()
	if got := len(r.sub.snapshotSubs()); got != 1 {
		t.Fatalf("subs after repeat reconcile = %d, want 1 (idempotent)", got)
	}

	// Master changes to b.
	r.cl.setSnap(masterSnap(b, defaultSettings(), self))
	r.e.reconcile()
	subs = r.sub.snapshotSubs()
	if len(subs) != 2 {
		t.Fatalf("subs after master change = %d, want 2", len(subs))
	}
	if subs[1].addr.Addr() != netip.AddrFrom4([4]byte{127, 0, 0, 3}) {
		t.Fatalf("new sub addr = %v", subs[1].addr)
	}
	// Clock follower + sink re-pointed too.
	if len(r.cc.snapshot()) != 2 {
		t.Fatalf("clockctl calls = %d, want 2", len(r.cc.snapshot()))
	}
	if len(r.snk.snapshotResets()) != 2 {
		t.Fatalf("sink resets = %d, want 2", len(r.snk.snapshotResets()))
	}
}

func TestRepointUsesDialCandidatesAndPorts(t *testing.T) {
	self, master := idN(1), idN(2)
	r := newRig(self, 0, false)
	masterIP := netip.AddrFrom4([4]byte{10, 0, 0, 5})
	r.cl.dialResults[master] = []netip.Addr{masterIP}

	snap := masterSnap(master, defaultSettings(), self)
	// give the master distinct ports.
	for i := range snap.Nodes {
		if snap.Nodes[i].ID == master {
			snap.Nodes[i].SourcePort = 9300
			snap.Nodes[i].StreamPort = 9190
		}
	}
	r.cl.setSnap(snap)
	r.e.reconcile()

	subs := r.sub.snapshotSubs()
	if len(subs) != 1 {
		t.Fatalf("subs = %d", len(subs))
	}
	if subs[0].addr != netip.AddrPortFrom(masterIP, 9300) {
		t.Fatalf("sub addr = %v, want %v:9300", subs[0].addr, masterIP)
	}
	cc := r.cc.snapshot()
	if cc[0].dst != netip.AddrPortFrom(masterIP, 9190) {
		t.Fatalf("clock addr = %v, want %v:9190", cc[0].dst, masterIP)
	}
}

func TestMasterSubscribesToSelfLoopback(t *testing.T) {
	self := idN(1)
	r := newRig(self, 0, false)
	// No dial candidates for self → loopback fallback.
	r.cl.setSnap(soloSnap(self))
	r.e.reconcile()

	subs := r.sub.snapshotSubs()
	if len(subs) != 1 {
		t.Fatalf("subs = %d, want 1", len(subs))
	}
	if !subs[0].addr.Addr().IsLoopback() {
		t.Fatalf("self sub addr = %v, want loopback", subs[0].addr)
	}
}

func TestWatchTearsDownSessionOnMasterLoss(t *testing.T) {
	self, newMaster := idN(1), idN(2)
	r := newRig(self, 1000, true)
	r.cl.setSnap(soloSnap(self))
	if err := r.e.Play("input:"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	defer r.e.Close()
	waitFor(t, time.Second, func() bool { return len(r.srv.snapshotReleases()) >= 1 }, "first release")
	plBefore := len(r.cl.playback)

	// self loses mastership: now a follower of newMaster.
	r.cl.dialResults[newMaster] = []netip.Addr{netip.AddrFrom4([4]byte{127, 0, 0, 9})}
	r.cl.setSnap(masterSnap(newMaster, defaultSettings(), self))
	r.e.reconcile()

	// session torn down (StopSession), no status rewrite by the teardown itself.
	if r.srv.stopCount() < 1 {
		t.Fatal("StopSession not called on master loss")
	}
	r.e.mu.Lock()
	sess := r.e.sess
	r.e.mu.Unlock()
	if sess != nil {
		t.Fatal("session not cleared after master loss")
	}
	// teardown must NOT have written a fresh idle playback (we are no longer the
	// writer); the only new playback writes would be from the heartbeat, which is
	// gated on isMaster.
	if len(r.cl.playback) != plBefore {
		t.Fatalf("teardown rewrote playback: %d -> %d", plBefore, len(r.cl.playback))
	}
}

func TestReconcileSkipsBeforeSelfDerived(t *testing.T) {
	self := idN(1)
	r := newRig(self, 0, false)
	r.cl.setSnap(contracts.Snapshot{}) // self not present
	r.e.reconcile()                    // must not panic
	if len(r.sub.snapshotSubs()) != 0 {
		t.Fatal("no re-point expected before self derived")
	}
}

func TestReconcileHealViaRun(t *testing.T) {
	self := idN(1)
	r := newRig(self, 0, false)
	r.e.p.Grace = 50 * time.Millisecond
	// Dangling follow snapshot (stale).
	n := node(self, idN(9), true)
	g := contracts.GroupView{ID: id.XOR(self), Master: self, Members: []id.ID{self}}
	r.cl.setSnap(contracts.Snapshot{Nodes: []contracts.NodeView{n}, Groups: []contracts.GroupView{g}})

	r.e.reconcile()                   // arms healAt (fake now)
	r.advance(100 * time.Millisecond) // past Grace
	r.e.reconcile()
	got, ok := r.cl.lastFollowing()
	if !ok || got != id.Zero {
		t.Fatalf("self-heal SetFollowing = %v,%v want Zero", got, ok)
	}
}
