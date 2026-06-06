package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/auth"
)

// This file implements the two endpoints that bracket a node's lifecycle: the
// init probe GET /api/v1/status (08 §G.1) the SPA reads to choose wizard-vs-app,
// and the genesis POST /api/v1/setup (08 §B.1) that mints the cluster on the very
// first node. Both are reachable on an UNINITIALIZED node — they are the only
// /api/v1 surface the [1] gate lets through pre-init (03 §7.5). The shape (a
// status/GET + setup/POST pair, the s.deps.X==nil guards, the LimitReader decode,
// the already-configured 409) is adopted from media internal/web/api_setup.go;
// the group-secret model is dropped for the clusterName/adminPassword/nodeName
// genesis body and the B.1 response.

// bodyLimit caps a request body to 64 KiB (media's io.LimitReader(r.Body, 1<<16)
// idiom): the setup/auth bodies are tiny, so this bounds memory on a Pico-class
// node against a hostile or buggy client without affecting legitimate requests.
const bodyLimit = 1 << 16

// initialized reports whether this node has a cluster, nil-safe: a nil
// Initialized closure means "not wired yet", treated as uninitialized so the
// gate and the setup guard stay safe-by-default.
func (s *Server) initialized() bool {
	return s.deps.Initialized != nil && s.deps.Initialized()
}

// handleStatus serves GET /api/v1/status (08 §G.1): the node's live runtime
// projection. It is read-only and reachable pre-init; Initialized=false steers
// the SPA to the Setup Wizard. When StatusView is unwired (early bring-up/tests)
// it still answers with the Initialized flag so the probe is always meaningful.
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	var v StatusView
	if s.deps.StatusView != nil {
		v = s.deps.StatusView()
	}
	// The gate's Initialized closure is authoritative for the probe bit even if a
	// partial StatusView left it false.
	if s.initialized() {
		v.Initialized = true
	}
	if v.NodeID == "" {
		v.NodeID = s.deps.NodeID
	}
	if v.ConfigVersion == 0 && s.deps.ConfigVersion != nil {
		v.ConfigVersion = s.deps.ConfigVersion()
	}
	w.Header().Set("ETag", strconv.FormatUint(v.ConfigVersion, 10))
	writeJSON(w, v)
}

// setupRequest is the POST /api/v1/setup body (08 §B.1): the founding cluster
// name, the admin password, and this node's friendly name.
type setupRequest struct {
	ClusterName   string `json:"clusterName"`
	AdminPassword string `json:"adminPassword"`
	NodeName      string `json:"nodeName"`
}

// setupResponse is the B.1 success body.
type setupResponse struct {
	Cluster struct {
		Name          string `json:"name"`
		CAFingerprint string `json:"caFingerprint"`
		Created       string `json:"created"`
	} `json:"cluster"`
	Node struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"node"`
	Version uint64 `json:"version"`
}

// handleSetup serves POST /api/v1/setup (08 §B.1): the once-only genesis act. It
// validates the body (empty clusterName => 422; weak/empty adminPassword => 400),
// refuses on an already-initialized node (409 conflict — re-keying is adoption,
// not setup), calls the cmd-built Setup closure (mint CA, hash pw, self-sign leaf,
// write ConfigDoc v1, activate), then logs the operator in immediately by issuing
// a session cookie and returns the B.1 body with ETag:1.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if s.deps.Setup == nil {
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "setup unavailable")
		return
	}
	// Once a cluster exists, genesis is closed (03 §2.6): 409.
	if s.initialized() {
		writeErr(w, http.StatusConflict, codeConflict, "node already initialized")
		return
	}

	var req setupRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, bodyLimit)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "malformed JSON body")
		return
	}
	// 422: empty cluster name is semantically invalid (08 §B.1 status map).
	if req.ClusterName == "" {
		writeErr(w, http.StatusUnprocessableEntity, codeUnprocessable, "clusterName is required")
		return
	}
	// 400: the admin password must satisfy the 03 password policy.
	if !validPassword(req.AdminPassword) {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, passwordPolicyMsg)
		return
	}

	res, err := s.deps.Setup(req.ClusterName, req.AdminPassword, req.NodeName)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, codeInternal, "setup failed: "+err.Error())
		return
	}

	// Log the operator in immediately (B.1): issue a session cookie. Genesis is a
	// human, interactive act, so a session (not a node/api-key) is the right class.
	if s.sessions != nil {
		if value := s.sessions.Issue(); value != "" {
			auth.SetSessionCookie(w, value)
		}
	}

	var body setupResponse
	body.Cluster.Name = res.ClusterName
	body.Cluster.CAFingerprint = res.CAFingerprint
	body.Cluster.Created = res.Created
	body.Node.ID = res.NodeID
	body.Node.Name = res.NodeName
	body.Version = res.Version

	w.Header().Set("ETag", strconv.FormatUint(res.Version, 10))
	writeJSON(w, body)
}
