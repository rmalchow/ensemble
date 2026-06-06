package web

import (
	"net/http"
	"strconv"
)

// This file implements the dashboard's cluster READ endpoints (the GETs the SPA
// fires after login/setup to render the cluster view): cluster/info, discovery,
// nodes, groups. They are reads, so they sit directly under the [0..3] auth chain
// (no RequireAdminSession) like GET /status, and reach the engine only through
// the Deps.{ClusterInfo,State,Discovery} closures. Every response sets ETag to
// the ConfigDoc version so the SPA's fetch wrapper can track staleness, and each
// body matches the shape the bundled frontend expects:
//
//	GET /api/v1/cluster/info -> ClusterInfoView         (spread + .version)
//	GET /api/v1/discovery    -> {members, discovered}
//	GET /api/v1/nodes        -> {version, nodes}
//	GET /api/v1/groups       -> {version, groups}
//
// All four are nil-safe: a closure that is not wired yet yields an empty (but
// well-formed) body, so the dashboard renders instead of erroring.

// registerClusterReadRoutes mounts the dashboard read endpoints onto the API
// sub-mux. Called from registerAPIRoutes. Reads only — no RequireAdminSession.
func (s *Server) registerClusterReadRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/v1/cluster/info", s.handleClusterInfo)
	api.HandleFunc("GET /api/v1/discovery", s.handleDiscovery)
	api.HandleFunc("GET /api/v1/nodes", s.handleListNodes)
	api.HandleFunc("GET /api/v1/groups", s.handleListGroups)
}

// clusterInfoResponse is the C.1 body shape the SPA consumes:
// {version, cluster:{name, caFingerprint, created}, counts:{nodes, groups}}.
type clusterInfoResponse struct {
	Version uint64 `json:"version"`
	Cluster struct {
		Name          string `json:"name"`
		CAFingerprint string `json:"caFingerprint"`
		Created       string `json:"created"`
	} `json:"cluster"`
	Counts struct {
		Nodes  int `json:"nodes"`
		Groups int `json:"groups"`
	} `json:"counts"`
}

// handleClusterInfo serves GET /api/v1/cluster/info: the cluster identity header
// the dashboard + cluster screens render. The body is the NESTED C.1 shape the
// SPA's getClusterInfo()/clusterInfoFull() decode (a flat body silently yields
// an undefined `cluster` and an empty header). Nil ClusterInfo closure => zero
// values (an uninitialised node still answers so the SPA can render).
func (s *Server) handleClusterInfo(w http.ResponseWriter, _ *http.Request) {
	var v ClusterInfoView
	if s.deps.ClusterInfo != nil {
		v = s.deps.ClusterInfo()
	}
	resp := clusterInfoResponse{Version: v.Version}
	resp.Cluster.Name = v.ClusterName
	resp.Cluster.CAFingerprint = v.CAFingerprint
	resp.Cluster.Created = v.Created
	resp.Counts.Nodes = v.NodeCount
	if s.deps.State != nil {
		resp.Counts.Groups = len(s.deps.State().Groups)
	}
	w.Header().Set("ETag", strconv.FormatUint(v.Version, 10))
	writeJSON(w, resp)
}

// discoveryResponse is the GET /api/v1/discovery body: the current cluster
// members (the ConfigDoc nodes joined with gossip liveness) plus the
// LAN-discovered-but-unadopted nodes (mDNS cache). The SPA reads
// {members, discovered}.
type discoveryResponse struct {
	Members    []MemberView `json:"members"`
	Discovered []Discovered `json:"discovered"`
}

// handleDiscovery serves GET /api/v1/discovery: known members + discovered peers.
// Members come from Deps.Members (liveness-joined; State fallback); discovered
// come from Deps.Discovery (mDNS cache), re-filtered against the member ids so a
// just-adopted node never shows in both lists while the 5s browse cache catches
// up. Nil-safe (empty slices, never null).
func (s *Server) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	resp := discoveryResponse{Members: []MemberView{}, Discovered: []Discovered{}}
	var version uint64
	if s.deps.State != nil {
		version = s.deps.State().Version
	}
	switch {
	case s.deps.Members != nil:
		if m := s.deps.Members(); len(m) > 0 {
			resp.Members = m
		}
	case s.deps.State != nil:
		for _, n := range s.deps.State().Nodes {
			resp.Members = append(resp.Members, MemberView{NodeView: n})
		}
	}
	memberIDs := make(map[string]bool, len(resp.Members))
	for _, m := range resp.Members {
		memberIDs[m.ID] = true
	}
	if s.deps.Discovery != nil {
		for _, d := range s.deps.Discovery() {
			if memberIDs[d.NodeID] {
				continue // already a member; the stale mDNS cache hasn't refreshed yet
			}
			resp.Discovered = append(resp.Discovered, d)
		}
	}
	w.Header().Set("ETag", strconv.FormatUint(version, 10))
	writeJSON(w, resp)
}

// nodesResponse is the GET /api/v1/nodes body: {version, nodes}.
type nodesResponse struct {
	Version uint64     `json:"version"`
	Nodes   []NodeView `json:"nodes"`
}

// handleListNodes serves GET /api/v1/nodes: the cluster's member node records
// (redacted projection via Deps.State). Nil-safe (empty list).
func (s *Server) handleListNodes(w http.ResponseWriter, _ *http.Request) {
	resp := nodesResponse{Nodes: []NodeView{}}
	if s.deps.State != nil {
		cfg := s.deps.State()
		resp.Version = cfg.Version
		if len(cfg.Nodes) > 0 {
			resp.Nodes = cfg.Nodes
		}
	}
	w.Header().Set("ETag", strconv.FormatUint(resp.Version, 10))
	writeJSON(w, resp)
}

// groupsResponse is the GET /api/v1/groups body: {version, groups}.
type groupsResponse struct {
	Version uint64      `json:"version"`
	Groups  []GroupView `json:"groups"`
}

// handleListGroups serves GET /api/v1/groups: the cluster's group records
// (redacted projection via Deps.State). Nil-safe (empty list).
func (s *Server) handleListGroups(w http.ResponseWriter, _ *http.Request) {
	resp := groupsResponse{Groups: []GroupView{}}
	if s.deps.State != nil {
		cfg := s.deps.State()
		resp.Version = cfg.Version
		if len(cfg.Groups) > 0 {
			resp.Groups = cfg.Groups
		}
	}
	w.Header().Set("ETag", strconv.FormatUint(resp.Version, 10))
	writeJSON(w, resp)
}
