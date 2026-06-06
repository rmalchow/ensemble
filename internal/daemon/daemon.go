// Package daemon is the Ensemble node orchestrator: the wiring layer that
// constructs the always-on subsystems (web listener + mDNS) and, when the node
// is configured, activate()s the full realtime session in-process — with no
// process restart on configure/forget (doc 01 §4.4). It owns the
// activate/deactivate/Configure/forget lifecycle and the generation-fenced role
// loop skeleton (Appendix A.14.4), and it is the sole constructor of the web
// Deps function-value seam (A.14.3) so internal/web never imports the engine
// (doc 01 §2 rule 1).
//
// This is the P0.3 wiring skeleton. The realtime planes (cluster membership,
// clock server, group engine, stream/audio) are referenced as nil-able fields
// constructed by later phases (P1+); their start/stop bodies in applyRole are
// TODO stubs that only flip Status.Role. The lifecycle, the session-guard
// discipline, the listener pre-bind and the role-loop fence are wired now so
// those pieces drop in without restructuring.
//
// daemon may import config, pki, auth, state, web, cluster, discovery, clock,
// group, allowlist, stream/* and audio/* (doc 01 §2 rule 5/6). It is imported
// only by cmd/ensemble.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/config"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/web"
)

// errNotImplemented is returned by the web.Deps closures whose owning piece has
// not landed yet (Adopt/Forget/Leave/SetNodeConfig/CalibratePlay). The web
// layer surfaces it as a 501-class error; later phases replace the stub.
var errNotImplemented = errors.New("not implemented (pending later phase)")

// Options is the wiring contract assembled by cmd from the flag/config overlay
// (doc 01 §5.1) and the persisted Identity (01 §5.2). Realtime pieces (P1 pki/
// auth, P2 cluster/adoption, P3 clock+group, P4 sync) plug their constructors
// and Status fields into this same struct.
type Options struct {
	Paths     config.Paths // resolved data-dir layout (config.OpenDataDir)
	NodeID    string       // stable id from config.Identity (election tiebreak)
	Name      string       // resolved friendly name
	WebPort   int          // control-plane HTTPS base port (retries +1 on conflict)
	ClockPort int          // clock-plane UDP port
	AudioPort int          // audio-plane UDP port
	BindPort  int          // memberlist gossip port
	Seeds     []string     // explicit gossip seeds (config ∪ --join)
	UseMDNS   bool         // mDNS announce/browse enabled
	Device    string       // audio sink device override
	Log       io.Writer    // verbose log sink (nil => discard)

	// WebDevDir, when non-empty, serves web assets from disk instead of the
	// embedded build (on-device iteration). Not a flag in P0.3; reserved for the
	// web-dev seam.
	WebDevDir string

	// Configured reports whether this node has a cluster CA + cluster.yaml (doc
	// 01 §4.4). It is the "configured?" predicate, supplied by cmd so daemon does
	// not hard-depend on the pki symbol that owns it (risk Q1 in the spec). Nil =>
	// a cluster.yaml presence check on Paths.Cluster.
	Configured func() bool
}

// Status is a role/health snapshot for the web layer, read via Deps.Status.
type Status struct {
	Configured bool          // has cluster CA + cluster.yaml
	Active     bool          // full session running
	Role       string        // "master" | "follower" | "starting" | "idle"
	MasterID   string        //
	Members    int           //
	GroupID    string        //
	HaveSync   bool          //
	Offset     time.Duration //
}

// Node is a running orchestrator. All lifecycle methods are idempotent and
// guarded by sessMu (the session fields) / mu (the status snapshot).
type Node struct {
	options Options

	// webPort is the port the web UI actually bound to (Options.WebPort is only
	// the starting point; listenWeb retries +1 on conflict). It is what later
	// phases advertise over mDNS / cluster Meta. 0 => web disabled.
	webPort int

	// rootCtx is the lifetime context (Run's ctx). activate() derives the active
	// session context from it.
	rootCtx context.Context

	// configured is the resolved "has cluster CA + cluster.yaml" predicate.
	configured func() bool

	// activateHook / persistHook are the activate + persist seams Configure drives.
	// They default to the real (*Node).activate / persistCluster in New; tests
	// override them to exercise the activate-first-then-persist contract (the
	// failure path that must leave the node UNCONFIGURED) without a realtime plane.
	activateHook func() error
	persistHook  func()

	// sessMu guards the active-session fields and activeGroup. activate/
	// deactivate hold it; the role loop reads the realtime handles only after
	// activate returns (they are started inside the session under activeCtx).
	sessMu      sync.Mutex
	activeGroup string // "" when unconfigured/inactive
	active      bool
	activeCtx   context.Context
	activeStop  context.CancelFunc

	// Realtime-plane session handles. Typed as the upstream-piece interfaces and
	// left nil in P0.3 (constructed by P2/P3/P4 inside activate). The session-
	// guard discipline around them is wired now.
	mem      membership  // cluster.Membership (P2)
	clockSrv clockServer // clock.Server (P3)
	engine   groupEngine // group.Engine (P3/P4)

	// tx is the live transport seam the media/play/stop/status/calibrate Deps
	// closures (media.go) drive: a state.Store + master resolver + peer proxy +
	// local-render hooks. It is set by activate() once the realtime planes stand
	// up and cleared on deactivate(); nil => the ops degrade to not-ready (the
	// P0.3 skeleton runs with tx==nil). Guarded by sessMu like the other session
	// handles.
	tx *transport

	// fatal carries an unrecoverable active-session error up to Run so the
	// process exits.
	fatal chan error

	mu        sync.Mutex
	roleName  string
	curMaster string
	members   int
	haveSync  bool
	curOffset time.Duration
}

// membership, clockServer and groupEngine are the minimal seams the role loop
// drives. P2/P3/P4 supply concrete types (cluster.Membership, clock.Server,
// group.Engine) that satisfy these; until then activate leaves the fields nil
// and the loop runs with no realtime plane.
type (
	membership  interface{}
	clockServer interface{}
	groupEngine interface{}
)

// New constructs a Node from Options. It is split from Run so tests can drive
// the lifecycle directly without binding sockets.
func New(opts Options) *Node {
	configured := opts.Configured
	if configured == nil {
		configured = func() bool { return clusterFilePresent(opts.Paths) }
	}
	n := &Node{
		options:    opts,
		roleName:   "idle",
		configured: configured,
		fatal:      make(chan error, 1),
	}
	n.activateHook = n.activate
	n.persistHook = n.persistCluster
	return n
}

// Run brings up the always-on subsystems (web server + mDNS), activate()s the
// full session iff configured, then blocks until ctx is cancelled or a fatal
// error. It pre-binds the control listener BEFORE advertising so the actual
// port is known (doc 01 §3.1). The role loop lives inside the active session,
// so Run itself just blocks (mpvsync's node.Run shape).
func Run(ctx context.Context, opts Options) error {
	n := New(opts)
	n.rootCtx = ctx
	defer n.deactivate()

	logf(opts.Log, "ensemble node=%s name=%q", shortID(opts.NodeID), opts.Name)

	// Bind the control listener up front, retrying the port on conflict, so the
	// ACTUAL port is known before any advertise. The web UI is how an
	// unconfigured node is provisioned, so a busy port must never break startup —
	// we take the next free one (doc 01 §3.1/§5.3, listenWeb +1 retry).
	var webLn net.Listener
	if opts.WebPort != 0 {
		ln, port, err := listenWeb(opts.WebPort)
		if err != nil {
			return fmt.Errorf("web listen: %w", err)
		}
		webLn = ln
		n.webPort = port
		logf(opts.Log, "web UI on :%d", port)
	}

	// TODO(P1/P2): always-on mDNS register advertising n.webPort (re-registered
	// with the cluster identity on Configure). Skeleton: mDNS is a no-op stub so
	// the lifecycle is exercisable without the discovery plane.
	n.registerMDNS()
	defer n.deregisterMDNS()

	// Always-on web server (wizard when unconfigured, app when configured). It
	// never imports daemon/engine; everything flows through Deps (A.14.3).
	if webLn != nil {
		srv := web.New(buildDeps(n), opts.WebDevDir)
		go func() {
			if err := srv.Serve(ctx, webLn); err != nil {
				select {
				case n.fatal <- fmt.Errorf("web server: %w", err):
				default:
				}
			}
		}()
	}

	// If configured at startup, activate the full session immediately (no
	// restart on later Configure/forget — doc 01 §4.4).
	if n.configured() {
		if err := n.activate(); err != nil {
			return err
		}
	} else {
		logf(opts.Log, "idle — unconfigured (awaiting wizard/adoption)")
	}

	select {
	case <-ctx.Done():
		return nil
	case err := <-n.fatal:
		return err
	}
}

// Configure is the single activation path shared by the wizard (P1) and
// adoption (P2). It activates FIRST and only persists/advertises on success, so
// a failed activate leaves the node cleanly UNCONFIGURED — never writing
// cluster.yaml (mpvsync's exact rationale, doc 01 §4.4 B5). It is idempotent.
func (n *Node) Configure() error {
	// Activate FIRST; only persist + advertise once the session is actually up.
	// activate() cleans up its own partials on failure.
	if err := n.activateHook(); err != nil {
		return fmt.Errorf("could not start cluster: %w", err)
	}
	// TODO(P1/P2): persist {cluster.yaml + CA} and re-register mDNS with the
	// cluster identity here, AFTER a successful activate. P0.3 has no persist
	// plane yet, so this is a no-op; the activate-first ordering is what matters.
	n.persistHook()
	n.registerMDNS()
	return nil
}

// activate brings up the full active session in-process. It is idempotent and
// guarded by sessMu + activeGroup: already active with the same group => no-op;
// a different group => deactivate then activate (doc 01 §4.4 B5).
func (n *Node) activate() error {
	const group = "default" // TODO(P2): real group id from cluster.yaml

	n.sessMu.Lock()
	if n.active && n.activeGroup == group {
		n.sessMu.Unlock()
		return nil
	}
	if n.active {
		// Moving to a different group: tear the current session down first.
		n.sessMu.Unlock()
		n.deactivate()
		n.sessMu.Lock()
	}
	n.sessMu.Unlock()

	// TODO(P3/P4): construct the realtime planes here, each erroring out (and
	// tearing down the already-constructed ones) on a port conflict so a failed
	// activate is clean:
	//   clockSrv, err := clock.Listen(opts.ClockPort)
	//   mem,      err := cluster.New(...BindPort, Seeds...)
	//   engine,   err := group.New(...AudioPort, Device...)
	// In P0.3 they stay nil; the lifecycle runs with no realtime plane.

	activeCtx, activeStop := context.WithCancel(n.rootCtx)

	// Construct the realtime transport seam (state store + group engine + hooks
	// bound to stream/audio/clock). This is the P4.9 wiring; it is best-effort —
	// a build failure (e.g. unreadable data dir) leaves tx nil so the lifecycle
	// still runs and the media/status closures degrade to not-ready rather than
	// failing activate. The full cluster/pki plane is the pending P2-wiring step;
	// P4.9 stands up the single-node (solo) substrate so a solo group can decode
	// and render.
	tx := n.buildTransport(group)

	n.sessMu.Lock()
	n.activeCtx = activeCtx
	n.activeStop = activeStop
	n.activeGroup = group
	n.active = true
	n.tx = tx
	n.sessMu.Unlock()

	n.setRole("starting")
	logf(n.options.Log, "active group=%q", group)

	go func() { _ = n.loop(activeCtx) }()
	return nil
}

// deactivate tears down the active session: cancels activeCtx (stopping the
// role loop), closes the realtime planes, and clears the session fields. It is
// a no-op when inactive and safe to call multiple times (doc 01 §4.4 B5).
func (n *Node) deactivate() {
	n.sessMu.Lock()
	if !n.active {
		n.sessMu.Unlock()
		return
	}
	stop := n.activeStop
	// Snapshot the realtime handles to close them outside the lock once they exist
	// (P2/P3/P4). In P0.3 they are nil.
	mem := n.mem
	clockSrv := n.clockSrv
	engine := n.engine
	n.active = false
	n.activeGroup = ""
	n.mem = nil
	n.clockSrv = nil
	n.engine = nil
	n.tx = nil
	n.activeCtx = nil
	n.activeStop = nil
	n.sessMu.Unlock()

	if stop != nil {
		stop()
	}
	// Teardown ordering: stop the loop (above) → leave membership → close clock →
	// close engine, so a superseded session never emits on the planes.
	closeIfCloser(mem)
	closeIfCloser(clockSrv)
	closeIfCloser(engine)
	n.setRole("idle")
}

// forget deactivates the session and wipes the cluster state, returning the
// node to UNCONFIGURED (doc 01 §4.4). P2 (/cluster/leave) calls it. It is safe
// to call when already unconfigured.
func (n *Node) forget() error {
	n.deactivate()
	// TODO(P1/P2): wipe cluster.yaml + certs/ so configured() flips to false, then
	// re-register mDNS broadcasting the unconfigured identity. P0.3 has no persist
	// plane, so this only tears the session down.
	n.registerMDNS()
	logf(n.options.Log, "forgotten — now unconfigured")
	return nil
}

// listenWeb binds a TCP listener for the web UI, starting at base and retrying
// base+1, base+2, … when the port is already in use, so several nodes can run
// on one host and a busy port never breaks startup (doc 01 §3.1/§5.3). It
// returns the listener and the actual port. Non-"address in use" errors fail
// fast. Copied verbatim from media/internal/node.listenWeb.
func listenWeb(base int) (net.Listener, int, error) {
	const attempts = 64
	var lastErr error
	for p := base; p < base+attempts; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err == nil {
			return ln, p, nil
		}
		lastErr = err
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, 0, err
		}
	}
	return nil, 0, fmt.Errorf("no free web port in [%d,%d): %w", base, base+attempts, lastErr)
}

// session returns the current active-session handles under sessMu.
func (n *Node) session() (mem membership, clockSrv clockServer, engine groupEngine, active bool) {
	n.sessMu.Lock()
	defer n.sessMu.Unlock()
	return n.mem, n.clockSrv, n.engine, n.active
}

// loop is the per-session role loop (Appendix A.14.4). On each membership/config
// tick it calls applyRole, which idempotently starts/stops the master-vs-
// follower subsystems FENCED by the election generation, so a superseded master
// cannot emit on the planes. In P0.3 the start/stop bodies are stubs that only
// flip Status.Role; the fence + cancel-old-ctx-before-start-new discipline is
// wired now so P3/P4 fill the bodies without restructuring.
func (n *Node) loop(ctx context.Context) error {
	mem, _, _, active := n.session()
	if !active {
		return nil
	}

	// roleState is the loop's fenced role-switch driver. It is split out (and
	// exported to the test via applyRole below) so the control flow can be unit-
	// tested with a fake membership sequence (doc P0.3 §7 T5).
	rs := &roleState{node: n, ctx: ctx, self: n.options.NodeID}
	defer rs.cancel()

	// First evaluation against the current membership.
	rs.apply(electedMaster(mem, n.options.NodeID), generationOf(mem))

	membershipTick := time.NewTicker(2 * time.Second)
	defer membershipTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-membershipTick.C:
			rs.apply(electedMaster(mem, n.options.NodeID), generationOf(mem))
		}
	}
}

// roleState holds the loop-local fence for the current role goroutine. It is the
// "cancel old role ctx before starting the new one, fenced by generation" core
// of A.14.4, factored out so it is testable without a live session.
type roleState struct {
	node *Node
	ctx  context.Context
	self string

	roleCancel context.CancelFunc
	gen        uint64 // generation of the currently-applied role
	hasRole    bool
}

// apply (re)configures the role when the elected master changes. It is fenced by
// generation: a stale (older-or-equal) generation that does not change the
// master is ignored, and a master change cancels the prior role ctx BEFORE
// starting the next so a superseded master cannot keep emitting. In P0.3 the
// start bodies only flip Status.Role (TODO(P3 clock+group / P4 sync)).
func (rs *roleState) apply(master string, gen uint64) {
	rs.node.mu.Lock()
	prevMaster := rs.node.curMaster
	rs.node.curMaster = master
	rs.node.mu.Unlock()

	changed := master != prevMaster || !rs.hasRole
	// Fence: ignore a stale generation that does not change the master.
	if !changed && gen <= rs.gen {
		return
	}
	if !changed {
		// Same master, newer generation: refresh the fence but do not churn the
		// role goroutine.
		rs.gen = gen
		return
	}

	// Master changed: cancel the prior role ctx before starting the next.
	if rs.roleCancel != nil {
		rs.roleCancel()
		rs.roleCancel = nil
	}
	rs.gen = gen
	rs.hasRole = true

	if master == "" {
		rs.node.setRole("starting")
		return
	}

	rctx, rcancel := context.WithCancel(rs.ctx)
	rs.roleCancel = rcancel
	if master == rs.self {
		rs.node.setRole("master")
		rs.node.runMaster(rctx, gen)
	} else {
		rs.node.setRole("follower")
		rs.node.runFollower(rctx, gen, master)
	}
}

// cancel stops the current role goroutine (loop teardown / test cleanup).
func (rs *roleState) cancel() {
	if rs.roleCancel != nil {
		rs.roleCancel()
		rs.roleCancel = nil
	}
}

// electedMaster computes the elected master id from the membership. In P0.3,
// with no membership plane, this node is the sole member and elects itself once
// active (so a configured single node reports role "master"). P3 replaces it
// with the real cluster.Election (lowest-id tiebreak, A.5).
func electedMaster(mem membership, self string) string {
	if mem == nil {
		return self
	}
	return self // TODO(P3): mem.Election().Update(...)
}

// generationOf returns the election generation (A.5). 0 in P0.3 (no election).
func generationOf(mem membership) uint64 {
	if mem == nil {
		return 0
	}
	return 0 // TODO(P3): mem.Generation()
}

// registerMDNS / deregisterMDNS are the always-on discovery hooks. P1/P2 fill
// them with the zeroconf register/close advertising n.webPort. Stubs in P0.3.
func (n *Node) registerMDNS()   { /* TODO(P1/P2): zeroconf announce */ }
func (n *Node) deregisterMDNS() { /* TODO(P1/P2): zeroconf close */ }

// persistCluster writes the cluster.yaml + CA. P1/P2 own it. Stub in P0.3.
func (n *Node) persistCluster() { /* TODO(P1/P2): write cluster.yaml (0600) + certs/ */ }

// setRole records the current role for the Status snapshot.
func (n *Node) setRole(r string) {
	n.mu.Lock()
	n.roleName = r
	n.mu.Unlock()
}

// status assembles the current Status snapshot for the web layer.
func (n *Node) status() Status {
	n.sessMu.Lock()
	active := n.active
	group := n.activeGroup
	n.sessMu.Unlock()

	n.mu.Lock()
	defer n.mu.Unlock()
	return Status{
		Configured: n.configured(),
		Active:     active,
		Role:       n.roleName,
		MasterID:   n.curMaster,
		Members:    n.members,
		GroupID:    group,
		HaveSync:   n.haveSync,
		Offset:     n.curOffset,
	}
}

// logf writes a timestamped line to w (nil => discarded). Verbose log sink.
func logf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "[%s] "+format+"\n", append([]any{ts()}, args...)...)
}

// closeIfCloser closes v if it implements io.Closer (best-effort). Used to tear
// down realtime handles whose concrete type lands in a later phase.
func closeIfCloser(v any) {
	if c, ok := v.(io.Closer); ok && c != nil {
		_ = c.Close()
	}
}

// clusterFilePresent is the default "configured?" predicate: a node is
// configured when cluster.yaml exists (doc 01 §4.4). The CA-presence half is
// added by P1 when pki lands; cmd can override Configured to call pki directly.
func clusterFilePresent(p config.Paths) bool {
	if p.Cluster == "" {
		return false
	}
	return fileExists(p.Cluster)
}

// fileExists reports whether path names an existing (non-directory) file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func ts() string { return time.Now().Format("15:04:05") }

// shortID returns the first 8 chars of an id for logging (full id elsewhere).
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
