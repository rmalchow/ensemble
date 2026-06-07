package group

import (
	"errors"
	"testing"
	"time"

	"ensemble/internal/contracts"
	"ensemble/internal/dl"
	"ensemble/internal/id"
)

// waitFor polls cond up to d, failing the test on timeout.
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", msg)
}

func TestPlayRejectsFollower(t *testing.T) {
	master, self := idN(1), idN(2)
	r := newRig(self, 5, false)
	r.cl.setSnap(masterSnap(master, defaultSettings(), self))
	if err := r.e.Play("song.wav"); !errors.Is(err, ErrNotMaster) {
		t.Fatalf("err = %v, want ErrNotMaster", err)
	}
}

func TestPlayRejectsBadURI(t *testing.T) {
	self := idN(1)
	r := newRig(self, 5, false)
	r.cl.setSnap(soloSnap(self))
	r.med.err = errors.New("bad uri")
	if err := r.e.Play("garbage"); err == nil {
		t.Fatal("want error")
	}
	if _, ok := r.cl.lastPlayback(); ok {
		t.Fatal("no playback status expected on open failure")
	}
	if r.e.gen != 0 {
		t.Fatalf("gen = %d, want 0 (not consumed)", r.e.gen)
	}
}

func TestPlayDowngradesOpusWithoutCap(t *testing.T) {
	self, follower := idN(1), idN(2)
	r := newRig(self, 5, false)
	// group codec opus; follower lacks opus capability → DOWNGRADE to pcm (not
	// rejected): opus is the default, pcm is the universal fallback.
	s := masterSnap(self, contracts.GroupSettings{Codec: "opus", Transport: "udp", BufferMs: 150}, follower)
	r.cl.setSnap(s)
	if err := r.e.Play("song.wav"); err != nil {
		t.Fatalf("Play should succeed (downgrade), got %v", err)
	}
	defer r.e.Close()
	pc, ok := r.cl.lastPlayback()
	if !ok {
		t.Fatal("no playback status written")
	}
	if pc.pb.Codec != "pcm" {
		t.Fatalf("codec = %q, want pcm (downgraded)", pc.pb.Codec)
	}
}

func TestPlayWritesPlayingStatus(t *testing.T) {
	self := idN(1)
	r := newRig(self, 3, false)
	r.cl.setSnap(soloSnap(self))
	r.srv.stats = contracts.SourceStats{Clients: 1, Connects: 1}
	if err := r.e.Play("song.wav"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	defer r.e.Close()

	pc, ok := r.cl.lastPlayback()
	if !ok {
		t.Fatal("no playback status written")
	}
	if pc.pb.State != "playing" || pc.pb.URI != "song.wav" {
		t.Fatalf("playback = %+v", pc.pb)
	}
	if pc.pb.Codec != "pcm" || pc.pb.Transport != "udp" {
		t.Fatalf("codec/transport = %s/%s", pc.pb.Codec, pc.pb.Transport)
	}
	if pc.pb.Source.Connects != 1 {
		t.Fatalf("Source stats not carried: %+v", pc.pb.Source)
	}
}

func TestPlayBumpsGeneration(t *testing.T) {
	self := idN(1)
	r := newRig(self, 100, true) // live so it doesn't EOF
	r.cl.setSnap(soloSnap(self))
	if err := r.e.Play("input:"); err != nil {
		t.Fatalf("Play 1: %v", err)
	}
	gen1 := r.e.gen
	// Replace with a second play.
	r.med.src = &fakeSource{live: true}
	if err := r.e.Play("input:"); err != nil {
		t.Fatalf("Play 2: %v", err)
	}
	defer r.e.Close()
	if r.e.gen != gen1+1 {
		t.Fatalf("gen = %d, want %d", r.e.gen, gen1+1)
	}
	st, _ := r.srv.lastStart()
	if st.gen != gen1+1 {
		t.Fatalf("StartSession gen = %d, want %d", st.gen, gen1+1)
	}
}

func TestPlayReplacesRunningSession(t *testing.T) {
	self := idN(1)
	r := newRig(self, 100, true)
	r.cl.setSnap(soloSnap(self))
	if err := r.e.Play("input:"); err != nil {
		t.Fatalf("Play 1: %v", err)
	}
	first := r.med.src
	r.med.src = &fakeSource{live: true}
	if err := r.e.Play("input:"); err != nil {
		t.Fatalf("Play 2: %v", err)
	}
	defer r.e.Close()
	// The first source must have been Closed by the replace.
	waitFor(t, time.Second, func() bool {
		first.mu.Lock()
		defer first.mu.Unlock()
		return first.closed
	}, "first source closed")
	if r.srv.stopCount() < 1 {
		t.Fatal("StopSession not called on replace")
	}
}

func TestPlayWaitsForClockSync(t *testing.T) {
	self := idN(1)
	r := newRig(self, 3, false)
	r.cl.setSnap(soloSnap(self))
	r.clk.setOK(false)
	// Flip to synced shortly.
	go func() {
		time.Sleep(20 * time.Millisecond)
		r.clk.setOK(true)
	}()
	if err := r.e.Play("song.wav"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	r.e.Close()
}

func TestPlayUnsyncedTimesOut(t *testing.T) {
	self := idN(1)
	r := newRig(self, 3, false)
	r.cl.setSnap(soloSnap(self))
	r.clk.setOK(false)
	// Use the fake now to expire the wait fast: advance past clockWaitTimeout.
	go func() {
		for i := 0; i < 200; i++ {
			r.advance(50 * time.Millisecond)
			time.Sleep(time.Millisecond)
		}
	}()
	err := r.e.Play("song.wav")
	if !errors.Is(err, ErrNotSynced) {
		t.Fatalf("err = %v, want ErrNotSynced", err)
	}
	if r.e.gen != 0 {
		t.Fatal("gen consumed despite no sync")
	}
}

func TestPlayOpusEncodes(t *testing.T) {
	self := idN(1)
	r := newRig(self, 4, false)
	// self caps include opus; solo group set to opus.
	r.e.p.Caps = contracts.Capabilities{Codecs: []string{"pcm", "opus"}}
	n := node(self, id.Zero, true)
	n.Capabilities.Codecs = []string{"pcm", "opus"}
	g := contracts.GroupView{
		ID:       id.XOR(self),
		Master:   self,
		Members:  []id.ID{self},
		Settings: contracts.GroupSettings{Codec: "opus", Transport: "udp", BufferMs: 150},
	}
	r.cl.setSnap(contracts.Snapshot{Nodes: []contracts.NodeView{n}, Groups: []contracts.GroupView{g}})

	if err := r.e.Play("song.wav"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitFor(t, time.Second, func() bool {
		return len(r.srv.snapshotReleases()) >= 1
	}, "first release")
	r.e.Close()
	// Encoder used → released payloads are the (short) opus packets, not 3840 B.
	rel := r.srv.snapshotReleases()
	if len(rel[0].payload) != 8 {
		t.Fatalf("payload len = %d, want 8 (fake opus packet)", len(rel[0].payload))
	}
}

func TestPlayOpusUnavailableRejected(t *testing.T) {
	self := idN(1)
	r := newRig(self, 4, false)
	r.e.p.Caps = contracts.Capabilities{Codecs: []string{"pcm", "opus"}}
	r.op.err = dl.ErrUnavailable
	n := node(self, id.Zero, true)
	n.Capabilities.Codecs = []string{"pcm", "opus"}
	g := contracts.GroupView{
		ID:       id.XOR(self),
		Master:   self,
		Members:  []id.ID{self},
		Settings: contracts.GroupSettings{Codec: "opus", Transport: "udp", BufferMs: 150},
	}
	r.cl.setSnap(contracts.Snapshot{Nodes: []contracts.NodeView{n}, Groups: []contracts.GroupView{g}})

	if err := r.e.Play("song.wav"); !errors.Is(err, ErrNoOpus) {
		t.Fatalf("err = %v, want ErrNoOpus", err)
	}
	if r.e.gen != 0 {
		t.Fatal("gen consumed despite opus unavailable")
	}
	// underlying source must be closed.
	waitFor(t, time.Second, func() bool {
		r.med.src.mu.Lock()
		defer r.med.src.mu.Unlock()
		return r.med.src.closed
	}, "source closed")
}
