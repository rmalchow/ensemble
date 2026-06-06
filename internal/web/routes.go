package web

import (
	"net/http"
	"strings"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/auth"
)

// This file wires the 08 §B setup/auth surface onto the mux and composes the
// [0..4] auth chain (03 §7.4) in front of it. The chain itself lives in
// internal/auth (P1.2: auth.Chain / auth.Deps / auth.RequireAdminSession); web
// only supplies the per-node session store and the closures that read live trust
// state, keeping web's import of the engine to the function-value seam (01 §2).

// passwordPolicyMin is the pinned minimum admin-password length (resolves P1.3
// open question §9.5: 03 references a policy but pins no number — we adopt the
// 08 §B.1 worked example "correct horse battery staple" as the floor: >= 12
// characters). Used for both setup (B.1) and password change (B.3a).
const passwordPolicyMin = 12

// passwordPolicyMsg is the human message returned with a 400 invalid_request when
// the policy fails; kept in one place so setup and password-change agree.
const passwordPolicyMsg = "password must be at least 12 characters"

// validPassword enforces the 03 password policy (P1.3 §9.5 resolution): a
// non-empty passphrase of at least passwordPolicyMin runes. Length is counted in
// runes so a multi-byte passphrase is not under-counted.
func validPassword(pw string) bool {
	return len([]rune(pw)) >= passwordPolicyMin
}

// registerAPIRoutes mounts the 08 §B setup/auth endpoints under the [0..4] auth
// chain. It is called by registerRoutes (server.go) after the skeleton routes so
// the /api/v1/* handlers take precedence over the asset catch-all. The chain is:
//
//	[0] recoverLog  -> [1] uninitialized gate + [2] node + [3] human  (auth.Chain)
//	[4] RequireAdminSession  (per-endpoint, only on admin-only routes)
//
// Reads (status, session) and credential-minting endpoints (setup, login) sit
// directly under the chain; mutations and key management add RequireAdminSession.
func (s *Server) registerAPIRoutes() {
	// The chain's [1]/[3] exceptions: the pre-init probe + genesis, plus login
	// (mints a session, so it must bypass the [3] human-credential requirement).
	public := map[string]bool{
		"/api/v1/status":     true,
		"/api/v1/setup":      true,
		"/api/v1/auth/login": true,
	}
	d := auth.Deps{
		Initialized: s.deps.Initialized,
		Sessions:    s.sessions,
		VerifyKey:   s.verifyAPIKey,
		NodeAuth:    nodeAuthFromTLS,
		PublicPaths: public,
		// session is the SPA's auth-OPTIONAL boot probe (08 §B.4): anonymous gets
		// 200 {authenticated:false} (render the login form), a credentialed caller
		// gets its method reflected. The [1] uninitialized 503 gate still applies.
		ProbePaths: map[string]bool{"/api/v1/auth/session": true},
	}

	api := http.NewServeMux()

	// Pre-init reachable (03 §7.5).
	api.HandleFunc("GET /api/v1/status", s.handleStatus)
	api.HandleFunc("POST /api/v1/setup", s.handleSetup)

	// Auth (08 §B.2-B.7). The [0..3] chain (recover/log + uninitialized gate +
	// node + human) wraps the WHOLE sub-mux once below; here the admin-only
	// endpoints add ONLY the [4] RequireAdminSession authz wrapper so the chain is
	// not run twice. login/status/setup are whitelisted in the chain's PublicPaths
	// (they mint / pre-date credentials); session accepts any authenticated class.
	api.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	api.Handle("POST /api/v1/auth/logout", auth.RequireAdminSession(http.HandlerFunc(s.handleLogout)))
	api.Handle("POST /api/v1/auth/password", auth.RequireAdminSession(http.HandlerFunc(s.handlePassword)))
	api.HandleFunc("GET /api/v1/auth/session", s.handleSession)
	api.Handle("GET /api/v1/auth/keys", auth.RequireAdminSession(http.HandlerFunc(s.handleListKeys)))
	api.Handle("POST /api/v1/auth/keys", auth.RequireAdminSession(http.HandlerFunc(s.handleCreateKey)))
	api.Handle("DELETE /api/v1/auth/keys/{id}", auth.RequireAdminSession(http.HandlerFunc(s.handleDeleteKey)))

	// Cluster membership ops (08 §C.3–C.6): adopt / takeover / forget / leave.
	// Operator-initiated (admin-session), so they add RequireAdminSession on top of
	// the [0..3] chain like the other mutating endpoints.
	s.registerClusterRoutes(api)

	// Cluster READ endpoints the dashboard fires after login (cluster/info,
	// discovery, nodes, groups). Reads only — they sit under the [0..3] chain
	// (no admin-session requirement) like GET /status.
	s.registerClusterReadRoutes(api)

	// Per-node detail (08 §D.2 read + §D.3 admin PATCH) for the Node screen.
	s.registerNodeRoutes(api)

	// Media / transport (08 §F): GET /media + POST /groups/{id}/{media,play,stop}.
	// Calibration (08 §F2.1): POST /calibrate/play. Group status (08 §G.2): GET
	// /groups/{id}/status. The reads sit under the chain; the mutations and
	// calibrate add RequireAdminSession inside their register funcs.
	s.registerMediaRoutes(api)
	s.registerCalibrateRoutes(api)
	s.registerGroupStatusRoutes(api)

	// Mount the whole sub-mux under the chain on the main mux so [0] recover/log
	// and the [1] uninitialized gate wrap every /api/v1 request uniformly (any
	// unmatched /api/v1 path on an uninitialized node => 503 not_ready before it
	// can 404). The status/setup/login whitelist lives in PublicPaths above.
	s.mux.Handle("/api/v1/", auth.Chain(d)(api))
}

// verifyAPIKey adapts the web Deps key list into the auth.VerifyKey closure shape
// ([3] Bearer path). It returns ("",false) when keys are unavailable so the chain
// simply falls through to 401. The constant-time match over all records lives in
// auth.VerifyAPIKey.
func (s *Server) verifyAPIKey(plaintext string) (id string, ok bool) {
	if s.deps.ListAPIKeys == nil {
		return "", false
	}
	_, keys := s.deps.ListAPIKeys()
	return auth.VerifyAPIKey(plaintext, keys)
}

// nodeAuthFromTLS implements the [2] node path: a request carrying a verified
// peer certificate chained to the cluster CA authenticates as method:"node" with
// NodeID=CN. The chain/revoked-set is enforced upstream by pki.PeerVerifier at the
// TLS handshake (03 §5.4), so a cert that reaches here is already trusted; this
// only surfaces the verified leaf's CN. Browser requests carry no client cert
// (tls.VerifyClientCertIfGiven) so VerifiedChains is empty and it returns false,
// falling through to the [3] human path.
func nodeAuthFromTLS(r *http.Request) (nodeID string, ok bool) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		return "", false
	}
	leaf := r.TLS.VerifiedChains[0][0]
	cn := strings.TrimSpace(leaf.Subject.CommonName)
	if cn == "" {
		return "", false
	}
	return cn, true
}
