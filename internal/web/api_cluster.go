package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/auth"
)

// This file implements the controller-side cluster-membership endpoints (08 §C.3–
// C.6): adopt, takeover, forget, leave. They run INSIDE the mTLS / human-auth
// chain and reach the engine only through the Deps.{Adopt,Forget,Leave} closures
// (cmd binds them to adopt.Controller + pki + state + cluster — wire_adopt.go).
// Structure (the "controller drives the target over HTTP" pattern, the status
// passthrough, the If-Match enforcement) is adapted from media internal/web/
// api_cluster.go; the group/password/seed logic and add-by-ip are dropped for the
// PKI fingerprint/PIN/phase flow.
//
// The Adopt/Forget/Leave closures own the optimistic ConfigDoc write internally
// (they re-read the version and retry on a mid-handshake gossip bump, open
// question §9.5), so the handlers only enforce the presence of If-Match (412 if
// absent) and map the closure's sentinel errors to the locked status codes; the
// new version + affectedGroups are read back from Deps.State() after success.

// registerClusterRoutes mounts the §C.3–C.6 endpoints onto the API sub-mux behind
// RequireAdminSession (operator-initiated). It is called from registerAPIRoutes.
func (s *Server) registerClusterRoutes(api *http.ServeMux) {
	api.Handle("POST /api/v1/cluster/adopt", auth.RequireAdminSession(http.HandlerFunc(s.handleClusterAdopt)))
	api.Handle("POST /api/v1/cluster/takeover", auth.RequireAdminSession(http.HandlerFunc(s.handleClusterTakeover)))
	api.Handle("POST /api/v1/cluster/leave", auth.RequireAdminSession(http.HandlerFunc(s.handleClusterLeave)))
	api.Handle("POST /api/v1/nodes/{id}/forget", auth.RequireAdminSession(http.HandlerFunc(s.handleNodeForget)))
}

// adoptRequest is the POST /api/v1/cluster/{adopt,takeover} body (08 §C.3/§C.4).
type adoptRequest struct {
	NodeID      string `json:"nodeId"`
	Addr        string `json:"addr"`
	Fingerprint string `json:"fingerprint"`
	PIN         string `json:"pin"`
	Name        string `json:"name"`
	Password    string `json:"password"` // C.4 takeover release credential
	Force       bool   `json:"force"` // C.4 takeover; C.3 ignores it
}

// nodeResponse is the §C.3/§C.4 success body: {version, node:{…}}.
type nodeResponse struct {
	Version uint64   `json:"version"`
	Node    NodeView `json:"node"`
}

// handleClusterAdopt serves POST /api/v1/cluster/adopt (08 §C.3): controller-
// initiated adoption of a discovered uninitialized node. If-Match required. It
// runs Deps.Adopt(addr,fp,pin,id,name,force=false); a foreign target surfaces 403
// (use takeover), a fingerprint/epoch mismatch 422, an unreachable target 502.
func (s *Server) handleClusterAdopt(w http.ResponseWriter, r *http.Request) {
	s.adoptCommon(w, r, false)
}

// handleClusterTakeover serves POST /api/v1/cluster/takeover (08 §C.4): identical
// to adopt but forces re-issue over an existing/foreign membership (force=true).
// Still PIN-gated.
func (s *Server) handleClusterTakeover(w http.ResponseWriter, r *http.Request) {
	s.adoptCommon(w, r, true)
}

// adoptCommon is the shared adopt/takeover driver. force selects the takeover
// semantics (C.4) vs the foreign-aborting adopt (C.3).
func (s *Server) adoptCommon(w http.ResponseWriter, r *http.Request, force bool) {
	if s.deps.Adopt == nil {
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "adoption unavailable")
		return
	}
	if _, ok := parseIfMatch(w, r); !ok {
		return
	}
	var req adoptRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, bodyLimit)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "malformed JSON body")
		return
	}
	if req.Addr == "" || req.NodeID == "" {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "addr and nodeId are required")
		return
	}
	// Takeover forces past the foreign guard; the request's own force flag is
	// honoured for adopt only as an explicit opt-in (normally false).
	useForce := force || req.Force

	if err := s.deps.Adopt(req.Addr, req.Fingerprint, req.PIN, req.NodeID, req.Name, req.Password, useForce); err != nil {
		writeAdoptErr(w, err)
		return
	}
	// Read back the post-write version + the node's record projection.
	resp := nodeResponse{}
	if s.deps.State != nil {
		cfg := s.deps.State()
		resp.Version = cfg.Version
		for _, n := range cfg.Nodes {
			if n.ID == req.NodeID {
				resp.Node = n
				break
			}
		}
	}
	if resp.Node.ID == "" {
		// The handshake + write reported success but the node is not yet visible in
		// our snapshot; surface at least the id/name the operator asked for.
		resp.Node.ID = req.NodeID
		resp.Node.Name = req.Name
	}
	setVersionHeader(w, resp.Version)
	writeJSON(w, resp)
}

// forgetResponse is the §C.5 success body.
type forgetResponse struct {
	Version        uint64   `json:"version"`
	RemovedNodeID  string   `json:"removedNodeId"`
	AffectedGroups []string `json:"affectedGroups"`
}

// handleNodeForget serves POST /api/v1/nodes/{id}/forget (08 §C.5): revoke +
// drop. If-Match required. The single ConfigDoc write (RevokedSet add + drop
// NodeRecord + pull from groups) and the gossip rekey are the Deps.Forget closure
// (wire_adopt.go); the handler reads back the new version + affectedGroups.
func (s *Server) handleNodeForget(w http.ResponseWriter, r *http.Request) {
	if s.deps.Forget == nil {
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "forget unavailable")
		return
	}
	if _, ok := parseIfMatch(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "node id required")
		return
	}
	// Capture the group memberships the id was in BEFORE the write so the response
	// reports affectedGroups even though Forget returns only an error.
	affected := s.groupsContaining(id)

	if err := s.deps.Forget(id); err != nil {
		writeForgetLeaveErr(w, err)
		return
	}
	var version uint64
	if s.deps.State != nil {
		version = s.deps.State().Version
	}
	setVersionHeader(w, version)
	writeJSON(w, forgetResponse{Version: version, RemovedNodeID: id, AffectedGroups: affected})
}

// leaveResponse is the §C.6 success body.
type leaveResponse struct {
	Version        uint64   `json:"version"`
	LeftNodeID     string   `json:"leftNodeId"`
	Coordinated    bool     `json:"coordinated"`
	AffectedGroups []string `json:"affectedGroups"`
}

// handleClusterLeave serves POST /api/v1/cluster/leave (08 §C.6): coordinated
// self-forget. If-Match required for the coordinated path. Deps.Leave proxies the
// revoke+drop to a peer over mTLS then wipes local identity. An unreachable
// cluster (ErrUnreachable) is NOT a hard error: the node wiped locally, so the
// handler returns 200 with coordinated:false (08 §C.6 fallback).
func (s *Server) handleClusterLeave(w http.ResponseWriter, r *http.Request) {
	if s.deps.Leave == nil {
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "leave unavailable")
		return
	}
	if _, ok := parseIfMatch(w, r); !ok {
		return
	}
	self := s.deps.NodeID
	affected := s.groupsContaining(self)

	coordinated := true
	if err := s.deps.Leave(); err != nil {
		if errors.Is(err, ErrUnreachable) {
			// Local wipe happened, cluster not updated by this node: 200 fallback.
			coordinated = false
		} else {
			writeForgetLeaveErr(w, err)
			return
		}
	}
	var version uint64
	if coordinated && s.deps.State != nil {
		version = s.deps.State().Version
	}
	setVersionHeader(w, version)
	writeJSON(w, leaveResponse{Version: version, LeftNodeID: self, Coordinated: coordinated, AffectedGroups: affected})
}

// groupsContaining returns the ids of groups whose MemberNodeIDs include nodeID,
// read from the live ConfigDoc projection (Deps.State). Used to fill
// affectedGroups on forget/leave without a new mutation seam (§9.4).
func (s *Server) groupsContaining(nodeID string) []string {
	if s.deps.State == nil {
		return nil
	}
	var out []string
	for _, g := range s.deps.State().Groups {
		for _, m := range g.MemberNodeIDs {
			if m == nodeID {
				out = append(out, g.ID)
				break
			}
		}
	}
	return out
}

// writeAdoptErr maps a Deps.Adopt sentinel to its 08 §C.3 status. proxy_failed
// (502) is the catch-all for an unreachable/rejected target.
func writeAdoptErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrForeign):
		writeErr(w, http.StatusForbidden, codeForbidden, "target belongs to another cluster; use takeover")
	case errors.Is(err, ErrEpochMismatch):
		writeErr(w, http.StatusUnprocessableEntity, codeUnprocessable, "protocol epoch mismatch")
	case errors.Is(err, ErrFingerprintMismatch):
		writeErr(w, http.StatusUnprocessableEntity, codeUnprocessable, "fingerprint mismatch")
	case errors.Is(err, ErrVersionConflict):
		writeErr(w, http.StatusConflict, codeVersionConflict, "config version conflict")
	case errors.Is(err, ErrWrongPassword):
		writeErr(w, http.StatusUnauthorized, codeUnauthenticated, "wrong takeover password")
	case errors.Is(err, ErrUnreachable):
		writeErr(w, http.StatusBadGateway, codeProxyFailed, "target unreachable")
	default:
		// An unclassified failure (e.g. bad PIN to the target) surfaces as
		// proxy_failed per 08 §C.3 (the controller could not complete with the target).
		writeErr(w, http.StatusBadGateway, codeProxyFailed, "adopt failed: "+err.Error())
	}
}

// writeForgetLeaveErr maps a Deps.Forget / Deps.Leave sentinel to its 08 §C.5/§C.6
// status.
func writeForgetLeaveErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, codeNotFound, "node not found")
	case errors.Is(err, ErrLastNode):
		writeErr(w, http.StatusConflict, codeConflict, "cannot remove the last node or a sole playing master")
	case errors.Is(err, ErrVersionConflict):
		writeErr(w, http.StatusConflict, codeVersionConflict, "config version conflict")
	case errors.Is(err, ErrUnreachable):
		writeErr(w, http.StatusBadGateway, codeProxyFailed, "no peer reachable")
	default:
		writeErr(w, http.StatusInternalServerError, codeInternal, "operation failed")
	}
}
