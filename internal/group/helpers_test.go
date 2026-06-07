package group

import (
	"net/netip"
	"sync"
	"time"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// testRig bundles an Engine with all its fakes for easy assertion.
type testRig struct {
	e   *Engine
	cl  *fakeCluster
	fc  *fakeFollowClient
	med *fakeMedia
	srv *fakeSourceServer
	sub *fakeSubscriber
	snk *fakeSink
	clk *fakeClock
	cc  *fakeClockCtl
	op  *fakeOpusFactory

	nowMu sync.Mutex
	now   time.Time
}

// newRig builds a rig whose self is `self`, with a pull source of n frames.
func newRig(self id.ID, srcFrames int, live bool) *testRig {
	r := &testRig{
		cl:  newFakeCluster(self),
		fc:  &fakeFollowClient{},
		med: &fakeMedia{src: &fakeSource{remaining: srcFrames, live: live}},
		srv: &fakeSourceServer{},
		sub: &fakeSubscriber{},
		snk: &fakeSink{},
		clk: &fakeClock{ok: true},
		cc:  &fakeClockCtl{},
		op:  &fakeOpusFactory{},
		now: time.Unix(1_700_000_000, 0),
	}
	r.e = New(Params{
		Cluster:  r.cl,
		Media:    r.med,
		Opus:     r.op,
		Source:   r.srv,
		Sub:      r.sub,
		Sink:     r.snk,
		Clock:    r.clk,
		ClockCtl: r.cc,
		Follow:   r.fc,
		Caps:     contracts.Capabilities{Codecs: []string{"pcm"}},
		now:      r.nowFn(),
	})
	return r
}

func (r *testRig) nowFn() func() time.Time {
	return func() time.Time {
		r.nowMu.Lock()
		defer r.nowMu.Unlock()
		return r.now
	}
}

func (r *testRig) advance(d time.Duration) {
	r.nowMu.Lock()
	r.now = r.now.Add(d)
	r.nowMu.Unlock()
}

// --- snapshot builders -------------------------------------------------------

func node(nid id.ID, following id.ID, alive bool) contracts.NodeView {
	return contracts.NodeView{
		ID:           nid,
		Name:         nid.String()[:8],
		Following:    following,
		Alive:        alive,
		SourcePort:   9200,
		StreamPort:   9090,
		Capabilities: contracts.Capabilities{Codecs: []string{"pcm"}},
	}
}

// soloSnap makes self a solo master (group of 1).
func soloSnap(self id.ID) contracts.Snapshot {
	n := node(self, id.Zero, true)
	g := contracts.GroupView{
		ID:       id.XOR(self),
		Master:   self,
		Members:  []id.ID{self},
		Settings: defaultSettings(),
		Playback: contracts.Playback{State: "idle"},
	}
	return contracts.Snapshot{Nodes: []contracts.NodeView{n}, Groups: []contracts.GroupView{g}}
}

// masterSnap makes `master` master over `members` (master included), all alive.
func masterSnap(master id.ID, settings contracts.GroupSettings, members ...id.ID) contracts.Snapshot {
	all := append([]id.ID{master}, members...)
	var nodes []contracts.NodeView
	nodes = append(nodes, node(master, id.Zero, true))
	for _, m := range members {
		nodes = append(nodes, node(m, master, true))
	}
	g := contracts.GroupView{
		ID:       id.XOR(all...),
		Master:   master,
		Members:  all,
		Settings: settings,
		Playback: contracts.Playback{State: "idle"},
	}
	return contracts.Snapshot{Nodes: nodes, Groups: []contracts.GroupView{g}}
}

func addrFor(n contracts.NodeView, port int) netip.AddrPort {
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), uint16(port))
}

// idN returns a deterministic non-zero ID for tests.
func idN(b byte) id.ID {
	var i id.ID
	i[15] = b
	return i
}
