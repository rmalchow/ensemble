package web

import (
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/auth"
)

// Server hosts the control-plane HTTP/1.1 + WebSocket surface over (m)TLS. It
// reads node state and triggers operations only through Deps, so it never
// imports group, stream/* or audio/* (doc 01 §2 rule 1). It owns the asset
// handler (embedded build or, in dev, a disk dir) and the websocket hub.
type Server struct {
	deps   Deps
	devDir string
	mux    *http.ServeMux
	hub    *hub
	// sessions is the per-node, in-memory human-session store (03 §7.2). Owned by
	// the Server (not replicated): a cookie issued here is valid only on this node.
	sessions *auth.Sessions
}

// New builds a Server. If devDir is non-empty, assets are served from that
// directory on disk (useful for on-device iteration); otherwise they come from
// the embedded build.
func New(deps Deps, devDir string) *Server {
	s := &Server{
		deps:     deps,
		devDir:   devDir,
		mux:      http.NewServeMux(),
		hub:      newHub(),
		sessions: auth.NewSessions(),
	}
	s.registerRoutes()
	return s
}

// Handler returns the server's root http.Handler (the wired mux). It lets cmd
// mount the control plane on an externally-supplied server (e.g. the bootstrap
// listener) and lets out-of-package tests drive the full route table via httptest
// without reaching into unexported fields.
func (s *Server) Handler() http.Handler { return s.mux }

// registerRoutes wires the mux. This is the extension point later pieces hook
// into to add the /api/v1/* and /bootstrap/* handlers; for the skeleton it is
// just /healthz, /ws, and the asset handler (with SPA fallback) as the
// catch-all. The asset catch-all already 404s unknown /api/ + /ws paths, so
// adding real handlers later is additive and non-breaking.
func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	s.mux.HandleFunc("GET /ws", s.handleWS)
	// The /bootstrap/* surface (08 §A) lives OUTSIDE mTLS and OUTSIDE the [0..4]
	// /api/v1 auth chain: it is the unauthenticated probe + PIN-gated A.9 handshake
	// an uninitialized node exposes so a controller can adopt it (03 §9). PIN
	// auth + the A.12 guard are enforced inside the handlers, not by the chain.
	s.mux.HandleFunc("GET /bootstrap/info", s.handleBootstrapInfo)
	s.mux.HandleFunc("POST /bootstrap/adopt", s.handleBootstrapAdopt)
	// /bootstrap/takeover (03 §4): the password-authorized self-release a foreign
	// controller drives before re-adopting a node that is already a member of
	// another cluster. Guarded by the same A.12 brute-force throttle as the PIN.
	s.mux.HandleFunc("POST /bootstrap/takeover", s.handleBootstrapTakeover)
	// The 08 §B setup/auth surface under the [0..4] auth chain (routes.go). It is
	// registered before the asset catch-all so /api/v1/* is handled by the API mux
	// (the catch-all's /api/ guard only 404s genuinely-unmatched API paths).
	s.registerAPIRoutes()
	s.mux.Handle("/", s.assetHandler())
}

// assetHandler serves static assets from the embedded build (or devDir) with an
// SPA fallback: unknown paths that are not under /api/ or /ws resolve to
// index.html so client-side routing works.
func (s *Server) assetHandler() http.Handler {
	var assets fs.FS
	if s.devDir != "" {
		assets = os.DirFS(s.devDir)
	} else {
		sub, err := fs.Sub(DistFS, "dist")
		if err != nil {
			// Should never happen: dist is embedded at build time.
			panic(err)
		}
		assets = sub
	}
	fileServer := http.FileServer(http.FS(assets))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if upath == "" {
			upath = "index.html"
		}
		// API and websocket paths are never assets; if they reach the catch-all
		// they are genuinely unhandled (their real handlers land in later pieces).
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/ws" {
			http.NotFound(w, r)
			return
		}
		if _, err := fs.Stat(assets, upath); err != nil {
			// SPA fallback: serve index.html for unknown client-side routes.
			r = r.Clone(r.Context())
			r.URL.Path = "/"
			serveIndex(w, r, assets)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

// serveIndex writes index.html for the SPA fallback.
func serveIndex(w http.ResponseWriter, r *http.Request, assets fs.FS) {
	f, err := assets.Open("index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, "index.html", time.Time{}, rs)
		return
	}
	_, _ = io.Copy(w, f)
}
