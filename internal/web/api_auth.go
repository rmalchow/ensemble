package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/auth"
)

// This file implements the 08 §B.2-B.7 auth endpoints: login/logout/password/
// session and the API-key CRUD. They run behind the [0..4] chain wired in
// routes.go, so by the time a handler executes the request is already
// authenticated (and, for admin routes, authorized via RequireAdminSession).
// Config-mutating handlers parse and forward If-Match (08 §0.5) to the cmd
// closure, which owns the optimistic-concurrency write; the handlers map the
// closure's sentinel errors to the locked status codes.

// parseIfMatch reads and parses the If-Match header for a config-mutating call
// (08 §0.5). A missing header is a 412 precondition_required; a present but
// non-integer value is a 400 invalid_request. ok=false means a response was
// already written.
func parseIfMatch(w http.ResponseWriter, r *http.Request) (version uint64, ok bool) {
	raw := r.Header.Get("If-Match")
	if raw == "" {
		writeErr(w, http.StatusPreconditionRequired, codePreconditionReq, "If-Match header required")
		return 0, false
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "malformed If-Match header")
		return 0, false
	}
	return v, true
}

// setVersionHeader sets the ETag to the new ConfigDoc version after a successful
// mutating write (08 §0.5).
func setVersionHeader(w http.ResponseWriter, version uint64) {
	w.Header().Set("ETag", strconv.FormatUint(version, 10))
}

// loginRequest is the POST /api/v1/auth/login body (08 §B.2).
type loginRequest struct {
	Password string `json:"password"`
}

// handleLogin serves POST /api/v1/auth/login (08 §B.2): exchange the admin
// password for a session cookie. It is public-in-cluster but unreachable
// pre-init (503 not_ready). A wrong password is 401; on success it issues a
// session, sets the cookie, and returns the absolute expiry hint.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.initialized() {
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "node not initialized")
		return
	}
	if s.deps.VerifyAdminPassword == nil {
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "login unavailable")
		return
	}
	var req loginRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, bodyLimit)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "malformed JSON body")
		return
	}
	if !s.deps.VerifyAdminPassword(req.Password) {
		writeErr(w, http.StatusUnauthorized, codeUnauthenticated, "invalid password")
		return
	}
	value := s.sessions.Issue()
	if value == "" {
		writeErr(w, http.StatusInternalServerError, codeInternal, "could not issue session")
		return
	}
	auth.SetSessionCookie(w, value)
	// expiresAt is the absolute cap (7 d, 03 §7.2); the cookie's own MaxAge tracks
	// the 12 h sliding idle window separately.
	resp := struct {
		Session struct {
			ExpiresAt string `json:"expiresAt"`
		} `json:"session"`
	}{}
	resp.Session.ExpiresAt = time.Now().Add(auth.AbsoluteTTL).UTC().Format(time.RFC3339)
	writeJSON(w, resp)
}

// handleLogout serves POST /api/v1/auth/logout (08 §B.3): revoke the current
// session and clear the cookie. Admin-session only (enforced upstream). 204.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.SessionCookieName); err == nil {
		s.sessions.Revoke(c.Value)
	}
	auth.ClearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// passwordRequest is the POST /api/v1/auth/password body (08 §B.3a).
type passwordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// handlePassword serves POST /api/v1/auth/password (08 §B.3a): change the cluster
// admin password. If-Match required (412 absent / 409 mismatch); wrong current
// 401; weak new 400. On success it returns the new version + ETag. The actual
// verify+validate+write+gossip is the cmd closure under optimistic concurrency.
func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	if s.deps.ChangeAdminPassword == nil {
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "password change unavailable")
		return
	}
	ifMatch, ok := parseIfMatch(w, r)
	if !ok {
		return
	}
	var req passwordRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, bodyLimit)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "malformed JSON body")
		return
	}
	// Validate the new password locally too (defence-in-depth; the closure also
	// enforces it and may return ErrWeakPassword).
	if !validPassword(req.NewPassword) {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, passwordPolicyMsg)
		return
	}
	version, err := s.deps.ChangeAdminPassword(ifMatch, req.CurrentPassword, req.NewPassword)
	if err != nil {
		writeMutationErr(w, err)
		return
	}
	setVersionHeader(w, version)
	writeJSON(w, struct {
		Version uint64 `json:"version"`
	}{Version: version})
}

// sessionResponse is the GET /api/v1/auth/session body (08 §B.4).
type sessionResponse struct {
	Authenticated bool   `json:"authenticated"`
	Method        string `json:"method"`
	NodeID        string `json:"nodeId"`
	ConfigVersion uint64 `json:"configVersion"`
}

// handleSession serves GET /api/v1/auth/session (08 §B.4): "who am I". It is an
// auth-OPTIONAL probe (chain ProbePaths): an anonymous request gets 200
// {authenticated:false} — the SPA's signal to render the login form — leaking
// neither the config version nor the node id. A credentialed caller (session OR
// api-key OR node cert) gets its method reflected plus the current config
// version (so the SPA can seed If-Match) and ETag:<version>.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	method, ok := auth.MethodFromContext(r.Context())
	if !ok {
		writeJSON(w, sessionResponse{Authenticated: false})
		return
	}
	var version uint64
	if s.deps.ConfigVersion != nil {
		version = s.deps.ConfigVersion()
	}
	// For a node-cert caller the NodeID is the cert CN; otherwise it is this node.
	nodeID := s.deps.NodeID
	if id, ok := auth.NodeIDFromContext(r.Context()); ok && id != "" {
		nodeID = id
	}
	setVersionHeader(w, version)
	writeJSON(w, sessionResponse{
		Authenticated: true,
		Method:        string(method),
		NodeID:        nodeID,
		ConfigVersion: version,
	})
}

// keyMeta is the metadata-only projection of a stored API key (08 §B.5): never
// the secret, never the hash.
type keyMeta struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	CreatedAt  string `json:"createdAt"`
	LastUsedAt string `json:"lastUsedAt,omitempty"`
}

// handleListKeys serves GET /api/v1/auth/keys (08 §B.5): list key metadata.
// Admin-session only. Returns {version,keys[]} and ETag:<version>.
func (s *Server) handleListKeys(w http.ResponseWriter, _ *http.Request) {
	var version uint64
	var keys []keyMeta
	if s.deps.ListAPIKeys != nil {
		v, stored := s.deps.ListAPIKeys()
		version = v
		keys = make([]keyMeta, 0, len(stored))
		for _, k := range stored {
			keys = append(keys, keyMeta{
				ID:         k.ID,
				Label:      k.Name,
				CreatedAt:  k.Created,
				LastUsedAt: k.LastUsed,
			})
		}
	}
	setVersionHeader(w, version)
	writeJSON(w, struct {
		Version uint64    `json:"version"`
		Keys    []keyMeta `json:"keys"`
	}{Version: version, Keys: keys})
}

// createKeyRequest is the POST /api/v1/auth/keys body (08 §B.6).
type createKeyRequest struct {
	Label string `json:"label"`
}

// handleCreateKey serves POST /api/v1/auth/keys (08 §B.6): mint a key. Admin-
// session only, If-Match required. The plaintext secret is returned exactly once
// in the 201 body and never persisted (only its hash is stored cmd-side).
func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	if s.deps.CreateAPIKey == nil {
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "key creation unavailable")
		return
	}
	ifMatch, ok := parseIfMatch(w, r)
	if !ok {
		return
	}
	var req createKeyRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, bodyLimit)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "malformed JSON body")
		return
	}
	if req.Label == "" {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "label is required")
		return
	}
	version, id, secret, err := s.deps.CreateAPIKey(ifMatch, req.Label)
	if err != nil {
		writeMutationErr(w, err)
		return
	}
	setVersionHeader(w, version)
	resp := struct {
		Version uint64 `json:"version"`
		Key     struct {
			ID        string `json:"id"`
			Label     string `json:"label"`
			Secret    string `json:"secret"`
			CreatedAt string `json:"createdAt"`
		} `json:"key"`
	}{Version: version}
	resp.Key.ID = id
	resp.Key.Label = req.Label
	resp.Key.Secret = secret
	resp.Key.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	writeJSONStatus(w, http.StatusCreated, resp)
}

// handleDeleteKey serves DELETE /api/v1/auth/keys/{id} (08 §B.7): revoke a key.
// Admin-session only, If-Match required. Unknown id 404. Returns the new version.
func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	if s.deps.DeleteAPIKey == nil {
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "key deletion unavailable")
		return
	}
	ifMatch, ok := parseIfMatch(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "key id required")
		return
	}
	version, err := s.deps.DeleteAPIKey(ifMatch, id)
	if err != nil {
		writeMutationErr(w, err)
		return
	}
	setVersionHeader(w, version)
	writeJSON(w, struct {
		Version uint64 `json:"version"`
	}{Version: version})
}

// writeMutationErr maps a cmd-closure sentinel error to its locked HTTP status
// (08 §B). An unrecognised error is a 500 internal — the engine failed in a way
// the API contract does not enumerate.
func writeMutationErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrVersionConflict):
		writeErr(w, http.StatusConflict, codeVersionConflict, "config version conflict")
	case errors.Is(err, ErrWrongPassword):
		writeErr(w, http.StatusUnauthorized, codeUnauthenticated, "wrong current password")
	case errors.Is(err, ErrWeakPassword):
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, passwordPolicyMsg)
	case errors.Is(err, ErrKeyNotFound):
		writeErr(w, http.StatusNotFound, codeNotFound, "api key not found")
	default:
		writeErr(w, http.StatusInternalServerError, codeInternal, "operation failed")
	}
}
