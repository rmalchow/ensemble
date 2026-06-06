package group

// Integration test (pure-Go, no UDP, no hardware, no sleeps): two+ concurrent
// groups, each with its own master / clock / timeline / streamGen, driven through
// a scripted multi-group sequence. It proves the P6.1 invariant — cross-group
// isolation (A.13 P6 / doc 04 §4.1.2, §4.6) — at the engine + timeline level. The
// channel-role-across-the-pair half lives in multigroup_render_test.go (external
// package, so it may import internal/audio/render without a cycle).

import (
	"sync"
	"testing"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/cluster"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// mgRecorder records per-group hook calls (concurrency-safe for -race).
type mgRecorder struct {
	mu    sync.Mutex
	calls map[string][]string
}

func newMGRecorder() *mgRecorder { return &mgRecorder{calls: map[string][]string{}} }

func (r *mgRecorder) logc(g, name string) {
	r.mu.Lock()
	r.calls[g] = append(r.calls[g], name)
	r.mu.Unlock()
}

func (r *mgRecorder) snapshot(g string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls[g]...)
}

func (r *mgRecorder) hooksFor(g string) Hooks {
	return Hooks{
		StartClockServer:   func(string) error { r.logc(g, "StartClockServer"); return nil },
		StopClockServer:    func() { r.logc(g, "StopClockServer") },
		StartOrigin:        func(string, uint64) error { r.logc(g, "StartOrigin"); return nil },
		StopOrigin:         func() { r.logc(g, "StopOrigin") },
		OriginResumeAt:     func(int64, bool) { r.logc(g, "OriginResumeAt") },
		StartClockFollower: func(string, string) error { r.logc(g, "StartClockFollower"); return nil },
		StopClockFollower:  func() { r.logc(g, "StopClockFollower") },
		StartReceiver:      func(string) error { r.logc(g, "StartReceiver"); return nil },
		StopReceiver:       func() { r.logc(g, "StopReceiver") },
		StartRender:        func() error { r.logc(g, "StartRender"); return nil },
		StopRender:         func() { r.logc(g, "StopRender") },
	}
}

// TestMultiGroup_FailoverIsolation: two concurrent groups, A (master n1) and B
// (master n3). Killing A's master re-elects A and bumps A's streamGen + re-points
// A's engines, while B's election outcome, streamGen, and lifecycle hooks are
// completely unchanged (doc 04 §4.6 "re-run isolates to the affected group").
func TestMultiGroup_FailoverIsolation(t *testing.T) {
	rec := newMGRecorder()
	r := NewRegistry("n2", rec.hooksFor) // self is a render-capable member of A

	nodes := []state.NodeRecord{
		renderNode("n1"), renderNode("n2"), renderNode("n3"), renderNode("n4"),
	}
	doc := state.ConfigDoc{
		Nodes: nodes,
		Groups: []state.GroupRecord{
			{ID: "A", MemberNodeIDs: []string{"n1", "n2"}},
			{ID: "B", MemberNodeIDs: []string{"n3", "n4"}},
		},
	}

	r.OnState(doc, map[string]cluster.Outcome{
		"A": {MasterID: "n1", Generation: 1},
		"B": {MasterID: "n3", Generation: 1},
	})

	bBefore := r.Decisions()["B"]
	bCallsBefore := rec.snapshot("B")
	aGenBefore := r.Decisions()["A"].StreamGen

	// "Kill" A's master: re-elect n2 (self) as A's master, new generation. B's
	// outcome is absent ⇒ carried forward unchanged.
	r.OnState(doc, map[string]cluster.Outcome{
		"A": {MasterID: "n2", Generation: 2, IsSelf: true},
	})

	aAfter := r.Decisions()["A"]
	bAfter := r.Decisions()["B"]

	if !aAfter.IsMaster || !aAfter.RunOrigin {
		t.Fatalf("A after failover = %+v, want self-master origin", aAfter)
	}
	if aAfter.StreamGen <= aGenBefore {
		t.Fatalf("A streamGen did not advance on failover: %d -> %d", aGenBefore, aAfter.StreamGen)
	}
	if bAfter != bBefore {
		t.Fatalf("B decision changed during A failover: %+v -> %+v", bBefore, bAfter)
	}
	if got := rec.snapshot("B"); len(got) != len(bCallsBefore) {
		t.Fatalf("B got new hook calls during A failover: %v -> %v", bCallsBefore, got)
	}
}

// TestMultiGroup_StreamGenIndependence: bumping A's streamGen (master re-key) does
// not change B's streamGen — each group keeps its own generation sequence.
func TestMultiGroup_StreamGenIndependence(t *testing.T) {
	rec := newMGRecorder()
	r := NewRegistry("n1", rec.hooksFor) // self masters A

	nodes := []state.NodeRecord{renderNode("n1"), renderNode("n2"), renderNode("n3")}
	doc := state.ConfigDoc{
		Nodes: nodes,
		Groups: []state.GroupRecord{
			{ID: "A", MemberNodeIDs: []string{"n1", "n2"}},
			{ID: "B", MemberNodeIDs: []string{"n3"}},
		},
	}
	r.OnState(doc, map[string]cluster.Outcome{
		"A": {MasterID: "n1", Generation: 1, IsSelf: true},
		"B": {MasterID: "n3", Generation: 1},
	})
	bGen := r.Decisions()["B"].StreamGen

	r.OnState(doc, map[string]cluster.Outcome{
		"A": {MasterID: "n1", Generation: 2, IsSelf: true},
	})
	if r.Decisions()["B"].StreamGen != bGen {
		t.Fatalf("B streamGen changed when only A re-keyed: %d -> %d",
			bGen, r.Decisions()["B"].StreamGen)
	}
}

// TestMultiGroup_TimelineNonInterference: two FollowerTimelines anchored on
// DIFFERENT (sampleIndex, masterMono) produce independent NowSample() values from
// the same wall clock — verifying the per-group timeline isolation 04 §4.1.2
// requires (no shared mutable state). Internal test so it can inject nowMono.
func TestMultiGroup_TimelineNonInterference(t *testing.T) {
	const rate = 48000
	var now int64 = 1_000_000_000 // shared monotonic clock (ns)
	clkA := zeroOffsetClock{}
	clkB := zeroOffsetClock{}

	chA := &mgChunks{meta: ChunkMeta{SampleIndex: 0, MasterMono: now, StreamGen: 1, Playing: true}}
	chB := &mgChunks{meta: ChunkMeta{SampleIndex: 480_000, MasterMono: now, StreamGen: 1, Playing: true}}

	tlA := NewFollowerTimeline(chA, clkA, rate)
	tlB := NewFollowerTimeline(chB, clkB, rate)
	tlA.SetStreamGen(1)
	tlB.SetStreamGen(1)
	tlA.nowMono = func() int64 { return now }
	tlB.nowMono = func() int64 { return now }

	now += 1_000_000_000 // advance 1 s of wall time

	sA, okA, _ := tlA.NowSample()
	sB, okB, _ := tlB.NowSample()
	if !okA || !okB {
		t.Fatalf("timelines not synced: A.ok=%v B.ok=%v", okA, okB)
	}
	if sA != 48_000 {
		t.Fatalf("A NowSample=%d, want 48000", sA)
	}
	if sB != 480_000+48_000 {
		t.Fatalf("B NowSample=%d, want %d", sB, 480_000+48_000)
	}
	if sB-sA != 480_000 {
		t.Fatalf("timeline cross-talk: sB-sA=%d, want 480000 (anchor diff)", sB-sA)
	}
}

// ── fakes (internal) ──────────────────────────────────────────────────────────

type zeroOffsetClock struct{}

func (zeroOffsetClock) Offset() (time.Duration, bool)   { return 0, true }
func (zeroOffsetClock) MinDelay() (time.Duration, bool) { return 0, true }

type mgChunks struct{ meta ChunkMeta }

func (m *mgChunks) LatestChunkMeta() (ChunkMeta, bool) { return m.meta, true }
