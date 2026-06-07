// Package api is the Echo HTTP server: REST routes (§9.1), WebSocket pushes
// (§9.2), the node proxy (§9.3), and SPA serving from the web/dist embed (§10).
// It is thin: every route reads the cluster snapshot (piece C) or delegates a
// mutation to the cluster setters (C) or the group engine (H). The API holds no
// domain state of its own beyond the WebSocket hub and the embedded SPA.
package api

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"

	"github.com/labstack/echo/v4"
)

// Config bundles everything the API needs, wired by main (K).
type Config struct {
	Cluster Cluster
	Group   Group
	Media   Media
	NodeCfg NodeConfig         // config A: persist PATCH /api/node fields
	Stats   func() StatusStats // closure over sink (E), clock (F), source (G) stats
	Sink    func() SinkControl // closure → the live sink (E); may return nil
	// ApplyOutputDevice reopens the output backend for the new device and swaps it
	// into the live sink (D37, §8.5). Wired by main (K); only effective when the
	// active backend kind is alsa (otherwise a no-op — persist+replicate still
	// happen). nil makes the live-apply step a no-op.
	ApplyOutputDevice func(device string)
	// ApplyDisabled applies the operator-disabled feature list live (D40): when
	// "playback" is (un)disabled it swaps the sink to the null backend (or back to
	// the configured device/backend). Wired by main (K); nil makes the live-apply
	// step a no-op (persist+replicate still happen). opus/input disabling needs no
	// live swap — effective caps gate new sessions and the constructors refuse.
	ApplyDisabled func(disabled []string)
	Ports         PortsResp    // actually-bound ports (§2), surfaced by /api/status
	Listener      net.Listener // HTTP listener from netx.BindTCP (K owns binding)
	DistFS        fs.FS        // SPA build FS = web.DistFS (D15)
	Log           *slog.Logger
}

// Server is the Echo HTTP server: REST + WebSocket + proxy + SPA.
type Server struct {
	e   *echo.Echo
	cfg Config
	hub *wsHub
	spa *spaState
	log *slog.Logger
}

// New builds the server, registers all routes/middleware, and starts the WS
// hub goroutine. It does NOT begin accepting connections — call Start.
func New(cfg Config) *Server {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("comp", "api")

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.HTTPErrorHandler = jsonErrorHandler(log)

	s := &Server{
		e:   e,
		cfg: cfg,
		hub: newWSHub(cfg.Cluster, log),
		log: log,
	}

	// Pre-router: observe (§3.1) + proxy (§9.3) run BEFORE routing so a request
	// for another node is short-circuited, and a self/one-hop request gets its
	// /api/<id> segment stripped so the local router then matches it.
	e.Pre(s.observeMiddleware)
	e.Pre(s.proxyMiddleware)

	e.Use(recoverMiddleware(log))
	e.Use(requestLogMiddleware(log))    // DEBUG access log (non-mutating chatter)
	e.Use(bodyLimitMiddleware(1 << 20)) // 1 MiB (§9 no large uploads)

	g := e.Group("/api")
	g.GET("/status", s.handleStatus)
	g.PATCH("/node", s.handlePatchNode)
	g.GET("/cluster", s.handleCluster)
	g.GET("/media", s.handleMedia)
	g.POST("/follow", s.handleFollow)
	g.POST("/unfollow", s.handleUnfollow)
	g.POST("/group/name", s.handleGroupName)
	g.POST("/group/master", s.handleGroupMaster)
	g.POST("/play", s.handlePlay)
	g.POST("/stop", s.handleStop)
	g.POST("/pause", s.handlePause)
	g.POST("/resume", s.handleResume)
	g.GET("/group/settings", s.handleGetSettings)
	g.POST("/group/settings", s.handleSetSettings)
	g.POST("/tone", s.handleTone)
	g.GET("/ws", s.handleWS)

	// SPA: everything not under /api. Registered on the root Echo, last.
	s.initSPA()
	e.GET("/*", s.handleSPA)

	go s.hub.run()
	return s
}

// Start serves on cfg.Listener until Shutdown. Blocks; run in a goroutine.
// Returns nil on clean shutdown (http.ErrServerClosed is folded to nil).
func (s *Server) Start() error {
	s.e.Listener = s.cfg.Listener
	err := s.e.Start("")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully drains HTTP, closes all WebSocket connections, and stops
// the hub. Honors ctx deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	s.hub.close()
	return s.e.Shutdown(ctx)
}

// FollowClient returns a contracts.FollowClient bound to this server's cluster
// so the group engine (H) can drive takeover (§5.2). Equivalent to
// NewFollowClient(s.cfg.Cluster); provided for symmetry with the arch doc.
func (s *Server) FollowClient() FollowClientImpl {
	return NewFollowClient(s.cfg.Cluster)
}
