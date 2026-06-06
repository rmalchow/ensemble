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
	"crypto"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	sink "gitlab.rand0m.me/ruben/go/ensemble/internal/audio/sink"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/auth"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/cluster"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/config"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/discovery"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/pki"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/web"
)

// (Historical note: stub closures used to return a shared errNotImplemented;
// every web.Deps closure is now wired to its owning piece.)

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
	Version   string       // software version string (for /bootstrap/info; "" => "dev")

	// WebDevDir, when non-empty, serves web assets from disk instead of the
	// embedded build (on-device iteration). Not a flag in P0.3; reserved for the
	// web-dev seam.
	WebDevDir string

	// OpenSink overrides the audio output device constructor (default
	// sink.Open(nil, Device)). Tests inject a capturing fake so a two-node
	// playback session runs in-process without a real device.
	OpenSink func() (sink.AudioSink, error)

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

	// plane is the per-session multi-node substrate (gossip membership +
	// per-group elections + allowlist + peer cache, plane.go). Built by
	// activate(), closed by deactivate(). nil before activate / in the P0.3
	// skeleton paths. Guarded by sessMu like the other session handles.
	plane *clusterPlane

	// loopDone closes when the session's role loop goroutine exits; deactivate
	// joins on it (bounded) so no in-flight loop iteration writes to the data
	// dir after teardown. Guarded by sessMu.
	loopDone chan struct{}

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

	// store is the daemon's single persistent ConfigDoc store (state.Load over
	// paths.Doc, doc.json, 0600). It is loaded once at boot and shared by setup,
	// adoption, the media ops and every read closure — there is exactly ONE store
	// per node so the genesis write, an adoption write and a gossip merge all land
	// in the same authoritative document (doc 07 §5). nil only in tests that build
	// a Node without OpenDataDir.
	store *state.Store

	// tls holds the switchable control-plane TLS material: a self-signed bootstrap
	// config before genesis, the cluster mTLS identity after (doc 03 §8). The
	// GetConfigForClient callback reads it atomically, so genesis/adoption switch
	// the served config WITHOUT rebinding the listener.
	tls tlsState

	// genesis holds the in-process cluster identity minted by the setup wizard
	// (POST /api/v1/setup, P1.3 §5.3) or loaded from disk at boot. It is read by the
	// Initialized / StatusView / VerifyAdminPassword / ConfigVersion web closures.
	// Guarded by genesisMu. nil until setup runs or a persisted ConfigDoc is loaded.
	genesisMu sync.Mutex
	genesis   *genesisState

	// leafKey is this node's persistent Ed25519 leaf private key (held in memory
	// for the TLS listener + CSR builds). Set by genesis or boot-load; nil before.
	leafKey crypto.Signer

	// bootGuard is the A.12 adoption brute-force guard + nonce store, shared by the
	// bootstrap adoptee surface (one per process). Built in New so the guard the
	// wizard never closes is the same one a controller throttles against.
	bootGuard *guardAdapter

	// bootstrapFinger is "sha256:<hex>" of this node's self-signed bootstrap cert
	// DER — the value a controller pins before adoption (doc 03 §2.2). Served in
	// GET /bootstrap/info. "" if self-signed TLS could not be built.
	bootstrapFinger string

	// startedAt is the process start time, for the StatusView uptime field.
	startedAt time.Time

	// webChanged is the coalesced state-change signal for the web layer's
	// websocket hub (Deps.Changed → immediate snapshot push). The role loop
	// signals it on every store/membership change; the store's own Changed
	// channel cannot be shared (single coalesced channel, the loop consumes it).
	webChanged chan struct{}

	// disc is the live mDNS advertisement (always-on once Run registers it,
	// re-registered with the cluster identity on Configure/forget). nil when
	// UseMDNS is off or registration failed. Guarded by discMu.
	discMu sync.Mutex
	disc   *discovery.Discovery

	// discovered is the cached BrowseAll survey for the web Discovery dep
	// (refreshed by browseLoop on the 5s rebrowse ticker, 02 §2.3 — never
	// browse per request). nil/empty when UseMDNS is off.
	discovered atomic.Pointer[[]web.Discovered]
}

// genesisState is the live cluster identity produced by the setup wizard or
// loaded from disk at boot. Its store field points at the daemon's single shared
// persistent store (n.store), so the genesis ConfigDoc is the same authoritative
// document setup, adoption, the media ops and gossip all act on.
type genesisState struct {
	store       *state.Store // shared persistent ConfigDoc store (= n.store)
	caFinger    string       // CA cert SHA-256 fingerprint (hex)
	clusterName string
	createdRFC  string // RFC3339 creation timestamp
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
		startedAt:  time.Now(),
		webChanged: make(chan struct{}, 1),
	}
	n.activateHook = n.activate
	n.persistHook = n.persistCluster

	// One persistent store per node (doc 07 §5): doc.json under the data dir, 0600
	// (it carries ClusterSecrets). An empty Doc path (tests without OpenDataDir)
	// yields a non-persistent store — still a real Store, just no disk.
	n.store = state.Load(opts.NodeID, opts.Paths.Doc)

	// The A.12 adoption guard + nonce store, shared by the bootstrap surface.
	n.bootGuard = newGuardAdapter(auth.NewAdoptionGuard(), nil)

	// Bootstrap self-signed TLS for the control plane (replaced atomically by the
	// cluster mTLS config on genesis/adoption; still presented to browsers after,
	// since they reject the Ed25519 cluster leaf). PERSISTED under certs/ so the
	// cert — and the operator's one-time browser exception — survives restarts.
	// Best-effort: a key-gen failure leaves the listener bare (web.Serve serves it
	// without TLS) rather than failing construction — the wizard is still
	// reachable, just over plain HTTP.
	if cfg, cert, der, err := loadOrCreateBrowserTLS(opts.Paths, opts.NodeID); err == nil {
		n.tls.selfSigned.Store(cfg)
		bc := cert
		n.tls.browserCert.Store(&bc)
		n.bootstrapFinger = "sha256:" + pki.Fingerprint(der)
	} else {
		logf(opts.Log, "self-signed bootstrap TLS unavailable (serving plain HTTP): %v", err)
	}

	// If the node is already configured on disk, restore the live cluster identity
	// (mTLS + adoption signer) and the genesis state so initialized() is true after
	// a restart WITHOUT re-running setup (GAP 2 restart-survival).
	n.loadPersistedCluster()
	return n
}

// loadPersistedCluster restores the cluster identity at boot when certs/ +
// doc.json + cluster.yaml are present. It is a no-op (clean idle) when any piece
// is missing, so a fresh node boots into the wizard. Failures are logged and
// non-fatal: a corrupt cert set leaves the node uninitialized rather than
// crashing (the operator can re-run setup / re-adopt).
func (n *Node) loadPersistedCluster() {
	p := n.options.Paths
	if p.Cluster == "" || !fileExists(p.Cluster) || !fileExists(p.NodeCert) || !fileExists(p.NodeKey) {
		return
	}
	doc := n.store.Get()
	if doc.Version == 0 {
		// cluster.yaml present but no doc.json: inconsistent on-disk state. Treat as
		// unconfigured rather than serving a node with no CA/auth.
		logf(n.options.Log, "boot: cluster.yaml present but doc.json empty — staying unconfigured")
		return
	}
	ci, err := loadClusterIdentity(p, doc, n.revokedPredicate(), n.tls.browserCert.Load())
	if err != nil {
		logf(n.options.Log, "boot: could not restore cluster identity (staying unconfigured): %v", err)
		return
	}
	n.leafKey = ci.leaf.PrivateKey.(crypto.Signer)
	n.tls.cluster.Store(ci)
	n.genesisMu.Lock()
	n.genesis = &genesisState{
		store:       n.store,
		caFinger:    doc.Cluster.Fingerprint,
		clusterName: doc.Cluster.Name,
		createdRFC:  doc.Cluster.Created,
	}
	n.genesisMu.Unlock()
	logf(n.options.Log, "boot: restored cluster %q version=%d (mTLS active, no re-setup)",
		doc.Cluster.Name, doc.Version)
}

// revokedPredicate returns a live closure over the store's RevokedSet for the
// PeerVerifier (a gossiped revocation takes effect without a restart, doc 03 §8).
func (n *Node) revokedPredicate() func(fingerprint string) bool {
	return func(fp string) bool {
		doc := n.store.Get()
		for _, e := range doc.Revoked.Entries {
			if e.Fingerprint == fp {
				return true
			}
		}
		return false
	}
}

// tlsConfigFunc is the Deps.TLSConfig seam: it returns a tls.Config whose
// GetConfigForClient picks the live cluster mTLS config once configured, else the
// self-signed bootstrap config. Returning a single wrapper with the callback means
// web.Serve wraps the listener ONCE and the genesis/adoption switch needs no
// rebind (doc 03 §8 last paragraph). Returns nil only if no self-signed config was
// built (key-gen failure) AND the node is unconfigured — then Serve runs bare.
func (n *Node) tlsConfigFunc() *tls.Config {
	if n.tls.cluster.Load() == nil && n.tls.selfSigned.Load() == nil {
		return nil
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return n.tls.serverConfig(), nil
		},
	}
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
		// Always announce the ACTUAL bound port (not the requested base): when a
		// second instance shares the host, listenWeb takes the next free port and
		// the operator must see where the UI really landed (doc 01 §3.1/§5.3).
		if port != opts.WebPort {
			fmt.Printf("  web port %d busy — bound :%d instead\n", opts.WebPort, port)
		}
		logf(opts.Log, "web UI on :%d", port)
	}

	// Always-on mDNS register advertising this node's identity + ports (an
	// uninitialized node announces cf="" init=0 — the adoption hook, 02 §2.4).
	// Re-registered with the cluster identity on Configure/forget. The BrowseAll
	// survey loop feeds the web Discovery dep on the 5s rebrowse cadence.
	n.registerMDNS()
	defer n.deregisterMDNS()
	if opts.UseMDNS {
		go n.browseLoop(ctx)
	}

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

	activeCtx, activeStop := context.WithCancel(n.rootCtx)

	// Stand the multi-node substrate up first (gossip membership + per-group
	// elections + allowlist + peer cache, plane.go) — BEST-EFFORT: a node with no
	// gossip port (or a bind failure) degrades to the solo doc-elected substrate
	// rather than failing activate (doc 01 §4.4).
	cp := newClusterPlane(n, group)
	cp.start(activeCtx)

	// Construct the realtime transport seam over the daemon's ONE persistent
	// store (so genesis/adoption/gossip writes flow straight into the engine) +
	// the group engine + hooks bound to stream/audio/clock, election-resolved
	// master addressing and the allowlist gates. Best-effort: a build failure
	// leaves tx nil so the lifecycle still runs and the media/status closures
	// degrade to not-ready rather than failing activate.
	tx := n.buildTransport(group, cp)

	loopDone := make(chan struct{})

	n.sessMu.Lock()
	n.activeCtx = activeCtx
	n.activeStop = activeStop
	n.activeGroup = group
	n.active = true
	n.tx = tx
	n.plane = cp
	n.loopDone = loopDone
	n.sessMu.Unlock()

	n.setRole("starting")
	logf(n.options.Log, "active group=%q", group)

	go func() {
		defer close(loopDone)
		_ = n.loop(activeCtx)
	}()
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
	// Snapshot the realtime handles to close them outside the lock.
	mem := n.mem
	clockSrv := n.clockSrv
	engine := n.engine
	tx := n.tx
	cp := n.plane
	loopDone := n.loopDone
	n.active = false
	n.activeGroup = ""
	n.mem = nil
	n.clockSrv = nil
	n.engine = nil
	n.tx = nil
	n.plane = nil
	n.activeCtx = nil
	n.activeStop = nil
	n.loopDone = nil
	n.sessMu.Unlock()

	if stop != nil {
		stop()
	}
	// Join the role loop (bounded) so no in-flight iteration keeps writing
	// (doc.json/peers.json) after teardown — e.g. into a directory a test (or a
	// release) is about to remove.
	if loopDone != nil {
		select {
		case <-loopDone:
		case <-time.After(3 * time.Second):
			logf(n.options.Log, "deactivate: role loop did not exit within 3s (continuing)")
		}
	}
	// Teardown ordering: stop the loop (above) → shut the group engine's
	// subsystems down (closes the clock-server/receiver sockets the role ctx
	// cancel alone does not) → leave membership, so a superseded session never
	// emits on the planes.
	if tx != nil && tx.roleEngine != nil && tx.roleEngine.engine != nil {
		tx.roleEngine.engine.Shutdown()
	}
	if cp != nil {
		cp.close()
	}
	closeIfCloser(mem)
	closeIfCloser(clockSrv)
	closeIfCloser(engine)
	n.setRole("idle")
}

// forget deactivates the session and wipes the cluster state, returning the
// node to UNCONFIGURED (doc 01 §4.4): /cluster/leave and the takeover release
// both land here. It is safe to call when already unconfigured.
func (n *Node) forget() error {
	n.releaseCluster()
	logf(n.options.Log, "forgotten — now unconfigured")
	return nil
}

// releaseCluster is the full self-release (03 §4 takeover atomicity / 01 §4.4
// forget): deactivate the session, wipe the on-disk cluster state (cluster.yaml
// marker + node/CA certs + doc.json + peers.json — the departing node must not
// retain the old cluster's secrets or seeds), reset the in-memory store to the
// zero doc (a version-0 doc loses every gossip merge, so a stale replica cannot
// re-adopt us), drop the live mTLS identity (the control plane atomically falls
// back to the persistent self-signed browser cert), and re-announce the
// unconfigured identity over mDNS. The node identity (node.json) and the
// browser cert survive — only cluster membership is erased.
func (n *Node) releaseCluster() {
	n.deactivate()

	p := n.options.Paths
	for _, f := range []string{p.Cluster, p.NodeCert, p.NodeKey, p.CACert} {
		if f != "" {
			_ = os.Remove(f)
		}
	}
	if p.Peers != "" {
		cluster.LoadPeerStore(p.Peers).Clear()
	}
	n.store.Reset() // zero doc + doc.json removed

	n.genesisMu.Lock()
	n.genesis = nil
	// n.leafKey is kept: the bootstrap adopt.Node was built over it at New, and
	// reusing the keypair for the next cluster's CSR is sound.
	n.genesisMu.Unlock()
	n.tls.cluster.Store(nil) // next handshake serves the self-signed bootstrap config

	n.registerMDNS()
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

// loop is the per-session role loop (Appendix A.14.4). On each membership/store
// signal (and the 2s safety tick, 02 §3.5) it re-resolves the per-group election
// (plane.electNow), drives the generation-fenced role switch (roleState.apply,
// which cancels the prior role ctx before starting the next), and re-Applies the
// group engine — whose reconcile is idempotent, so re-Applying on every tick is
// the designed way media/clock-health/membership changes converge the running
// subsystems (orphan→follower once sync lands, origin start once media selects).
func (n *Node) loop(ctx context.Context) error {
	n.sessMu.Lock()
	cp := n.plane
	active := n.active
	n.sessMu.Unlock()
	if !active {
		return nil
	}

	// roleState is the loop's fenced role-switch driver. It is split out (and
	// exported to the test via applyRole below) so the control flow can be unit-
	// tested with a fake membership sequence (doc P0.3 §7 T5).
	rs := &roleState{node: n, ctx: ctx, self: n.options.NodeID}
	defer rs.cancel()

	reapply := func() {
		master, gen := n.electNow(cp)
		rs.apply(master, gen)
		// Anchor incumbency in the agreed doc (plane.go claimMasterHint): the
		// elected master pins itself as the group's MasterHint so a lower-id
		// joiner cannot steal the role mid-stream.
		n.claimMasterHint(cp, master)
		// Re-Apply the engine on EVERY signal (idempotent reconcile): role changes
		// are handled inside rs.apply via runMaster/runFollower; unchanged-role
		// ticks still need the engine to observe doc/clock-health updates.
		if master != "" {
			n.applyRoleEngine(rs.ctxNow(), master, gen)
		}
	}
	reapply()

	// Keep our own record's addrs + probed playback devices current from the
	// start and then on a slow cadence (a node reappearing on a new IP — or with
	// a USB card plugged in — updates its record and gossips it).
	n.syncSelfAddrs(nonLoopbackIPStrings())
	n.syncSelfDevices(probedAudioDevices())
	n.syncSelfRender(n.sinkUsable())

	safetyTick := time.NewTicker(2 * time.Second)
	defer safetyTick.Stop()
	addrTick := time.NewTicker(30 * time.Second)
	defer addrTick.Stop()
	// With -v, log a playback/stream/clock status line every 5s so a silent
	// session is diagnosable (role, media, origin gen/listeners, ring fills,
	// renderer sync/drift/underruns, clock offset).
	var statusCh <-chan time.Time
	if n.options.Log != nil {
		statusTick := time.NewTicker(5 * time.Second)
		defer statusTick.Stop()
		statusCh = statusTick.C
	}
	var memCh <-chan struct{}
	if cp != nil {
		memCh = cp.changed()
	}
	storeCh := n.store.Changed()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-statusCh:
			if hs := n.hooksFor(); hs != nil {
				logf(n.options.Log, "%s", hs.statusLine(n.status().Role, n.store.Get(), n.currentGroup()))
			}
		case <-addrTick.C:
			n.syncSelfAddrs(nonLoopbackIPStrings())
			n.syncSelfDevices(probedAudioDevices())
			n.syncSelfRender(n.sinkUsable())
		case <-safetyTick.C:
			reapply()
		case <-memCh:
			if cp != nil {
				cp.persistPeers()
				cp.kickSync()
			}
			reapply()
			n.signalWebChanged()
		case <-storeCh:
			if cp != nil {
				cp.kickSync() // converge the doc write to peers promptly (plane.go)
			}
			reapply()
			n.signalWebChanged()
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
	roleCtx    context.Context // the live fenced role ctx (nil before the first role)
	gen        uint64          // generation of the currently-applied role
	hasRole    bool
}

// ctxNow returns the live fenced role ctx for engine re-Applies between role
// changes, falling back to the loop ctx before the first role lands.
func (rs *roleState) ctxNow() context.Context {
	if rs.roleCtx != nil {
		return rs.roleCtx
	}
	return rs.ctx
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
		rs.roleCtx = nil
	}
	rs.gen = gen
	rs.hasRole = true

	if master == "" {
		rs.node.setRole("starting")
		return
	}

	rctx, rcancel := context.WithCancel(rs.ctx)
	rs.roleCancel = rcancel
	rs.roleCtx = rctx
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

// registerMDNS (re)advertises this node over mDNS: id/name/cluster-fingerprint/
// group + the control/clock/audio/web ports in TXT, the gossip port as the
// SRV/A-record port peers feed to memberlist.Join (02 §2.2). An uninitialized
// node announces cf="" init=0 (the adoption hook, 02 §2.4). Re-registration
// (Configure/forget) closes the old advertisement first so the TXT reflects the
// new identity. No-op when UseMDNS is off (every test path).
func (n *Node) registerMDNS() {
	if !n.options.UseMDNS {
		return
	}
	n.discMu.Lock()
	defer n.discMu.Unlock()
	if n.disc != nil {
		n.disc.Close()
		n.disc = nil
	}
	doc := n.store.Get()
	group := "default"
	// Advertise the ACTUAL session ports when a plane is live (several instances
	// may share a host, so the canonical ports auto-increment); fall back to the
	// configured bases before activate.
	clockPort := resolvedPort(n.options.ClockPort, defaultClockPort)
	audioPort := resolvedPort(n.options.AudioPort, defaultAudioPort)
	gossipPort := n.options.BindPort
	n.sessMu.Lock()
	if n.activeGroup != "" {
		group = n.activeGroup
	}
	if n.plane != nil {
		clockPort = n.plane.clockPort
		audioPort = n.plane.audioPort
		if n.plane.gossipPort > 0 {
			gossipPort = n.plane.gossipPort
		}
	}
	n.sessMu.Unlock()
	d, err := discovery.Register(discovery.Announce{
		NodeID:      n.options.NodeID,
		Name:        n.options.Name,
		ClusterFP:   doc.Cluster.Fingerprint,
		GroupID:     group,
		Initialized: doc.Cluster.Fingerprint != "",
		ControlPort: n.webPort,
		ClockPort:   clockPort,
		AudioPort:   audioPort,
		WebPort:     n.webPort,
	}, gossipPort)
	if err != nil {
		logf(n.options.Log, "mDNS register failed (discovery off): %v", err)
		return
	}
	n.disc = d
}

// deregisterMDNS withdraws the mDNS advertisement (process shutdown).
func (n *Node) deregisterMDNS() {
	n.discMu.Lock()
	defer n.discMu.Unlock()
	if n.disc != nil {
		n.disc.Close()
		n.disc = nil
	}
}

// browseLoop is the always-on 5s BrowseAll survey (02 §2.3) feeding the cached
// web Discovery dep: every advertised node NOT in our cluster, classified
// uninitialized (adopt) vs foreign (takeover). Same-cluster nodes are omitted —
// the members list owns those rows.
func (n *Node) browseLoop(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		bctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		nodes, err := discovery.BrowseAll(bctx)
		cancel()
		if err == nil {
			n.storeDiscovered(nodes)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// storeDiscovered projects a BrowseAll survey into the cached web view. Addrs
// carry the target's CONTROL endpoint as host:port (the advertised ctrl TXT):
// the adopt flow dials this verbatim, and a bare IP would make dialTarget fall
// back to the default control port — on a multi-node host that is a DIFFERENT
// node (usually ourselves), whose member-closed bootstrap then answers the
// adoption with a misleading "foreign cluster" refusal.
func (n *Node) storeDiscovered(nodes []discovery.DiscoveredNode) {
	doc := n.store.Get()
	ourFP := doc.Cluster.Fingerprint
	out := make([]web.Discovered, 0, len(nodes))
	for _, d := range nodes {
		if d.NodeID == n.options.NodeID {
			continue
		}
		if d.Initialized && d.ClusterFP != "" && d.ClusterFP == ourFP {
			continue // our own member; the members list owns it
		}
		// A node already in our ConfigDoc is a member regardless of what its
		// (possibly stale, pre-adoption) mDNS TXT still says — a freshly-adopted
		// node must never linger in the discovered list next to its member row.
		if nodeRecord(doc, d.NodeID) != nil {
			continue
		}
		st := "uninitialized"
		if d.Initialized && d.ClusterFP != "" {
			st = "foreign"
		}
		addrs := []string{}
		if d.Addr != "" {
			if d.ControlPort > 0 {
				addrs = []string{net.JoinHostPort(d.Addr, strconv.Itoa(d.ControlPort))}
			} else {
				addrs = []string{d.Addr}
			}
		}
		out = append(out, web.Discovered{
			NodeID:      d.NodeID,
			Name:        d.Name,
			Addrs:       addrs,
			Fingerprint: d.ClusterFP,
			State:       st,
		})
	}
	n.discovered.Store(&out)
}

// persistCluster writes cluster.yaml (0600) marking the node configured with its
// activation group + cluster name + CA fingerprint (doc 01 §5.2). Its mere
// presence is what flips the default configured() predicate at the next boot.
// Best-effort: a write error is logged but does not roll back an already-active
// session (the node IS configured in memory; the marker is a boot hint). certs/
// and doc.json are written by setup()/adoption directly (they are the secrets +
// authoritative config), so persistCluster only owns the marker.
func (n *Node) persistCluster() {
	p := n.options.Paths
	if p.Cluster == "" {
		return // no data dir (test Node) — nothing to persist
	}
	n.genesisMu.Lock()
	g := n.genesis
	n.genesisMu.Unlock()
	m := clusterMarker{Configured: true, Group: "default"}
	if g != nil {
		m.ClusterName = g.clusterName
		m.CAFingerprint = g.caFinger
		m.CreatedAt = g.createdRFC
	}
	if err := writeClusterMarker(p, m); err != nil {
		logf(n.options.Log, "persist cluster.yaml failed (node still active): %v", err)
	}
}

// signalWebChanged nudges the web layer's websocket hub (coalesced) so browsers
// get an immediate snapshot push instead of waiting for the 3 Hz tick.
func (n *Node) signalWebChanged() {
	select {
	case n.webChanged <- struct{}{}:
	default:
	}
}

// currentGroup returns the active session's group id ("default" when none).
func (n *Node) currentGroup() string {
	n.sessMu.Lock()
	defer n.sessMu.Unlock()
	if n.activeGroup != "" {
		return n.activeGroup
	}
	return "default"
}

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
