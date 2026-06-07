package group

import (
	"testing"
	"time"

	"ensemble/internal/contracts"
)

func TestHeartbeatRefreshesPositionAndSource(t *testing.T) {
	self := idN(1)
	r := newRig(self, 1000, true)
	r.cl.setSnap(soloSnap(self))
	r.e.p.Heartbeat = 5 * time.Second
	r.srv.stats = contracts.SourceStats{Clients: 2, Connects: 3, Restarts: 1, Primes: 1}

	if err := r.e.Play("input:"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	defer r.e.Close()

	// Advance the fake clock past the heartbeat interval, then reconcile.
	r.advance(6 * time.Second)
	r.e.reconcile()

	pc, ok := r.cl.lastPlayback()
	if !ok || pc.pb.State != "playing" {
		t.Fatalf("playback = %+v", pc.pb)
	}
	if pc.pb.PositionSec < 6 {
		t.Fatalf("positionSec = %v, want >= 6", pc.pb.PositionSec)
	}
	if pc.pb.Source.Connects != 3 || pc.pb.Source.Clients != 2 {
		t.Fatalf("Source stats not refreshed: %+v", pc.pb.Source)
	}
}

func TestHeartbeatStopsWhenIdle(t *testing.T) {
	self := idN(1)
	r := newRig(self, 0, false)
	r.cl.setSnap(soloSnap(self))
	// No session running.
	r.advance(30 * time.Second)
	r.e.reconcile()
	if _, ok := r.cl.lastPlayback(); ok {
		t.Fatal("idle heartbeat wrote playback")
	}
}
