package web

// api_node.go is the 08 §D.2/§D.3 per-node surface the Node-detail screen
// (09 §6) drives:
//
//	GET   /api/v1/nodes/{id} -> {version, node: NodeDetailView}   (read, chain-only)
//	PATCH /api/v1/nodes/{id} -> {version, node: NodeDetailView}   (admin, If-Match)
//
// The GET joins the ConfigDoc record with cert/liveness/group facts via the
// Deps.NodeDetail closure (daemon-side, where doc + elections live); with no
// closure wired it degrades to the bare State() record. The PATCH validates the
// body shape here (channel enum, 400s) and delegates the optimistic write to
// Deps.SetNodeConfig (409 on a stale If-Match, 404 on an unknown id).

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/auth"
)

// registerNodeRoutes mounts the per-node detail endpoints onto the API sub-mux.
// Called from registerAPIRoutes. The read sits under the [0..3] chain; the
// mutation adds RequireAdminSession.
func (s *Server) registerNodeRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/v1/nodes/{id}", s.handleNodeDetail)
	api.Handle("PATCH /api/v1/nodes/{id}", auth.RequireAdminSession(http.HandlerFunc(s.handleNodePatch)))
}

// nodeDetailResponse is the §D.2/§D.3 body: {version, node}.
type nodeDetailResponse struct {
	Version uint64         `json:"version"`
	Node    NodeDetailView `json:"node"`
}

// resolveNodeDetail returns the §D.2 projection for id: the NodeDetail closure
// when wired, else the bare State() record. ok=false => unknown id.
func (s *Server) resolveNodeDetail(id string) (NodeDetailView, uint64, bool) {
	var version uint64
	if s.deps.State != nil {
		version = s.deps.State().Version
	}
	if s.deps.NodeDetail != nil {
		v, ok := s.deps.NodeDetail(id)
		return v, version, ok
	}
	if s.deps.State != nil {
		for _, n := range s.deps.State().Nodes {
			if n.ID == id {
				return NodeDetailView{NodeView: n}, version, true
			}
		}
	}
	return NodeDetailView{}, version, false
}

// handleNodeDetail serves GET /api/v1/nodes/{id} (08 §D.2).
func (s *Server) handleNodeDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v, version, ok := s.resolveNodeDetail(id)
	if !ok {
		writeErr(w, http.StatusNotFound, codeNotFound, "unknown node id")
		return
	}
	w.Header().Set("ETag", strconv.FormatUint(version, 10))
	writeJSON(w, nodeDetailResponse{Version: version, Node: v})
}

// handleNodePatch serves PATCH /api/v1/nodes/{id} (08 §D.3): a partial node
// config update (name / channel / hwDelayUs / gainDb) under If-Match.
func (s *Server) handleNodePatch(w http.ResponseWriter, r *http.Request) {
	if s.deps.SetNodeConfig == nil {
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "node config unavailable")
		return
	}
	id := r.PathValue("id")
	ifMatch, ok := parseIfMatch(w, r)
	if !ok {
		return
	}
	var patch NodePatch
	if err := json.NewDecoder(io.LimitReader(r.Body, bodyLimit)).Decode(&patch); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "malformed JSON body")
		return
	}
	if patch.Channel != nil {
		switch *patch.Channel {
		case "stereo", "left", "right":
		default:
			writeErr(w, http.StatusBadRequest, codeInvalidRequest,
				`channel must be one of "stereo", "left", "right"`)
			return
		}
	}
	if err := s.deps.SetNodeConfig(id, patch, ifMatch); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			writeErr(w, http.StatusNotFound, codeNotFound, "unknown node id")
		case errors.Is(err, ErrVersionConflict):
			writeErr(w, http.StatusConflict, codeVersionConflict, "config version conflict")
		default:
			writeErr(w, http.StatusInternalServerError, codeInternal, "node config update failed")
		}
		return
	}
	// Read the post-write projection back so the screen re-renders without a
	// second fetch.
	v, version, ok := s.resolveNodeDetail(id)
	if !ok {
		writeErr(w, http.StatusNotFound, codeNotFound, "unknown node id")
		return
	}
	setVersionHeader(w, version)
	writeJSON(w, nodeDetailResponse{Version: version, Node: v})
}
