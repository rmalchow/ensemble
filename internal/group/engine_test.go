package group

import (
	"context"
	"testing"
	"time"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

func TestRunReconcilesOnClusterChange(t *testing.T) {
	self, master := idN(1), idN(2)
	r := newRig(self, 0, false)
	r.cl.setSnap(soloSnap(self))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.e.Run(ctx)

	// Initial reconcile points at self (loopback). Wait for it.
	waitFor(t, time.Second, func() bool { return len(r.sub.snapshotSubs()) >= 1 }, "initial repoint")

	// Change to following a master and signal.
	r.cl.dialResults[master] = nil
	r.cl.setSnap(masterSnap(master, defaultSettings(), self))
	r.cl.signal()

	waitFor(t, time.Second, func() bool {
		subs := r.sub.snapshotSubs()
		if len(subs) == 0 {
			return false
		}
		return r.e.Group().Master == master
	}, "reconcile on cluster change")
}

func TestRunHealsOnBoot(t *testing.T) {
	self := idN(1)
	r := newRig(self, 0, false)
	r.e.p.Grace = 30 * time.Millisecond
	// Boot following a dead/unknown node → stale.
	n := node(self, idN(9), true)
	g := contracts.GroupView{ID: id.XOR(self), Master: self, Members: []id.ID{self}}
	r.cl.setSnap(contracts.Snapshot{Nodes: []contracts.NodeView{n}, Groups: []contracts.GroupView{g}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Use real time for boot heal so the 1 s ticker (and Grace) advance naturally;
	// override the now seam to real time by advancing it in a loop.
	go func() {
		for i := 0; i < 100; i++ {
			r.advance(20 * time.Millisecond)
			time.Sleep(2 * time.Millisecond)
		}
	}()
	go r.e.Run(ctx)

	waitFor(t, 2*time.Second, func() bool {
		got, ok := r.cl.lastFollowing()
		return ok && got == id.Zero
	}, "boot self-heal")
}

func TestCloseHaltsSessionAndUnsubscribes(t *testing.T) {
	self := idN(1)
	r := newRig(self, 1000, true)
	r.cl.setSnap(soloSnap(self))
	if err := r.e.Play("input:"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitFor(t, time.Second, func() bool { return len(r.srv.snapshotReleases()) >= 1 }, "first release")

	if err := r.e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if r.srv.stopCount() < 1 {
		t.Fatal("StopSession not called on Close")
	}
	r.sub.mu.Lock()
	unsubs := r.sub.unsubs
	r.sub.mu.Unlock()
	if unsubs < 1 {
		t.Fatal("Unsubscribe not called on Close")
	}
}

func TestCloseIdempotent(t *testing.T) {
	self := idN(1)
	r := newRig(self, 0, false)
	r.cl.setSnap(soloSnap(self))
	if err := r.e.Close(); err != nil {
		t.Fatalf("Close 1: %v", err)
	}
	if err := r.e.Close(); err != nil {
		t.Fatalf("Close 2: %v", err)
	}
}

func TestClosedRejectsOps(t *testing.T) {
	self := idN(1)
	r := newRig(self, 0, false)
	r.cl.setSnap(soloSnap(self))
	_ = r.e.Close()
	if err := r.e.Play("x"); err != ErrClosed {
		t.Fatalf("Play after close = %v, want ErrClosed", err)
	}
	if err := r.e.Follow(idN(2)); err != ErrClosed {
		t.Fatalf("Follow after close = %v, want ErrClosed", err)
	}
}
