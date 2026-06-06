package web

import (
	"errors"
	"net/http"
)

// This file implements G.2 GET /api/v1/groups/{id}/status (08 §G.2): the live,
// non-replicated per-group telemetry. It is a fan-out read — the receiving node
// aggregates each member's own sync state (offsetUs/driftRatio/underruns/
// syncErrorUs) over mTLS; the master is authoritative for masterNodeId/profile/
// streamGen/playing. Read-only (no If-Match). The aggregation lives behind
// Deps.GroupStatus so web never imports group/audio (doc 01 §2 rule 1).
//
// Status mapping (08 §G.2): a group not yet synced/playing => 503 not_ready; a
// single member unreachable is reported per-member (Online=false), NOT a
// top-level error; an unreachable master => 502 proxy_failed; an unknown group
// => 404.

// registerGroupStatusRoutes mounts the §G.2 read. It sits behind the [0..3]
// chain (any authenticated class) like the other reads — no admin gate.
func (s *Server) registerGroupStatusRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/v1/groups/{id}/status", s.handleGroupStatus)
}

// handleGroupStatus serves GET /api/v1/groups/{id}/status (08 §G.2).
func (s *Server) handleGroupStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.GroupStatus == nil {
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "group status unavailable")
		return
	}
	groupID := r.PathValue("id")
	if groupID == "" {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "group id required")
		return
	}
	st, err := s.deps.GroupStatus(groupID)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotMember):
			writeErr(w, http.StatusNotFound, codeNotFound, "group not found")
		case errors.Is(err, ErrGroupNotReady):
			writeErr(w, http.StatusServiceUnavailable, codeNotReady, "group not ready")
		case errors.Is(err, ErrUnreachable):
			writeErr(w, http.StatusBadGateway, codeProxyFailed, "group master unreachable")
		default:
			writeErr(w, http.StatusInternalServerError, codeInternal, "group status failed")
		}
		return
	}
	if st.Members == nil {
		st.Members = []MemberStatus{}
	}
	if st.GroupID == "" {
		st.GroupID = groupID
	}
	writeJSON(w, st)
}
