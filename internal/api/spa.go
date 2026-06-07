package api

import (
	"bytes"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/labstack/echo/v4"
)

// placeholderSentinel marks the committed dist/index.html placeholder (S). When
// present, the UI isn't built yet; we still serve it and log a warning once.
const placeholderSentinel = "ensemble-placeholder"

// spaState holds the SPA file server + index bytes, built once at New.
type spaState struct {
	fileServer http.Handler
	index      []byte
	hasIndex   bool
}

// initSPA prepares the SPA file server from cfg.DistFS and detects the
// placeholder (§10, D15).
func (s *Server) initSPA() {
	st := &spaState{}
	if s.cfg.DistFS != nil {
		st.fileServer = http.FileServer(http.FS(s.cfg.DistFS))
		if data, err := fs.ReadFile(s.cfg.DistFS, "index.html"); err == nil {
			st.index = data
			st.hasIndex = true
			if bytes.Contains(data, []byte(placeholderSentinel)) {
				s.log.Warn("serving SPA placeholder; web UI not built (run npm run build)")
			}
		}
	}
	s.spa = st
}

// handleSPA serves the SPA FS for any non-/api path, falling back to index.html
// for client-side routes (§10).
func (s *Server) handleSPA(c echo.Context) error {
	if s.spa == nil || s.cfg.DistFS == nil {
		return c.String(http.StatusNotFound, "no UI")
	}
	req := c.Request()
	upath := req.URL.Path
	// An unmatched /api/* path is an unknown API route, not an SPA route: emit
	// the JSON error envelope rather than serving index.html (§4).
	if upath == "/api" || strings.HasPrefix(upath, "/api/") {
		return c.JSON(http.StatusNotFound, ErrorResp{Error: "not_found"})
	}
	if upath == "/" || upath == "" {
		return s.serveIndex(c)
	}

	clean := strings.TrimPrefix(path.Clean(upath), "/")
	if clean == "" || clean == "." {
		return s.serveIndex(c)
	}

	// If the asset exists, serve it directly via the file server.
	if f, err := s.cfg.DistFS.Open(clean); err == nil {
		f.Close()
		s.spa.fileServer.ServeHTTP(c.Response(), req)
		return nil
	}

	// Unknown path: a real asset request (has an extension) → 404; otherwise
	// SPA client route → index.html fallback.
	if ext := path.Ext(clean); ext != "" {
		return c.String(http.StatusNotFound, "not found")
	}
	return s.serveIndex(c)
}

// serveIndex writes index.html (or a 404 if there is none).
func (s *Server) serveIndex(c echo.Context) error {
	if !s.spa.hasIndex {
		return c.String(http.StatusNotFound, "no UI")
	}
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMETextHTMLCharsetUTF8)
	c.Response().WriteHeader(http.StatusOK)
	_, err := io.Copy(c.Response(), bytes.NewReader(s.spa.index))
	return err
}
