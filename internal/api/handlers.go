package api

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// handleStatus reports this node's identity, role, group, and live sink/clock/
// source stats (§9.1, D19).
func (s *Server) handleStatus(c echo.Context) error {
	self := s.cfg.Cluster.Self()
	snap := s.cfg.Cluster.Snapshot()

	name := self.String()[:8]
	for _, n := range snap.Nodes {
		if n.ID == self {
			if n.Name != "" {
				name = n.Name
			}
			break
		}
	}

	role, gid := roleAndGroup(snap, self)

	var stats StatusStats
	if s.cfg.Stats != nil {
		stats = s.cfg.Stats()
	}

	resp := StatusResp{
		ID:      self.String(),
		Name:    name,
		Role:    role,
		GroupID: gid.String(),
		Ports:   s.cfg.Ports,
		Sink:    sinkStatsResp(stats.Sink),
		Clock:   stats.Clock,
		Source:  stats.Source,
	}
	return c.JSON(http.StatusOK, resp)
}

// roleAndGroup derives this node's role ("master"/"follower"/"solo") and group
// id from the resolved snapshot (§5). "solo" == master of a group of 1.
func roleAndGroup(snap contracts.Snapshot, self id.ID) (string, id.ID) {
	for _, g := range snap.Groups {
		inGroup := false
		for _, m := range g.Members {
			if m == self {
				inGroup = true
				break
			}
		}
		if !inGroup {
			continue
		}
		if g.Master == self {
			if len(g.Members) <= 1 {
				return "solo", g.ID
			}
			return "master", g.ID
		}
		return "follower", g.ID
	}
	// Not yet in any derived group (e.g. boot before self-record resolves):
	// behave as a solo group of self.
	return "solo", self
}

// handleCluster returns the resolved snapshot verbatim (§9.1) — no wrapper.
func (s *Server) handleCluster(c echo.Context) error {
	return c.JSON(http.StatusOK, s.cfg.Cluster.Snapshot())
}

// handleMedia lists this node's local playable files (§6).
func (s *Server) handleMedia(c echo.Context) error {
	if s.cfg.Media == nil {
		return c.JSON(http.StatusOK, MediaResp{Files: []MediaFile{}})
	}
	files, err := s.cfg.Media.List()
	if err != nil {
		s.log.Warn("media list failed", "err", err)
		return failCode(c, http.StatusInternalServerError, "internal_error", "")
	}
	if files == nil {
		files = []MediaFile{}
	}
	return c.JSON(http.StatusOK, MediaResp{Files: files})
}

// handlePatchNode applies {name?, volume?, outputDelayMs?} to THIS node:
// persist (A) → replicate (C) → apply live (E), per field (§9.1, D35/D36).
func (s *Server) handlePatchNode(c echo.Context) error {
	var req NodePatchReq
	if err := c.Bind(&req); err != nil {
		return failCode(c, http.StatusBadRequest, "bad_request", "")
	}
	if req.Name == nil && req.Volume == nil && req.OutputDelayMs == nil {
		return failCode(c, http.StatusBadRequest, "empty_patch", "")
	}

	// Validate everything up front; nothing is applied unless all valid.
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		return failCode(c, http.StatusBadRequest, "empty_name", "")
	}
	if req.Volume != nil && (*req.Volume < 0.0 || *req.Volume > 1.0) {
		return failCode(c, http.StatusBadRequest, "bad_volume", "")
	}
	if req.OutputDelayMs != nil && (*req.OutputDelayMs < -500 || *req.OutputDelayMs > 500) {
		return failCode(c, http.StatusBadRequest, "bad_delay", "")
	}

	if req.Name != nil {
		if err := s.cfg.NodeCfg.Rename(*req.Name); err != nil {
			s.log.Warn("rename persist failed", "err", err)
			return failCode(c, http.StatusInternalServerError, "internal_error", "")
		}
		s.cfg.Cluster.SetName(*req.Name)
	}
	if req.Volume != nil {
		if err := s.cfg.NodeCfg.SetVolume(*req.Volume); err != nil {
			s.log.Warn("volume persist failed", "err", err)
			return failCode(c, http.StatusInternalServerError, "internal_error", "")
		}
		s.cfg.Cluster.SetVolume(*req.Volume)
		if sink := s.sink(); sink != nil {
			sink.SetGain(*req.Volume)
		}
	}
	if req.OutputDelayMs != nil {
		if err := s.cfg.NodeCfg.SetOutputDelayMs(*req.OutputDelayMs); err != nil {
			s.log.Warn("delay persist failed", "err", err)
			return failCode(c, http.StatusInternalServerError, "internal_error", "")
		}
		s.cfg.Cluster.SetOutputDelayMs(*req.OutputDelayMs)
		if sink := s.sink(); sink != nil {
			sink.SetDelayOffset(int64(*req.OutputDelayMs) * 1_000_000)
		}
	}
	return c.NoContent(http.StatusNoContent)
}

// sink returns the live sink control or nil (playback-less / pre-session).
func (s *Server) sink() SinkControl {
	if s.cfg.Sink == nil {
		return nil
	}
	return s.cfg.Sink()
}

// handleFollow makes THIS node follow target (§5.1).
func (s *Server) handleFollow(c echo.Context) error {
	var req FollowReq
	if err := c.Bind(&req); err != nil {
		return failCode(c, http.StatusBadRequest, "bad_request", "")
	}
	target, err := id.Parse(req.Target)
	if err != nil {
		return failCode(c, http.StatusBadRequest, "bad_target", "")
	}
	if err := s.cfg.Group.Follow(c.Request().Context(), target); err != nil {
		return s.fail(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// handleUnfollow makes THIS node a solo master (§5.1).
func (s *Server) handleUnfollow(c echo.Context) error {
	if err := s.cfg.Group.Unfollow(c.Request().Context()); err != nil {
		return s.fail(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// handleGroupName names a group (§4/§9.1; any node may write, LWW).
func (s *Server) handleGroupName(c echo.Context) error {
	var req GroupNameReq
	if err := c.Bind(&req); err != nil {
		return failCode(c, http.StatusBadRequest, "bad_request", "")
	}
	gid, err := id.Parse(req.Group)
	if err != nil {
		return failCode(c, http.StatusBadRequest, "bad_group", "")
	}
	if err := s.cfg.Group.NameGroup(c.Request().Context(), gid, req.Name); err != nil {
		return s.fail(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// handleGroupMaster runs takeover so node becomes master of its group (§5.2).
func (s *Server) handleGroupMaster(c echo.Context) error {
	var req MasterReq
	if err := c.Bind(&req); err != nil {
		return failCode(c, http.StatusBadRequest, "bad_request", "")
	}
	node, err := id.Parse(req.Node)
	if err != nil {
		return failCode(c, http.StatusBadRequest, "bad_node", "")
	}
	if err := s.cfg.Group.MakeMaster(c.Request().Context(), node); err != nil {
		return s.fail(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// handlePlay serves a media-source URI to THIS node's group; master only
// (§6/§9.1). {file} folds to a "file:" URI; a bare scheme-less path too.
func (s *Server) handlePlay(c echo.Context) error {
	var req PlayReq
	if err := c.Bind(&req); err != nil {
		return failCode(c, http.StatusBadRequest, "bad_request", "")
	}
	uri := resolvePlayURI(req)
	if uri == "" {
		return failCode(c, http.StatusBadRequest, "bad_request", "")
	}
	if path, isFile := fileURIPath(uri); isFile && badMediaPath(path) {
		return failCode(c, http.StatusBadRequest, "bad_path", "")
	}
	if err := s.cfg.Group.Play(c.Request().Context(), uri); err != nil {
		return s.fail(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// resolvePlayURI picks the effective URI: uri wins; else {file} (or a bare
// path) folds to "file:". Empty if neither is present.
func resolvePlayURI(req PlayReq) string {
	u := strings.TrimSpace(req.URI)
	if u == "" {
		u = strings.TrimSpace(req.File)
		if u == "" {
			return ""
		}
	}
	if !hasScheme(u) {
		return "file:" + u
	}
	return u
}

// hasScheme reports whether u carries a "scheme:" prefix (file/http/https/input).
func hasScheme(u string) bool {
	i := strings.IndexByte(u, ':')
	if i <= 0 {
		return false
	}
	// A Windows-style or bare relative path won't have an early alpha scheme; we
	// treat anything before ':' that is a simple word as a scheme.
	for _, r := range u[:i] {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') &&
			!(r >= '0' && r <= '9') && r != '+' && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

// fileURIPath extracts the path of a "file:" URI (or returns isFile=false).
func fileURIPath(uri string) (path string, isFile bool) {
	if strings.HasPrefix(uri, "file:") {
		return strings.TrimPrefix(uri, "file:"), true
	}
	return "", false
}

// badMediaPath rejects absolute paths and traversal outside MEDIA_DIR (§6).
func badMediaPath(p string) bool {
	if p == "" {
		return true
	}
	if strings.HasPrefix(p, "/") {
		return true
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// handleStop stops THIS node's group playback; master only.
func (s *Server) handleStop(c echo.Context) error {
	if err := s.cfg.Group.Stop(c.Request().Context()); err != nil {
		return s.fail(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// handleGetSettings returns this node's group settings (§9.1).
func (s *Server) handleGetSettings(c echo.Context) error {
	return c.JSON(http.StatusOK, s.cfg.Group.Settings())
}

// handleSetSettings updates this node's group settings; master only, applies
// live via RECONFIG (§8.7, D23).
func (s *Server) handleSetSettings(c echo.Context) error {
	var body contracts.GroupSettings
	if err := c.Bind(&body); err != nil {
		return failCode(c, http.StatusBadRequest, "bad_request", "")
	}
	if err := s.cfg.Group.SetSettings(c.Request().Context(), body); err != nil {
		return s.fail(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
