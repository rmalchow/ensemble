package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/auth"
)

// This file implements the 08 §F media/transport surface: F.1 GET /media (the
// playable-media list), F.2 POST /groups/{id}/media (select), F.3 POST
// /groups/{id}/play, and F.4 POST /groups/{id}/stop. The HTTP scaffold (JSON
// decode, LimitReader, If-Match enforcement, error→status mapping) follows the
// api_cluster.go pattern; the engine is reached only through the Deps closures
// (ListMedia/SelectMedia/Play/Stop), so web never imports group/stream/state
// (doc 01 §2 rule 1, enforced by imports_test.go).
//
// The mutating handlers (F.2/F.3/F.4) require If-Match (08 §0.5): a missing
// header is a 412 precondition_required (parseIfMatch), even for a pure play —
// it mutates GroupRecord.Playing. The closure owns the optimistic-concurrency
// write+gossip+fan-out and returns the post-write ConfigView so the handler can
// emit the new version + ETag.

// registerMediaRoutes mounts the §F endpoints. The list (F.1) is a read behind
// the [0..3] chain (any authenticated class); the mutations add
// RequireAdminSession on top, matching the cluster/auth mutations.
func (s *Server) registerMediaRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/v1/media", s.handleListMedia)
	api.Handle("POST /api/v1/groups/{id}/media", auth.RequireAdminSession(http.HandlerFunc(s.handleSelectMedia)))
	api.Handle("POST /api/v1/groups/{id}/play", auth.RequireAdminSession(http.HandlerFunc(s.handlePlay)))
	api.Handle("POST /api/v1/groups/{id}/stop", auth.RequireAdminSession(http.HandlerFunc(s.handleStop)))
}

// mediaListResponse is the F.1 success body: {node, path, dirs, files}.
type mediaListResponse struct {
	NodeID string      `json:"nodeId"`
	Path   string      `json:"path"` // the data/-relative folder listed ("" = root)
	Dirs   []string    `json:"dirs"` // subdirectories of Path (browse targets)
	Files  []MediaFile `json:"files"`
}

// handleListMedia serves GET /api/v1/media (08 §F.1): one folder of a node's
// data/ media tree — the playable files plus the subdirectories the browser can
// enter. Optional ?node=<id> (the SPA historically sent ?nodeId= — both are
// accepted) selects a peer (default this node); ?path=<rel> selects a subfolder
// (default the root; traversal-sanitized in the closure). Unknown closure => 503.
func (s *Server) handleListMedia(w http.ResponseWriter, r *http.Request) {
	if s.deps.ListMedia == nil {
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "media listing unavailable")
		return
	}
	q := r.URL.Query()
	node := q.Get("node")
	if node == "" {
		node = q.Get("nodeId")
	}
	path := q.Get("path")
	files, dirs, err := s.deps.ListMedia(node, path)
	if err != nil {
		writeMediaErr(w, err)
		return
	}
	if files == nil {
		files = []MediaFile{}
	}
	if dirs == nil {
		dirs = []string{}
	}
	resp := mediaListResponse{NodeID: node, Path: path, Dirs: dirs, Files: files}
	if resp.NodeID == "" {
		resp.NodeID = s.deps.NodeID
	}
	writeJSON(w, resp)
}

// mediaRequest is the F.2 POST /groups/{id}/media body (08 §F.2). loop defaults
// to false.
type mediaRequest struct {
	File string `json:"file"`
	Loop bool   `json:"loop"`
	// NodeID is the node whose data/ holds File (the browse scope): it becomes
	// the group's MasterHint so the source node masters and decodes locally.
	NodeID string `json:"nodeId"`
}

// playRequest is the F.3 POST /groups/{id}/play body (08 §F.3): an optional
// one-shot select-and-play. Empty File => resume the already-selected media.
type playRequest struct {
	File string `json:"file,omitempty"`
	Loop bool   `json:"loop,omitempty"`
	// NodeID is the file's source node (see mediaRequest.NodeID).
	NodeID string `json:"nodeId,omitempty"`
}

// groupOpResponse is the §F.2-F.4 success body: {version, group:{…}}.
type groupOpResponse struct {
	Version uint64    `json:"version"`
	Group   GroupView `json:"group"`
}

// handleSelectMedia serves POST /api/v1/groups/{id}/media (08 §F.2): write
// GroupRecord.Media={file,loop} under If-Match. A non-mp3 file => 422, a file
// absent on the master => 404, an unknown group => 404.
func (s *Server) handleSelectMedia(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelectMedia == nil {
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "media selection unavailable")
		return
	}
	groupID := r.PathValue("id")
	if groupID == "" {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "group id required")
		return
	}
	ifMatch, ok := parseIfMatch(w, r)
	if !ok {
		return
	}
	var req mediaRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.File == "" {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "file is required")
		return
	}
	cfg, err := s.deps.SelectMedia(groupID, req.File, req.Loop, req.NodeID, ifMatch)
	if err != nil {
		writeMediaErr(w, err)
		return
	}
	writeGroupOp(w, cfg, groupID)
}

// handlePlay serves POST /api/v1/groups/{id}/play (08 §F.3): flip
// GroupRecord.Playing=true under If-Match (+ optional one-shot select), gossip,
// fan out to master. Play with no media selected => 409 conflict; master
// unreachable => 502.
func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	if s.deps.Play == nil {
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "play unavailable")
		return
	}
	groupID := r.PathValue("id")
	if groupID == "" {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "group id required")
		return
	}
	ifMatch, ok := parseIfMatch(w, r)
	if !ok {
		return
	}
	var req playRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	cfg, err := s.deps.Play(groupID, req.File, req.Loop, req.NodeID, ifMatch)
	if err != nil {
		writeMediaErr(w, err)
		return
	}
	writeGroupOp(w, cfg, groupID)
}

// handleStop serves POST /api/v1/groups/{id}/stop (08 §F.4): flip
// GroupRecord.Playing=false under If-Match, gossip, fan out the stop to the
// master. Master unreachable => 502 (the config write still happened).
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if s.deps.Stop == nil {
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "stop unavailable")
		return
	}
	groupID := r.PathValue("id")
	if groupID == "" {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "group id required")
		return
	}
	ifMatch, ok := parseIfMatch(w, r)
	if !ok {
		return
	}
	cfg, err := s.deps.Stop(groupID, ifMatch)
	if err != nil {
		writeMediaErr(w, err)
		return
	}
	writeGroupOp(w, cfg, groupID)
}

// decodeBody decodes the request JSON body into v, capping it at bodyLimit. An
// empty body (io.EOF) is allowed (the caller validates required fields). It
// returns ok=false and writes the 400 itself on a malformed body.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) (ok bool) {
	if err := json.NewDecoder(io.LimitReader(r.Body, bodyLimit)).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "malformed JSON body")
		return false
	}
	return true
}

// writeGroupOp emits the §F.2-F.4 success envelope from the post-write ConfigView,
// setting the ETag to the new version and projecting the named group.
func writeGroupOp(w http.ResponseWriter, cfg ConfigView, groupID string) {
	resp := groupOpResponse{Version: cfg.Version}
	for _, g := range cfg.Groups {
		if g.ID == groupID {
			resp.Group = g
			break
		}
	}
	if resp.Group.ID == "" {
		resp.Group.ID = groupID
	}
	setVersionHeader(w, resp.Version)
	writeJSON(w, resp)
}

// writeMediaErr maps a media/transport closure sentinel to its 08 §F status.
func writeMediaErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotMP3):
		writeErr(w, http.StatusUnprocessableEntity, codeUnprocessable, "media file is not an mp3")
	case errors.Is(err, ErrMissingOnMaster):
		writeErr(w, http.StatusNotFound, codeNotFound, "media file not found on master")
	case errors.Is(err, ErrNotMember):
		writeErr(w, http.StatusNotFound, codeNotFound, "group not found")
	case errors.Is(err, ErrNoMedia):
		writeErr(w, http.StatusConflict, codeConflict, "no media selected")
	case errors.Is(err, ErrVersionConflict):
		writeErr(w, http.StatusConflict, codeVersionConflict, "config version conflict")
	case errors.Is(err, ErrUnreachable):
		writeErr(w, http.StatusBadGateway, codeProxyFailed, "group master unreachable")
	default:
		writeErr(w, http.StatusInternalServerError, codeInternal, "media operation failed")
	}
}
