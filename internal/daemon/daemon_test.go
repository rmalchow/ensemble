package daemon

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/cluster"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/config"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/discovery"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/web"
)

// newTestNode builds a Node with a controllable configured() predicate and a
// cancelled-able root ctx, so the lifecycle can be driven without sockets.
func newTestNode(t *testing.T, configured *atomic.Bool) (*Node, context.CancelFunc) {
	t.Helper()
	n := New(Options{
		NodeID:     "0123456789abcdef0123456789abcdef",
		Name:       "test",
		Configured: func() bool { return configured.Load() },
	})
	ctx, cancel := context.WithCancel(context.Background())
	n.rootCtx = ctx
	return n, cancel
}

// TestLifecycle covers the unconfigured → activate → deactivate → forget state
// machine (doc 01 §4.4 B5, P0.3 §7 T3).
func TestLifecycle(t *testing.T) {
	var configured atomic.Bool
	n, cancel := newTestNode(t, &configured)
	defer cancel()

	// Boot unconfigured: no active session, role idle.
	if st := n.status(); st.Configured || st.Active || st.Role != "idle" {
		t.Fatalf("initial status = %+v, want Configured=false Active=false Role=idle", st)
	}

	// Configure (CA present): activate runs, session active.
	configured.Store(true)
	if err := n.Configure(); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if st := n.status(); !st.Active {
		t.Fatalf("after Configure status = %+v, want Active=true", st)
	}
	group := n.activeGroup

	// Second Configure with the same group is a no-op (idempotent): still active,
	// same group.
	if err := n.Configure(); err != nil {
		t.Fatalf("second Configure: %v", err)
	}
	if !n.active || n.activeGroup != group {
		t.Fatalf("idempotent Configure changed session: active=%v group=%q", n.active, n.activeGroup)
	}

	// forget: deactivate; configured() flips false (the fake) so we are back to
	// UNCONFIGURED.
	configured.Store(false)
	if err := n.forget(); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if st := n.status(); st.Active || st.Configured || st.Role != "idle" {
		t.Fatalf("after forget status = %+v, want Active=false Configured=false Role=idle", st)
	}

	// forget again is safe (no-op).
	if err := n.forget(); err != nil {
		t.Fatalf("second forget: %v", err)
	}
}

// TestActivateFailureLeavesUnconfigured asserts that a failed activate returns a
// wrapped error AND does not persist cluster state (P0.3 §7 T3 failure path). We
// inject the failure by overriding the activate hook via a test-only field; in
// P0.3 activate has no failure mode of its own, so we simulate the contract:
// Configure must call activate BEFORE persist, and a persist must not run if
// activate fails.
func TestConfigureActivatesBeforePersist(t *testing.T) {
	var persisted atomic.Bool
	n := New(Options{NodeID: "n", Configured: func() bool { return false }})
	n.rootCtx = context.Background()
	n.persistHook = func() { persisted.Store(true) }
	n.activateHook = func() error { return errors.New("port busy") }

	err := n.Configure()
	if err == nil {
		t.Fatal("Configure: want error from failed activate, got nil")
	}
	if !strings.Contains(err.Error(), "port busy") {
		t.Fatalf("Configure error = %v, want to wrap the activate failure", err)
	}
	if persisted.Load() {
		t.Fatal("persist ran despite a failed activate (must persist only on success)")
	}
	if n.active {
		t.Fatal("node active despite a failed activate")
	}
}

// TestListenWebRetry covers the +1-on-conflict retry (doc 01 §3.1/§5.3, P0.3 §7
// T4): with port P held, listenWeb(P) returns P+1.
func TestListenWebRetry(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold listener: %v", err)
	}
	defer held.Close()
	base := held.Addr().(*net.TCPAddr).Port

	// listenWeb binds on all interfaces (":port"); the held 127.0.0.1 socket makes
	// that base busy, so we should get base+1 (or higher if base+1 is also busy).
	ln, port, err := listenWeb(base)
	if err != nil {
		t.Fatalf("listenWeb(%d): %v", base, err)
	}
	defer ln.Close()
	if port <= base {
		t.Fatalf("listenWeb(%d) port = %d, want > %d", base, port, base)
	}
}

// TestApplyRoleFence covers the generation-fenced role switch (Appendix A.14.4,
// P0.3 §7 T5): a master→follower transition cancels the prior role ctx before
// starting the next, and a stale generation that does not change the master is
// ignored.
func TestApplyRoleFence(t *testing.T) {
	n := New(Options{NodeID: "self"})
	rs := &roleState{node: n, ctx: context.Background(), self: "self"}
	defer rs.cancel()

	// Become master.
	rs.apply("self", 1)
	if got := n.status().Role; got != "master" {
		t.Fatalf("role = %q, want master", got)
	}
	masterCtxCancelled := captureCancel(rs)

	// Transition to a different master: the prior (master) role ctx must be
	// cancelled before the follower role starts.
	rs.apply("other", 2)
	if got := n.status().Role; got != "follower" {
		t.Fatalf("role = %q, want follower", got)
	}
	if !masterCtxCancelled() {
		t.Fatal("master role ctx was not cancelled on transition to follower")
	}

	// Stale generation, same master: ignored (no role churn, fence holds).
	before := rs.gen
	rs.apply("other", 1) // older generation, same master
	if rs.gen != before {
		t.Fatalf("stale generation advanced the fence: gen %d -> %d", before, rs.gen)
	}
}

// captureCancel wraps the current role ctx so the test can observe whether it
// was cancelled by a subsequent apply. It re-derives the cancel via the parent
// ctx already stored in rs; we instead probe by replacing roleCancel with a
// recording wrapper.
func captureCancel(rs *roleState) func() bool {
	orig := rs.roleCancel
	cancelled := false
	rs.roleCancel = func() {
		cancelled = true
		if orig != nil {
			orig()
		}
	}
	return func() bool { return cancelled }
}

// TestDepsSeam covers the web.Deps assembly (P0.3 §7 T6): mutating stubs return
// errNotImplemented, State returns an (empty) ConfigView, and SetupStatus/Status
// read live node data.
func TestDepsSeam(t *testing.T) {
	var configured atomic.Bool
	configured.Store(true)
	n := New(Options{
		NodeID:     "abcd1234abcd1234",
		Name:       "kitchen",
		Paths:      config.Paths{Root: "/tmp/x"},
		Configured: func() bool { return configured.Load() },
	})

	d := buildDeps(n)

	// Identity passthrough.
	if d.NodeID != "abcd1234abcd1234" {
		t.Errorf("Deps.NodeID = %q, want abcd1234abcd1234", d.NodeID)
	}
	if d.Paths.Root != "/tmp/x" {
		t.Errorf("Deps.Paths.Root = %q, want /tmp/x", d.Paths.Root)
	}

	// SetNodeConfig is WIRED (§D.3): an unknown node id on the current version
	// surfaces ErrNotFound.
	if err := d.SetNodeConfig("id", web.NodePatch{}, 0); !errors.Is(err, web.ErrNotFound) {
		t.Errorf("Deps.SetNodeConfig err = %v, want ErrNotFound", err)
	}
	// NodeDetail is WIRED (§D.2): unknown id => ok=false.
	if _, ok := d.NodeDetail("nope"); ok {
		t.Error("Deps.NodeDetail(unknown) ok = true, want false")
	}

	// Adopt is now WIRED (GAP 2): on this unconfigured node (no cluster CA) it
	// surfaces a not-ready (ErrUnreachable-class) error rather than errNotImplemented.
	if err := d.Adopt("addr", "sha256:fp", "pin", "n-id", "name", "", false); !errors.Is(err, web.ErrUnreachable) {
		t.Errorf("Deps.Adopt (unconfigured) err = %v, want ErrUnreachable", err)
	}
	// Forget is WIRED (GAP 2): forgetting an unknown node id returns ErrNotFound.
	if err := d.Forget("nope"); !errors.Is(err, web.ErrNotFound) {
		t.Errorf("Deps.Forget (unknown id) err = %v, want ErrNotFound", err)
	}

	// The P4.9 media/transport/status/calibrate closures are wired (media.go), not
	// errNotImplemented stubs. Before a live session (tx==nil) they degrade: reads
	// return zero values, mutations report errNoSession.
	if _, _, err := d.CalibratePlay(web.CalibrateSel{NodeIDs: []string{"x"}}, 1); !errors.Is(err, errNoSession) {
		t.Errorf("Deps.CalibratePlay (no session) err = %v, want errNoSession", err)
	}
	if files, _, err := d.ListMedia("", ""); err != nil || files != nil {
		t.Errorf("Deps.ListMedia (no session) = (%v, %v), want (nil, nil)", files, err)
	}
	if _, err := d.GroupStatus("g"); !errors.Is(err, web.ErrGroupNotReady) {
		t.Errorf("Deps.GroupStatus (no session) err = %v, want ErrGroupNotReady", err)
	}

	// Read closures return zero/live values without panicking.
	if got := d.State(); !reflect_DeepEqualEmpty(got) {
		t.Errorf("Deps.State() = %+v, want empty ConfigView", got)
	}
	if got := d.SetupStatus(); !got.Configured || got.Name != "kitchen" || got.NodeID != "abcd1234abcd1234" {
		t.Errorf("Deps.SetupStatus() = %+v, want {Configured:true Name:kitchen NodeID:abcd1234abcd1234}", got)
	}
	if got := d.Status(); got.Role == "" {
		t.Errorf("Deps.Status() role empty, want a role string")
	}

	// Leave is wired to the live forget() hook (not a stub) and succeeds.
	if err := d.Leave(); err != nil {
		t.Errorf("Deps.Leave() = %v, want nil (wired to forget)", err)
	}
}

func reflect_DeepEqualEmpty(v web.ConfigView) bool {
	return v.Version == 0 && len(v.Nodes) == 0 && len(v.Groups) == 0
}

// TestConfigViewNonNilSlices pins that the projection never emits nil slices: a
// genesis NodeRecord has no Addrs, and a JSON null there crashes the SPA's
// members-table render (`addrs.join` TypeError).
func TestConfigViewNonNilSlices(t *testing.T) {
	v := configView(state.ConfigDoc{
		Nodes:  []state.NodeRecord{{ID: "a"}},
		Groups: []state.GroupRecord{{ID: "default"}},
	})
	n := v.Nodes[0]
	if n.Addrs == nil || n.Caps.Sinks == nil || n.Caps.EncodeCodecs == nil ||
		n.Caps.DecodeCodecs == nil || n.Caps.FEC == nil {
		t.Fatalf("configView emitted nil slices (JSON null): %+v", n)
	}
	if v.Groups[0].MemberNodeIDs == nil {
		t.Fatalf("configView emitted nil MemberNodeIDs: %+v", v.Groups[0])
	}
}

// TestStoreDiscoveredControlAddr pins that the discovery cache advertises the
// target's CONTROL endpoint as host:port. A bare IP would make the adopt flow
// dial the default control port — on a multi-node host that is a DIFFERENT node
// (usually ourselves), whose member-closed bootstrap answers with a misleading
// "foreign cluster" refusal.
func TestStoreDiscoveredControlAddr(t *testing.T) {
	n := New(Options{NodeID: "self"})
	n.storeDiscovered([]discovery.DiscoveredNode{
		{NodeID: "n-1", Name: "B", Addr: "192.0.2.10", ControlPort: 8444},
		{NodeID: "n-2", Name: "C", Addr: "192.0.2.11"}, // no ctrl TXT: bare IP fallback
		{NodeID: "self", Addr: "192.0.2.1", ControlPort: 8443},
	})
	d := buildDeps(n).Discovery()
	if len(d) != 2 {
		t.Fatalf("discovered = %+v, want 2 rows (self filtered)", d)
	}
	if got := d[0].Addrs[0]; got != "192.0.2.10:8444" {
		t.Errorf("discovered addr = %q, want host:ctrlPort 192.0.2.10:8444", got)
	}
	if got := d[1].Addrs[0]; got != "192.0.2.11" {
		t.Errorf("no-ctrl discovered addr = %q, want bare IP fallback", got)
	}
}

// TestSyncSelfAddrs pins the renumber behavior: a node whose host IPs changed
// rewrites ITS OWN NodeRecord.Addrs (gossiped to peers), never another node's,
// and never wipes the record when interface info is unavailable.
func TestSyncSelfAddrs(t *testing.T) {
	n := New(Options{NodeID: "self"})
	seed := state.ConfigDoc{Nodes: []state.NodeRecord{
		{ID: "self", Addrs: []string{"10.0.0.5"}},
		{ID: "peer", Addrs: []string{"10.0.0.9"}},
	}}
	if _, err := n.store.Apply(seed); err != nil {
		t.Fatal(err)
	}

	// Renumbered: own record updates, the peer's is untouched, version bumps.
	n.syncSelfAddrs([]string{"192.168.1.7", "fd00::7"})
	doc := n.store.Get()
	if got := doc.Nodes[0].Addrs; len(got) != 2 || got[0] != "192.168.1.7" {
		t.Fatalf("self addrs = %v, want the new IPs", got)
	}
	if got := doc.Nodes[1].Addrs; len(got) != 1 || got[0] != "10.0.0.9" {
		t.Fatalf("peer addrs touched: %v", got)
	}
	v := doc.Version

	// Unchanged set (any order): no write, no version churn.
	n.syncSelfAddrs([]string{"fd00::7", "192.168.1.7"})
	if n.store.Get().Version != v {
		t.Fatal("unchanged addrs must not bump the version")
	}

	// No interface info: never wipe.
	n.syncSelfAddrs(nil)
	if got := n.store.Get().Nodes[0].Addrs; len(got) != 2 {
		t.Fatalf("empty input wiped addrs: %v", got)
	}
}

// TestSyncSelfRender pins the 06 §1.5 last-resort fallback: no usable sink ⇒
// the node auto-disables its own render (control-only) and marks RenderAutoOff;
// a sink returning re-enables it; an operator's explicit Render=false is never
// overridden.
func TestSyncSelfRender(t *testing.T) {
	n := New(Options{NodeID: "self"})
	seed := state.ConfigDoc{Nodes: []state.NodeRecord{
		{ID: "self", Caps: state.Capabilities{Render: true}},
	}}
	if _, err := n.store.Apply(seed); err != nil {
		t.Fatal(err)
	}

	// No usable sink: render flips off, marked auto.
	n.syncSelfRender(false)
	nr := n.store.Get().Nodes[0]
	if nr.Caps.Render || !nr.RenderAutoOff {
		t.Fatalf("after no-sink: %+v, want Render=false RenderAutoOff=true", nr)
	}

	// Sink back: auto-disabled render recovers.
	n.syncSelfRender(true)
	nr = n.store.Get().Nodes[0]
	if !nr.Caps.Render || nr.RenderAutoOff {
		t.Fatalf("after recovery: %+v, want Render=true RenderAutoOff=false", nr)
	}

	// Operator-forced off (no auto flag): NEVER auto re-enabled.
	doc := n.store.Get()
	doc.Nodes[0].Caps.Render = false
	if _, err := n.store.Apply(doc); err != nil {
		t.Fatal(err)
	}
	n.syncSelfRender(true)
	if n.store.Get().Nodes[0].Caps.Render {
		t.Fatal("operator-forced Render=false must not be auto re-enabled")
	}
	v := n.store.Get().Version
	n.syncSelfRender(true) // consistent state: no version churn
	if n.store.Get().Version != v {
		t.Fatal("consistent render state must not bump the version")
	}
}

// TestClaimMasterHint pins the incumbency anchor: the elected master writes
// itself as the group's MasterHint when the hint is empty or names a DEAD
// predecessor — and never contests a live hinted node. With the hint anchored,
// a lower-id joiner no longer steals mastership mid-stream (the A.5 election
// honors a live hint over the lowest id).
func TestClaimMasterHint(t *testing.T) {
	n := New(Options{NodeID: "bbbb"})
	seed := state.ConfigDoc{
		Nodes:  []state.NodeRecord{{ID: "bbbb"}},
		Groups: []state.GroupRecord{{ID: "default", MemberNodeIDs: []string{"bbbb"}}},
	}
	if _, err := n.store.Apply(seed); err != nil {
		t.Fatal(err)
	}
	cp := newClusterPlane(n, "default") // no gossip: every peer is "dead"

	// Empty hint: the master claims it.
	n.claimMasterHint(cp, "bbbb")
	if got := n.store.Get().Groups[0].MasterHint; got != "bbbb" {
		t.Fatalf("hint = %q, want bbbb (claimed)", got)
	}
	v := n.store.Get().Version

	// Already anchored: no churn.
	n.claimMasterHint(cp, "bbbb")
	if n.store.Get().Version != v {
		t.Fatal("anchored hint must not bump the version")
	}

	// Not the master / no master: never claims.
	doc := n.store.Get()
	doc.Groups[0].MasterHint = "dead-node"
	if _, err := n.store.Apply(doc); err != nil {
		t.Fatal(err)
	}
	n.claimMasterHint(cp, "someone-else")
	n.claimMasterHint(cp, "")
	if got := n.store.Get().Groups[0].MasterHint; got != "dead-node" {
		t.Fatalf("hint = %q, want dead-node (untouched)", got)
	}

	// Dead predecessor: the failover master claims over it.
	n.claimMasterHint(cp, "bbbb")
	if got := n.store.Get().Groups[0].MasterHint; got != "bbbb" {
		t.Fatalf("hint = %q, want bbbb (claimed over dead predecessor)", got)
	}

	// And the anchored hint WINS the election against a lower-id joiner: with
	// both alive, the hint holds mastership (A.5 hint-over-lowest).
	el := cluster.NewGroupElections("bbbb")
	doc = n.store.Get()
	doc.Groups[0].MemberNodeIDs = []string{"aaaa", "bbbb"}
	alive := []cluster.Member{
		{Meta: cluster.Meta{NodeID: "aaaa", GroupID: "default"}},
		{Meta: cluster.Meta{NodeID: "bbbb", GroupID: "default"}},
	}
	el.Recompute(doc, alive)
	if got := el.Master("default"); got != "bbbb" {
		t.Fatalf("master = %q, want bbbb (anchored incumbent beats lower-id joiner)", got)
	}
}
