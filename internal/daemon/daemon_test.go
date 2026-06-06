package daemon

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/config"
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

	// Mutating stubs whose owning piece has not landed still return errNotImplemented.
	for name, fn := range map[string]error{
		"Adopt":         d.Adopt("addr", "sha256:fp", "pin", "n-id", "name", false),
		"Forget":        d.Forget("id"),
		"SetNodeConfig": d.SetNodeConfig("id", web.NodePatch{}),
	} {
		if !errors.Is(fn, errNotImplemented) {
			t.Errorf("Deps.%s err = %v, want errNotImplemented", name, fn)
		}
	}

	// The P4.9 media/transport/status/calibrate closures are wired (media.go), not
	// errNotImplemented stubs. Before a live session (tx==nil) they degrade: reads
	// return zero values, mutations report errNoSession.
	if _, _, err := d.CalibratePlay(web.CalibrateSel{NodeIDs: []string{"x"}}, 1); !errors.Is(err, errNoSession) {
		t.Errorf("Deps.CalibratePlay (no session) err = %v, want errNoSession", err)
	}
	if files, err := d.ListMedia(""); err != nil || files != nil {
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
