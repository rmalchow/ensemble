package web

import (
	"crypto/tls"
	"errors"
	"net/netip"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/adopt"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/config"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// Sentinel errors the auth-mutation closures (ChangeAdminPassword / CreateAPIKey
// / DeleteAPIKey) return so the handlers can map a cmd-side failure to the right
// HTTP status (08 §B) without web knowing the engine internals. cmd wraps these
// (errors.Is-matchable). Any other (unexpected) error maps to 500 internal.
var (
	// ErrVersionConflict => 409 version_conflict (If-Match did not match).
	ErrVersionConflict = errors.New("web: config version conflict")
	// ErrWrongPassword => 401 unauthenticated (currentPassword mismatch, B.3a).
	ErrWrongPassword = errors.New("web: wrong current password")
	// ErrWeakPassword => 400 invalid_request (newPassword fails policy, B.3a).
	ErrWeakPassword = errors.New("web: password fails policy")
	// ErrKeyNotFound => 404 not_found (DELETE of an unknown key id, B.7).
	ErrKeyNotFound = errors.New("web: api key not found")

	// --- adoption / cluster-membership closure sentinels (08 §C.3–C.6) ---
	// The Adopt/Forget/Leave closures (cmd-bound, wire_adopt.go) return these so
	// the api_cluster handlers map a cmd-side failure to the right HTTP status
	// without web importing adopt/pki/state/cluster internals.

	// ErrForeign => 403 forbidden (adopt target belongs to another cluster; the
	// operator must use takeover, 08 §C.3).
	ErrForeign = errors.New("web: target belongs to another cluster")
	// ErrEpochMismatch => 422 unprocessable (adopt protocol-epoch mismatch — m7).
	ErrEpochMismatch = errors.New("web: protocol epoch mismatch")
	// ErrFingerprintMismatch => 422 unprocessable (target self-signed cert did not
	// match the pinned fingerprint, 08 §C.3).
	ErrFingerprintMismatch = errors.New("web: fingerprint mismatch")
	// ErrUnreachable => 502 proxy_failed (adopt/leave could not reach the target /
	// any peer, 08 §C.3/§C.6).
	ErrUnreachable = errors.New("web: target unreachable")
	// ErrNotFound => 404 not_found (forget of an unknown node id, 08 §C.5).
	ErrNotFound = errors.New("web: node not found")
	// ErrLastNode => 409 conflict (cannot forget/leave the last node or a sole
	// playing master, 08 §C.5/§C.6).
	ErrLastNode = errors.New("web: cannot remove last node or sole playing master")

	// --- media / transport closure sentinels (08 §F.2-F.4) ---
	// The SelectMedia/Play/Stop closures (cmd-bound, daemon/media.go) return these
	// so the api_media handlers map a cmd-side failure to the right HTTP status.

	// ErrNotMP3 => 422 unprocessable (the selected file is not a .mp3, D14 MVP).
	ErrNotMP3 = errors.New("web: media file is not an mp3")
	// ErrMissingOnMaster => 404 not_found (the file does not exist on the group's
	// master, where master-side decode happens, 08 §F.2).
	ErrMissingOnMaster = errors.New("web: media file not found on master")
	// ErrNoMedia => 409 conflict (play requested with no media selected, 08 §F.3).
	ErrNoMedia = errors.New("web: no media selected")
	// ErrNotMember => 404 not_found (the group id is unknown).
	ErrNotMember = errors.New("web: group not found")

	// ErrGroupNotReady => 503 not_ready (the group is not yet synced/playing, so
	// no live telemetry is available, 08 §G.2). The GroupStatus closure returns it.
	ErrGroupNotReady = errors.New("web: group not ready")
)

// Deps is the function-value seam between cmd and the web layer (A.14.3). The
// web package depends ONLY on this struct of closures plus its own view types;
// cmd supplies the implementations bound to cluster / group / state / pki. This
// keeps the dependency one-directional (no import cycle) and the server
// independently testable, and — critically — lets web honour the hard layering
// rule that it must never import group, stream/* or audio/* (doc 01 §2 rule 1):
// the engine is reached only through these closures, never a concrete type.
//
// Every closure is OPTIONAL. A nil field means "that capability is not wired
// yet" — the server is fully nil-safe so it runs with a partially-populated
// Deps during early bring-up and in tests (BuildSnapshot returns zero values,
// handlers degrade gracefully). The shape is forward-looking: the API-handler
// pieces fill behaviour in behind these same fields without changing the seam.
type Deps struct {
	// --- transport (net-new TLS seam, doc 01 §3.1) ---

	// TLSConfig returns the mTLS server config (built by pki in a later piece):
	// MinVersion TLS1.3, ClientAuth VerifyClientCertIfGiven, ClientCAs/RootCAs =
	// cluster CA, leaf = node cert. Serve wraps the pre-bound listener in it via
	// tls.NewListener. Nil => the listener is served bare (dev/test, before pki
	// lands), so the skeleton is runnable end-to-end without certs.
	TLSConfig func() *tls.Config

	// --- read-mostly snapshots (A.14.3) ---

	// State returns the current replicated ConfigDoc projected to a web-owned
	// ConfigView. It is a view type (not the literal state.ConfigDoc) so web
	// stays decoupled from state's concrete type — the same "surface but do not
	// import the producer" pattern mpvsync used for its transcode rows. cmd
	// adapts state.ConfigDoc -> ConfigView. Nil => empty ConfigView.
	State func() ConfigView
	// Transcodes returns per-group stream/transcode status snapshots. It is a
	// func over a flat web-owned view type rather than a concrete stream type,
	// because the producer lives in stream/* which web must not import. Nil =>
	// no rows.
	Transcodes func() []TranscodeStatus
	// Discovery returns the discovered-but-unadopted nodes (mDNS cache read). It
	// must be cheap (no synchronous browse). Nil => no rows.
	Discovery func() []Discovered
	// ClusterInfo returns the cluster's public identity header (name, CA
	// fingerprint, created, node count, config version) for GET
	// /api/v1/cluster/info. It NEVER carries ClusterSecrets. Nil => the handler
	// degrades to a zero/empty cluster (uninitialised node), so the dashboard
	// still renders.
	ClusterInfo func() ClusterInfoView

	// --- cluster mutations (proxied node->node over mTLS as needed) ---

	// Adopt runs the A.9 adoption handshake against a target and records the
	// resulting NodeRecord into the ConfigDoc (C.3/C.4). It pins the target's
	// self-signed cert by fingerprint, drives the three /bootstrap/adopt phases
	// (CA signing on this controller, Model B), and on success does the If-Match
	// config write → gossip. force=true allows a foreign target (takeover, 03 §4);
	// force=false surfaces web.ErrForeign on a foreign target. The canonical
	// signature is the C.3 request body (08 §C.3): addr, fingerprint, pin, the
	// assigned nodeId, an optional display name, and the takeover flag. Nil =>
	// adoption unavailable (503).
	Adopt func(addr, fingerprint, pin, nodeID, name, password string, force bool) error
	// Forget revokes a node's cert and drops it from the ConfigDoc. Nil =>
	// unavailable.
	Forget func(nodeID string) error
	// Leave performs a coordinated self-forget (POST /cluster/leave). Nil =>
	// unavailable.
	Leave func() error

	// --- node/group config ---

	// Members returns the cluster-members rows for the discovery snapshot
	// (C.2): each ConfigDoc node joined with gossip liveness + its live control
	// endpoint. Nil => the handler degrades to the bare State() records
	// (online unknown ⇒ false).
	Members func() []MemberView

	// NodeDetail returns the full §D.2 node projection (the record joined with
	// cert fingerprint, gossip liveness, group membership and mastership).
	// ok=false => unknown id (404). Nil => the handler degrades to the bare
	// State() record.
	NodeDetail func(nodeID string) (NodeDetailView, bool)

	// SetNodeConfig applies a node config patch (name / channel / hwDelayUs /
	// gain) to the replicated ConfigDoc under optimistic concurrency at ifMatch
	// (ErrVersionConflict on a stale version, ErrNotFound on an unknown id).
	// Nil => unavailable.
	SetNodeConfig func(nodeID string, patch NodePatch, ifMatch uint64) error

	// --- calibration ---

	// CalibratePlay plays the A.10b calibration signal synchronously on the
	// selected group or nodes for durationSec, fanning out to each selected node
	// over mTLS. It returns the nodes the signal actually played on (playedOn) and
	// per-node warnings (e.g. a Render=false node that cannot play, F2.1). A nil
	// error with a non-empty warnings slice is the normal partial-success case.
	// Nil => unavailable.
	CalibratePlay func(sel CalibrateSel, durationSec int) (playedOn []string, warnings []string, err error)

	// --- media / transport (08 §F) ---------------------------------------------
	//
	// The four media/transport closures own the local config write (state.Apply
	// under If-Match) → gossip → fan-out-to-master semantics (08 §F.2-F.4). Each
	// mutating closure returns the post-write ConfigView so the handler can emit
	// the new version + ETag (08 §0.5). They return the typed web sentinels below
	// (ErrVersionConflict / ErrNoMedia / ErrMissingOnMaster / ErrNotMP3 /
	// ErrUnreachable) so the handlers map a cmd-side failure to its locked status
	// code without web importing group/stream/state internals.

	// ListMedia lists one data/-relative folder of the given node's media tree
	// (F.1): the playable files (data/-relative slash paths) plus the folder's
	// subdirectories, so the media browser can descend. nodeID=="" means this
	// node; path=="" means the data/ root. Nil => no rows.
	ListMedia func(nodeID, path string) ([]MediaFile, []string, error)

	// SelectMedia writes GroupRecord.Media={file,loop} under If-Match, gossips, and
	// proxies the existence check to the group master (F.2). ifMatch is the caller's
	// If-Match version (the handler has already 412'd a missing header). Returns the
	// post-write ConfigView.
	// sourceNodeID is the node whose data/ holds the file: selecting it also
	// writes GroupRecord.MasterHint, so the SOURCE node is elected master and
	// decodes its own file locally (A.5 soft hint; "" leaves the hint alone).
	SelectMedia func(groupID, file string, loop bool, sourceNodeID string, ifMatch uint64) (ConfigView, error)

	// Play flips GroupRecord.Playing=true under If-Match, gossips, and fans the
	// start out to the master (F.3). A non-empty file (optional one-shot select+play)
	// first selects the media (sourceNodeID as in SelectMedia). Returns the
	// post-write ConfigView.
	Play func(groupID, file string, loop bool, sourceNodeID string, ifMatch uint64) (ConfigView, error)

	// Stop flips GroupRecord.Playing=false under If-Match, gossips, and fans the
	// stop out to the master (F.4). Returns the post-write ConfigView.
	Stop func(groupID string, ifMatch uint64) (ConfigView, error)

	// --- status (08 §G.2) ------------------------------------------------------

	// GroupStatus aggregates a group's live, non-replicated per-member telemetry
	// (fan-out read over mTLS). Nil => unavailable (503).
	GroupStatus func(groupID string) (GroupStatus, error)

	// --- liveness / UI plumbing (skeleton needs these; not in A.14.3 verbatim) ---

	// Changed signals coalesced state changes; a receive triggers an immediate
	// out-of-band websocket push so the UI updates faster than the 3 Hz tick.
	// Nil => only the periodic tick drives pushes.
	Changed func() <-chan struct{}
	// Status returns this node's role/sync snapshot. Nil => zero value.
	Status func() NodeStatus
	// SetupStatus backs the legacy wizard gate (P0.2). Nil => {Configured:false}.
	SetupStatus func() SetupStatus

	// --- P1.3 setup / auth surface (08 §B + §G.1) -------------------------------
	//
	// Every closure below is OPTIONAL and nil-safe: a nil field means "not wired
	// yet" and the handler degrades to a 503 not_ready (mutating ops) or a zero
	// projection (reads). cmd/daemon supplies the implementations bound to
	// pki+state per 01 §2 rule 6; web never imports those engines, only the
	// state value types (state.ConfigDoc/state.APIKey are data, not engine).

	// Initialized reports whether this node has a cluster (CA + admin hash). It
	// backs the [1] uninitialized gate and the POST /setup 409 guard, and is the
	// Initialized field of GET /api/v1/status. Nil => treated as uninitialized.
	Initialized func() bool

	// StatusView returns this node's live G.1 runtime projection (08 §G.1). It is
	// a web-owned view type (web must not import the engine status type). Nil =>
	// zero StatusView with Initialized filled from Initialized().
	StatusView func() StatusView

	// Setup performs the genesis act (POST /api/v1/setup, B.1): mint cluster CA,
	// argon2id-hash the admin password, self-sign this node's leaf, write the
	// genesis ConfigDoc (Version=1), and activate in-process. Returns the founding
	// identity + version. Implemented in cmd over pki+state+group (01 §2 rule 6).
	// Nil => setup unavailable (503).
	Setup func(clusterName, adminPassword, nodeName string) (SetupResult, error)

	// VerifyAdminPassword constant-time-checks pw against ConfigDoc.Auth.AdminHash
	// (B.2 login). Nil => login unavailable.
	VerifyAdminPassword func(pw string) bool

	// ConfigVersion returns the current ConfigDoc.Version for ETag / session
	// responses (B.4). Nil => 0.
	ConfigVersion func() uint64

	// ChangeAdminPassword writes a new admin-password hash to ConfigDoc.Auth under
	// optimistic concurrency (B.3a). It verifies current, validates next, applies
	// the If-Match'd write, bumps version, gossips, and returns the new version.
	// Nil => unavailable.
	ChangeAdminPassword func(ifMatch uint64, current, next string) (version uint64, err error)

	// ListAPIKeys returns the current version and the stored key metadata (B.5).
	// The secret is never present in a KeyRecord. Nil => (0, nil).
	ListAPIKeys func() (version uint64, keys []state.APIKey)

	// CreateAPIKey mints a key under optimistic concurrency, persists its hash to
	// ConfigDoc.Auth, and returns the new version, the key id, and the plaintext
	// secret (shown exactly once, B.6). Nil => unavailable.
	CreateAPIKey func(ifMatch uint64, label string) (version uint64, id, secret string, err error)

	// DeleteAPIKey drops a key hash from ConfigDoc.Auth under optimistic
	// concurrency (B.7). Returns the new version. Nil => unavailable.
	DeleteAPIKey func(ifMatch uint64, id string) (version uint64, err error)

	// --- bootstrap node-side surface (08 §A, OUTSIDE mTLS) ----------------------
	//
	// The /bootstrap/* handlers (bootstrap.go) run on the UNINITIALIZED node and
	// drive the A.9 adoptee half. They are reached through the Bootstrap seam so
	// web depends only on internal/adopt (a leaf engine package), never on pki /
	// state / cluster. Nil Bootstrap => the bootstrap surface is unavailable (503).
	Bootstrap *BootstrapDeps

	// Logf is the optional verbose log sink (bound to the daemon's logf). It lets
	// the web layer emit operational/audit lines (bootstrap probes, adoption
	// phases, setup) without importing the daemon. Nil => logging is discarded
	// (the logf helper below is fully nil-safe), so tests and the bare skeleton
	// run silently.
	Logf func(format string, args ...any)

	// NodeID is this node's stable id (P0.1 Identity), supplied by cmd.
	NodeID string
	// Paths are the node's resolved data-directory locations (P0.1).
	Paths config.Paths
}

// logf emits a line through the Deps.Logf sink if one is wired (nil-safe). It is
// the single logging entry point for the web layer so handlers never touch a
// concrete logger.
func (s *Server) logf(format string, args ...any) {
	if s.deps.Logf != nil {
		s.deps.Logf(format, args...)
	}
}

// BootstrapDeps is the node-side adoptee seam consumed by the /bootstrap/*
// handlers. cmd constructs the adopt.Node (bound to this node's leaf key + PIN +
// the auth.AdoptionGuard) and the Install hook (bound to state's atomic identity
// write + cluster join + mDNS flip); web only orchestrates the phases.
type BootstrapDeps struct {
	// Node is the A.9 adoptee half (per-handshake sessions, BeginKey/AcceptCSR/
	// Complete). It is the engine the handlers dispatch the three phases onto.
	Node *adopt.Node

	// Guard is the A.12 online brute-force guard (auth.AdoptionGuard via a cmd
	// adapter): Allow gates PIN-bearing phases; RecordFail/RecordSuccess drive
	// backoff/lockout. It is also the nonce store the Node was built over, so a
	// nonceA the Node minted is single-use and TTL'd here.
	Guard adopt.Throttle

	// CSR builds this node's CSR PEM for its leaf key (pki.NewCSR bound by cmd):
	// the engine stays decoupled from pki, so the handler supplies the CSR to
	// Node.AcceptCSR. Returns an error on a key/marshal failure (-> 500).
	CSR func() (csrPEM []byte, err error)

	// Install atomically persists the verified adoption result (leaf+CA+secrets,
	// 0600), joins gossip with the seed peers, flips the mDNS TXT init=1/cf, and
	// closes /bootstrap/* (P2.3/P2.1). It runs after Node.Complete authenticates +
	// decrypts. An error fails the complete phase (-> 500) and the node stays
	// uninitialized (takeover atomicity, 03 §4).
	Install func(inst adopt.Installed) error

	// Info returns the live GET /bootstrap/info projection (id, fingerprint, state,
	// epoch, caps). The handler closes the surface (403) when State == "member".
	Info func() BootstrapInfo

	// VerifyPassword checks a takeover-release password against THIS node's
	// current cluster admin credential (03 §4: the target cluster's operator
	// authorizes the release). Nil => takeover release unavailable (503).
	VerifyPassword func(pw string) bool

	// Release performs the full self-release (wipe cluster state, reopen
	// bootstrap) after a verified takeover password. Nil => unavailable.
	Release func() error
}

// srcAddr extracts the source IP (for the per-source guard) from an http request
// RemoteAddr. It is here because both bootstrap handlers need it.
func srcAddr(remoteAddr string) string {
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return ap.Addr().String()
	}
	if a, err := netip.ParseAddr(remoteAddr); err == nil {
		return a.String()
	}
	return remoteAddr
}

// retryAfterSeconds renders a backoff duration as an integer Retry-After header
// value (08 §0.4 rate_limited), rounding up so a sub-second backoff still reports 1.
func retryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	s := int(d / time.Second)
	if d%time.Second != 0 {
		s++
	}
	return s
}
