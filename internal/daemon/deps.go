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

		// TLSConfig: P1 (pki) builds the mTLS server config. Nil here => Serve
		// serves the bare listener (dev/test before certs land — see web/serve.go).
		TLSConfig: nil,

		// --- read-mostly snapshots ---

		// State: P0.2/P2 own *state.Store; cmd adapts state.ConfigDoc -> ConfigView.
		// No Store exists yet, so this returns an empty ConfigView (the web layer
		// treats it as nil-safe). TODO(P2): bind to n.state.Snapshot() and project.
		State: func() web.ConfigView { return web.ConfigView{} },

		// Transcodes: stream/* status rows. TODO(P4). No rows yet.
		Transcodes: func() []web.TranscodeStatus { return nil },

		// Discovery: mDNS cache read (cheap, no synchronous browse). TODO(P2). No
		// rows yet.
		Discovery: func() []web.Discovered { return nil },

		// Status: this node's role/sync snapshot, flattened from daemon.Status into
		// the flat web.NodeStatus (web must not import daemon's Status type).
		Status: n.webStatus,

		// SetupStatus: the wizard gate (GET /api/v1/setup). Live from the node's
		// configured() predicate + identity.
		SetupStatus: n.setupStatus,

		// --- cluster mutations (owning pieces land in P2/P6) ---

		// Adopt: A.9 adoption handshake + ConfigDoc write (C.3/C.4). The controller
		// driver (adopt.Controller) and its closure are constructed in
		// cmd/ensemble/wire_adopt.go from a live state.Store + pki.CA + cluster
		// membership; the daemon skeleton has no live engine yet, so this stub
		// reports not-yet-wired. TODO(P2 wiring): bind via wireAdopt.
		Adopt: func(addr, fingerprint, pin, nodeID, name string, force bool) error { return errNotImplemented },
		// Forget: revoke a node's cert + drop it from the ConfigDoc. TODO(P2).
		Forget: func(nodeID string) error { return errNotImplemented },
		// Leave: coordinated self-forget (POST /cluster/leave) -> n.forget(). It is
		// wired to the live lifecycle hook because forget() already exists in P0.3.
		Leave: n.forget,

		// SetNodeConfig: node config patch (name/channel/hwDelay/gain). TODO(P6).
		SetNodeConfig: func(nodeID string, patch web.NodePatch) error { return errNotImplemented },

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
		Configured: n.configured(),
		Name:       n.options.Name,
		NodeID:     n.options.NodeID,
	}
}
