package group

import (
	"testing"
	"time"

	"ensemble/internal/stream"
)

func TestSessionReleasesInOrder(t *testing.T) {
	self := idN(1)
	r := newRig(self, 5, false)
	r.cl.setSnap(soloSnap(self))
	r.clk.offset = 1000 // LocalToMaster adds offset

	if err := r.e.Play("song.wav"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	defer r.e.Close()

	waitFor(t, 2*time.Second, func() bool {
		return len(r.srv.snapshotReleases()) >= 5
	}, "5 releases")

	rel := r.srv.snapshotReleases()
	if len(rel) < 5 {
		t.Fatalf("releases = %d", len(rel))
	}
	// pts must step by FrameNanos and the first must be the session start.
	for i := 1; i < 5; i++ {
		if rel[i].pts-rel[i-1].pts != stream.FrameNanos {
			t.Fatalf("pts step at %d = %d, want %d", i, rel[i].pts-rel[i-1].pts, stream.FrameNanos)
		}
	}
}

func TestSessionStartFromLocalToMaster(t *testing.T) {
	self := idN(1)
	r := newRig(self, 3, false)
	r.cl.setSnap(soloSnap(self))
	r.clk.offset = 500_000

	if err := r.e.Play("song.wav"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	defer r.e.Close()
	waitFor(t, time.Second, func() bool { return len(r.srv.snapshotReleases()) >= 1 }, "first release")

	// first pts == startMaster == LocalToMaster(playInstant)+lead. We can't know
	// the exact local instant, but the first pts must be >= offset (lead>0, local
	// nanos>0) and the engine's sess.startMaster must equal the first pts.
	r.e.mu.Lock()
	sess := r.e.sess
	r.e.mu.Unlock()
	if sess == nil {
		t.Fatal("session gone")
	}
	first := r.srv.snapshotReleases()[0].pts
	startMaster := sess.startMaster.Load()
	if first != startMaster {
		t.Fatalf("first pts = %d, want startMaster %d", first, startMaster)
	}
	if startMaster <= r.clk.offset {
		t.Fatalf("startMaster %d should exceed offset %d (lead added)", startMaster, r.clk.offset)
	}
}

func TestSessionPullEOFDrainsThenEnds(t *testing.T) {
	self := idN(1)
	r := newRig(self, 2, false)
	r.cl.setSnap(soloSnap(self))
	if err := r.e.Play("song.wav"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	defer r.e.Close()

	// Wait for the source to be fully read (2 frames) → drain begins.
	waitFor(t, time.Second, func() bool { return r.med.src.reads() == 2 }, "both frames read")
	relAfterRead := len(r.srv.snapshotReleases())

	// Keep advancing the fake clock so the drain deadline (set from now at EOF)
	// is eventually passed regardless of when EOF lands.
	stopAdv := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopAdv:
				return
			default:
				r.advance(time.Second)
				time.Sleep(time.Millisecond)
			}
		}
	}()
	defer close(stopAdv)

	waitFor(t, 2*time.Second, func() bool {
		pc, ok := r.cl.lastPlayback()
		return ok && pc.pb.State == "idle"
	}, "idle status after drain")

	// No additional releases during the drain (no more reads/publishes).
	if got := len(r.srv.snapshotReleases()); got != relAfterRead {
		t.Fatalf("releases grew during drain: %d -> %d", relAfterRead, got)
	}
	if r.srv.stopCount() < 1 {
		t.Fatal("StopSession not called on EOF end")
	}
}

func TestSessionLiveNeverEOF(t *testing.T) {
	self := idN(1)
	r := newRig(self, 0, true) // live source: never EOF
	r.cl.setSnap(soloSnap(self))
	if err := r.e.Play("input:"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	defer r.e.Close()

	waitFor(t, time.Second, func() bool { return len(r.srv.snapshotReleases()) >= 3 }, "live releases")
	r.advance(10 * time.Second) // would trigger drain end IF it were pull

	// Give it a moment; it must NOT have written idle.
	time.Sleep(60 * time.Millisecond)
	if pc, ok := r.cl.lastPlayback(); ok && pc.pb.State == "idle" {
		t.Fatal("live session ended on its own")
	}
}

func TestSessionStopHaltsImmediately(t *testing.T) {
	self := idN(1)
	r := newRig(self, 100, true)
	r.cl.setSnap(soloSnap(self))
	if err := r.e.Play("input:"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitFor(t, time.Second, func() bool { return len(r.srv.snapshotReleases()) >= 1 }, "first release")

	if err := r.e.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	relAtStop := len(r.srv.snapshotReleases())

	time.Sleep(60 * time.Millisecond) // a few ticks would pass
	if got := len(r.srv.snapshotReleases()); got != relAtStop {
		t.Fatalf("releases grew after Stop: %d -> %d", relAtStop, got)
	}
	pc, ok := r.cl.lastPlayback()
	if !ok || pc.pb.State != "idle" {
		t.Fatalf("status after Stop = %+v, want idle", pc.pb)
	}
}

func TestStopIdempotent(t *testing.T) {
	self := idN(1)
	r := newRig(self, 100, true)
	r.cl.setSnap(soloSnap(self))
	if err := r.e.Play("input:"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if err := r.e.Stop(); err != nil {
		t.Fatalf("Stop 1: %v", err)
	}
	stops := r.srv.stopCount()
	if err := r.e.Stop(); err != nil {
		t.Fatalf("Stop 2: %v", err)
	}
	if r.srv.stopCount() != stops {
		t.Fatalf("second Stop issued another StopSession: %d -> %d", stops, r.srv.stopCount())
	}
}

func TestStopWhenIdle(t *testing.T) {
	self := idN(1)
	r := newRig(self, 0, false)
	r.cl.setSnap(soloSnap(self))
	if err := r.e.Stop(); err != nil {
		t.Fatalf("Stop idle: %v", err)
	}
	if _, ok := r.cl.lastPlayback(); ok {
		t.Fatal("idle Stop should not write playback")
	}
}
