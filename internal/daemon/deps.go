package daemon

import (
	"gitlab.rand0m.me/ruben/go/ensemble/internal/web"
)

// buildDeps assembles the web.Deps function-value seam (Appendix A.14.3) from a
// Node's constructed subsystems. It is the SOLE constructor of web.Deps in the
// codebase: web receives only function values + the identity/paths, never a
// group/stream/audio import, which is how the hard layering rule (doc 01 §2
// rule 1) is honoured. Each closure binds to a live subsystem where one exists
// and otherwise returns a zero value (read closures, which the web layer treats
// as "not wired yet") or errNotImplemented (mutating closures, which the owning
// piece replaces in a later phase).
func buildDeps(n *Node) web.Deps {
	return web.Deps{
		// Identity / paths (P0.1), supplied directly.
		NodeID: n.options.NodeID,
		Paths:  n.options.Paths,

		// TLSConfig: the switchable control-plane TLS seam (GAP 3). It returns a
		// wrapper whose GetConfigForClient serves the self-signed bootstrap config
		// while unconfigured and the cluster mTLS config once genesis/adoption runs —
		// so web.Serve wraps the listener ONCE and the switch needs no rebind (doc 03
		// §8). Nil only if no TLS material exists at all (then Serve runs bare).
		TLSConfig: n.tlsConfigFunc,

		// --- read-mostly snapshots ---

		// State: the live ConfigView projection of the daemon's persistent store,
		// REDACTING ClusterSecrets (configView omits CAKeyPEM/SharedSecret — they are
		// never served, doc 09 §2.8). So a restart / adoption shows the converged
		// node+group list to the UI.
		State: func() web.ConfigView { return configView(n.store.Get()) },

		// Transcodes: stream/* status rows. TODO(P4). No rows yet.
		Transcodes: func() []web.TranscodeStatus { return nil },

		// Discovery: the cached BrowseAll survey (refreshed by the 5s browseLoop —
		// never a synchronous browse per request, 02 §2.3). Empty when mDNS is off.
		Discovery: func() []web.Discovered {
			if p := n.discovered.Load(); p != nil {
				return *p
			}
			return nil
		},

		// ClusterInfo: the dashboard's cluster identity header (name, CA
		// fingerprint, created, node count, version), projected from the
		// persistent store (ClusterSecrets never touched). Empty cluster name on
		// an uninitialised node.
		ClusterInfo: n.clusterInfo,

		// Status: this node's role/sync snapshot, flattened from daemon.Status into
		// the flat web.NodeStatus (web must not import daemon's Status type).
		Status: n.webStatus,

		// SetupStatus: the wizard gate (GET /api/v1/setup). Live from the node's
		// configured() predicate + identity.
		SetupStatus: n.setupStatus,

		// --- P1.3 setup / auth / status (08 §B + §G.1) ---
		// The genesis act + the read closures it feeds. Implemented in setup.go
		// (the daemon may import pki/state/auth per 01 §2 rule 6); web reaches
		// them only through these function values.
		Initialized:         n.initialized,
		StatusView:          n.statusView,
		Setup:               n.setup,
		VerifyAdminPassword: n.verifyAdminPassword,
		ConfigVersion:       n.configVersion,

		// --- cluster mutations (owning pieces land in P2/P6) ---

		// Adopt: A.9 adoption handshake + ConfigDoc write (C.3/C.4). Wired to the
		// daemon-side controller (adopt.go) over the persistent store + this node's
		// CA. When the node holds no CA (unconfigured / adoptee), the closure surfaces
		// not-ready so a node can only adopt once it is itself a cluster member.
		Adopt: n.adoptDep,
		// Forget: revoke a node's cert + drop it from the ConfigDoc (C.5). Wired to
		// the daemon-side forget over the persistent store + grow-only RevokedSet.
		Forget: n.forgetNodeDep,
		// Leave: coordinated self-forget (POST /cluster/leave) -> n.forget(). It is
		// wired to the live lifecycle hook because forget() already exists in P0.3.
		Leave: n.forget,

		// Members: the C.2 members rows (doc nodes ⋈ gossip liveness + live
		// control endpoints) for the cluster screen.
		Members: n.membersView,

		// Changed: the coalesced state-change signal driving the websocket hub's
		// immediate snapshot push (store writes + membership changes).
		Changed: func() <-chan struct{} { return n.webChanged },

		// NodeDetail / SetNodeConfig: the §D.2/§D.3 per-node surface (node_config.go):
		// the joined detail projection and the optimistic config patch (the owning
		// node's renderer picks the new lane off the gossiped doc).
		NodeDetail:    n.nodeDetail,
		SetNodeConfig: n.setNodeConfig,

		// --- media / transport (08 §F) + calibrate + status (P4.9) ---
		// Bound to the daemon-side transport ops (media.go). When no live session /
		// state store exists yet (P0.3 skeleton, or before activate), the ops are
		// nil-safe: the read closures return zero values and the mutating closures
		// surface ErrNotReady-class errors mapped by the handlers.
		ListMedia:     n.listMedia,
		SelectMedia:   n.selectMediaDep,
		Play:          n.playDep,
		Stop:          n.stopDep,
		GroupStatus:   n.groupStatus,
		CalibratePlay: n.calibratePlay,

		// Logf: the web layer's verbose log sink, bound to the daemon's logf so
		// bootstrap/adoption/setup lines land in the same stream as the engine
		// logs (nil-safe in web when Log is nil — verbose off).
		Logf: func(format string, args ...any) { logf(n.options.Log, "web: "+format, args...) },

		// Bootstrap: the node-side /bootstrap/* adoptee seam (08 §A). Fully wired now
		// (GAP 2): the adopt.Node half over this node's leaf key + PIN + the shared
		// A.12 guard, the CSR builder (pki.NewCSR), and the Install hook that
		// atomically persists the verified leaf+CA+secrets and switches this node to
		// mTLS. Info closes the surface (403) once this node is a member.
		Bootstrap: n.bootstrapDeps(),
	}
}

// bootstrapInfo is the live GET /bootstrap/info projection (08 §A.1): this
// node's id/name, init state, software + protocol versions, and (once pki
// lands) its self-signed cert fingerprint. In the P0.3 skeleton the fingerprint
// is empty (no cert yet) and Caps is the zero value; both fill in with P1/P2.
// State is "member" once configured (which closes the bootstrap surface, 403)
// and "uninitialized" otherwise.
func (n *Node) bootstrapInfo() web.BootstrapInfo {
	memberState := "uninitialized"
	if n.initialized() {
		memberState = "member"
	}
	ver := n.options.Version
	if ver == "" {
		ver = "dev"
	}
	return web.BootstrapInfo{
		NodeID:          n.options.NodeID,
		Name:            n.options.Name,
		Fingerprint:     n.bootstrapFinger, // sha256:<hex> of the self-signed cert to pin
		State:           memberState,
		SoftwareVersion: ver,
		// ProtocolEpoch left 0 here; the web handler defaults it to
		// adopt.ProtocolEpoch so daemon need not import adopt.
	}
}

// webStatus flattens daemon.Status into the flat web.NodeStatus (web must not
// import daemon's Status type, so the Offset time.Duration becomes OffsetMs).
func (n *Node) webStatus() web.NodeStatus {
	st := n.status()
	return web.NodeStatus{
		Role:     st.Role,
		MasterID: st.MasterID,
		Members:  st.Members,
		OffsetMs: st.Offset.Milliseconds(),
		HaveSync: st.HaveSync,
	}
}

// setupStatus backs GET /api/v1/setup: whether this node has joined a cluster
// plus its identity, so the frontend chooses the wizard vs. the app.
func (n *Node) setupStatus() web.SetupStatus {
	return web.SetupStatus{
		Configured: n.initialized(),
		Name:       n.options.Name,
		NodeID:     n.options.NodeID,
	}
}
