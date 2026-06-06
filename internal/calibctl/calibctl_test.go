package calibctl

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// calibctl_test.go drives the fan-out controller against fakes: selection,
// single-agreed-startSample, per-node warnings, render-false/unreachable splits.

func node(id string, render bool, addr string) state.NodeRecord {
	nr := state.NodeRecord{ID: id, Caps: state.Capabilities{Render: render}}
	if addr != "" {
		nr.Addrs = []string{addr}
	}
	return nr
}

func docWith(nodes []state.NodeRecord, groups []state.GroupRecord) func() state.ConfigDoc {
	d := state.ConfigDoc{Nodes: nodes, Groups: groups}
	return func() state.ConfigDoc { return d }
}

// proxyRec captures the (nodeID, startSample) of every proxied call.
type proxyRec struct {
	mu    sync.Mutex
	calls map[string]int64
	err   map[string]error
}

func newProxyRec() *proxyRec { return &proxyRec{calls: map[string]int64{}, err: map[string]error{}} }

func (p *proxyRec) fn(_ context.Context, _ string, nodeID string, startSample int64, _ int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[nodeID] = startSample
	return p.err[nodeID]
}

func TestPlayGroupSelectionSingleStart(t *testing.T) {
	nodes := []state.NodeRecord{
		node("self", true, ""),
		node("n2", true, "10.0.0.2:9000"),
		node("n3", true, "10.0.0.3:9000"),
	}
	groups := []state.GroupRecord{{ID: "g1", MemberNodeIDs: []string{"self", "n2", "n3"}}}

	var localStart int64 = -1
	local := func(_ context.Context, startSample int64, _ int) error { localStart = startSample; return nil }
	px := newProxyRec()
	now := func(string) (int64, bool) { return 1000, true }

	c := NewController(docWith(nodes, groups), "self", local, px.fn, now)
	res, err := c.PlayDetailed(context.Background(), "g1", nil, 5)
	if err != nil {
		t.Fatalf("PlayDetailed: %v", err)
	}

	wantStart := int64(1000) + LeadFrames
	if localStart != wantStart {
		t.Fatalf("local startSample = %d, want %d", localStart, wantStart)
	}
	for _, id := range []string{"n2", "n3"} {
		if px.calls[id] != wantStart {
			t.Fatalf("proxy %s startSample = %d, want %d (all nodes must agree)", id, px.calls[id], wantStart)
		}
	}
	sort.Strings(res.PlayedOn)
	if len(res.PlayedOn) != 3 {
		t.Fatalf("playedOn = %v, want 3 nodes", res.PlayedOn)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none", res.Warnings)
	}
}

func TestPlayNodeIdsSelection(t *testing.T) {
	nodes := []state.NodeRecord{node("self", true, ""), node("n2", true, "10.0.0.2:9000")}
	groups := []state.GroupRecord{{ID: "g1", MemberNodeIDs: []string{"self", "n2"}}}
	px := newProxyRec()
	now := func(string) (int64, bool) { return 500, true }
	c := NewController(docWith(nodes, groups), "self",
		func(context.Context, int64, int) error { return nil }, px.fn, now)

	res, err := c.PlayDetailed(context.Background(), "", []string{"self", "n2"}, 5)
	if err != nil {
		t.Fatalf("PlayDetailed: %v", err)
	}
	// Both share g1 => aligned, single start.
	if px.calls["n2"] != 500+LeadFrames {
		t.Fatalf("n2 start = %d, want aligned %d", px.calls["n2"], 500+LeadFrames)
	}
	if len(res.PlayedOn) != 2 {
		t.Fatalf("playedOn = %v, want 2", res.PlayedOn)
	}
}

func TestPlayRenderFalseWarned(t *testing.T) {
	nodes := []state.NodeRecord{
		node("self", true, ""),
		node("n2", false, "10.0.0.2:9000"), // cannot render
	}
	groups := []state.GroupRecord{{ID: "g1", MemberNodeIDs: []string{"self", "n2"}}}
	px := newProxyRec()
	now := func(string) (int64, bool) { return 0, true }
	c := NewController(docWith(nodes, groups), "self",
		func(context.Context, int64, int) error { return nil }, px.fn, now)

	res, err := c.PlayDetailed(context.Background(), "g1", nil, 5)
	if err != nil {
		t.Fatalf("PlayDetailed: %v", err)
	}
	if len(res.PlayedOn) != 1 || res.PlayedOn[0] != "self" {
		t.Fatalf("playedOn = %v, want [self]", res.PlayedOn)
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("warnings = %v, want 1 (Render=false)", res.Warnings)
	}
	if _, proxied := px.calls["n2"]; proxied {
		t.Fatalf("Render=false node must not be proxied")
	}
}

func TestPlayUnreachableWarned(t *testing.T) {
	nodes := []state.NodeRecord{node("self", true, ""), node("n2", true, "10.0.0.2:9000")}
	groups := []state.GroupRecord{{ID: "g1", MemberNodeIDs: []string{"self", "n2"}}}
	px := newProxyRec()
	px.err["n2"] = errors.New("dial timeout")
	now := func(string) (int64, bool) { return 0, true }
	c := NewController(docWith(nodes, groups), "self",
		func(context.Context, int64, int) error { return nil }, px.fn, now)

	res, err := c.PlayDetailed(context.Background(), "g1", nil, 5)
	if err != nil {
		t.Fatalf("unreachable peer must not be a top-level error: %v", err)
	}
	if len(res.PlayedOn) != 1 || res.PlayedOn[0] != "self" {
		t.Fatalf("playedOn = %v, want [self]", res.PlayedOn)
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("warnings = %v, want 1 (unreachable)", res.Warnings)
	}
}

func TestPlayUnknownGroup(t *testing.T) {
	c := NewController(docWith(nil, nil), "self", nil, nil, func(string) (int64, bool) { return 0, true })
	if _, err := c.PlayDetailed(context.Background(), "ghost", nil, 5); !errors.Is(err, ErrUnknownGroup) {
		t.Fatalf("err = %v, want ErrUnknownGroup", err)
	}
}

func TestPlayUnknownNode(t *testing.T) {
	nodes := []state.NodeRecord{node("self", true, "")}
	c := NewController(docWith(nodes, nil), "self", nil, nil, func(string) (int64, bool) { return 0, true })
	if _, err := c.PlayDetailed(context.Background(), "", []string{"ghost"}, 5); !errors.Is(err, ErrUnknownNode) {
		t.Fatalf("err = %v, want ErrUnknownNode", err)
	}
}

func TestPlayNeitherOrBoth(t *testing.T) {
	c := NewController(docWith(nil, nil), "self", nil, nil, func(string) (int64, bool) { return 0, true })
	if _, err := c.PlayDetailed(context.Background(), "", nil, 5); err == nil {
		t.Fatalf("neither selector should error")
	}
	if _, err := c.PlayDetailed(context.Background(), "g1", []string{"n1"}, 5); err == nil {
		t.Fatalf("both selectors should error")
	}
}

func TestPlayGroupUnsynced(t *testing.T) {
	nodes := []state.NodeRecord{node("self", true, "")}
	groups := []state.GroupRecord{{ID: "g1", MemberNodeIDs: []string{"self"}}}
	c := NewController(docWith(nodes, groups), "self",
		func(context.Context, int64, int) error { return nil }, nil,
		func(string) (int64, bool) { return 0, false }) // never synced
	if _, err := c.PlayDetailed(context.Background(), "g1", nil, 5); !errors.Is(err, ErrNotSynced) {
		t.Fatalf("err = %v, want ErrNotSynced", err)
	}
}

func TestPlaySelfNotTarget(t *testing.T) {
	// self is not in the selection => local player is never called.
	nodes := []state.NodeRecord{node("self", true, ""), node("n2", true, "10.0.0.2:9000")}
	groups := []state.GroupRecord{{ID: "g1", MemberNodeIDs: []string{"n2"}}}
	px := newProxyRec()
	localCalled := false
	now := func(string) (int64, bool) { return 0, true }
	c := NewController(docWith(nodes, groups), "self",
		func(context.Context, int64, int) error { localCalled = true; return nil }, px.fn, now)

	res, err := c.PlayDetailed(context.Background(), "g1", nil, 5)
	if err != nil {
		t.Fatalf("PlayDetailed: %v", err)
	}
	if localCalled {
		t.Fatalf("local player must not run when self is not a target")
	}
	if len(res.PlayedOn) != 1 || res.PlayedOn[0] != "n2" {
		t.Fatalf("playedOn = %v, want [n2]", res.PlayedOn)
	}
}

func TestPlayUngroupedNodesBestEffort(t *testing.T) {
	// Two nodes that do NOT share a group => no alignment; informational warning.
	nodes := []state.NodeRecord{node("self", true, ""), node("n2", true, "10.0.0.2:9000")}
	groups := []state.GroupRecord{
		{ID: "g1", MemberNodeIDs: []string{"self"}},
		{ID: "g2", MemberNodeIDs: []string{"n2"}},
	}
	px := newProxyRec()
	now := func(string) (int64, bool) { return 999, true }
	c := NewController(docWith(nodes, groups), "self",
		func(context.Context, int64, int) error { return nil }, px.fn, now)

	res, err := c.PlayDetailed(context.Background(), "", []string{"self", "n2"}, 5)
	if err != nil {
		t.Fatalf("PlayDetailed: %v", err)
	}
	// No common group => startSample 0 (immediate per-node), plus an info warning.
	if px.calls["n2"] != 0 {
		t.Fatalf("ungrouped n2 start = %d, want 0 (no alignment)", px.calls["n2"])
	}
	if len(res.PlayedOn) != 2 {
		t.Fatalf("playedOn = %v, want 2", res.PlayedOn)
	}
	foundInfo := false
	for _, w := range res.Warnings {
		if w == "nodes not in a common group: played per-node from each node's own now (no cross-node alignment)" {
			foundInfo = true
		}
	}
	if !foundInfo {
		t.Fatalf("warnings = %v, want the no-common-group info note", res.Warnings)
	}
}

func TestLeadFramesValue(t *testing.T) {
	if LeadFrames != 14400 {
		t.Fatalf("LeadFrames = %d, want 14400 (A.12 LeadMs 300 @ 48000)", LeadFrames)
	}
}
