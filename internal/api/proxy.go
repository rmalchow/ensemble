package api

import (
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"ensemble/internal/id"
)

const (
	// proxiedHeader marks a one-hop proxied request (§9.3).
	proxiedHeader = "X-Ensemble-Proxied"
	// fromHeader carries the originating node id so the target can Observe it.
	fromHeader = "X-Ensemble-From"
)

// reservedSegs are literal /api routes that are never node ids/names (§9.3).
var reservedSegs = map[string]bool{
	"status":   true,
	"node":     true,
	"cluster":  true,
	"media":    true,
	"follow":   true,
	"unfollow": true,
	"group":    true,
	"play":     true,
	"stop":     true,
	"ws":       true,
}

// proxyHTTP is the client used for reverse proxying (LAN). A short per-dial
// timeout lets DialCandidates failover move on from a dead address quickly,
// while the overall Timeout bounds a slow upstream.
var proxyHTTP = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
	},
}

// proxyMiddleware short-circuits requests whose first /api path segment is a
// 32-hex node id OR a unique node name, reverse-proxying them to that node's
// HTTP port (§9.3). All other requests pass through to local handlers.
func (s *Server) proxyMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		req := c.Request()
		seg, rest := firstSegment(req.URL.Path)
		if seg == "" || reservedSegs[seg] {
			return next(c)
		}

		target, ok, ambiguous := s.resolveTarget(seg)
		if ambiguous {
			return failCode(c, http.StatusNotFound, "ambiguous_node", "")
		}
		if !ok {
			// Not a node id/name → local route (Echo will 404 if unknown).
			return next(c)
		}

		// One-hop guard: a request that already came through a proxy is terminal.
		// Strip the segment and route locally even if target is foreign (§9.3).
		if req.Header.Get(proxiedHeader) != "" {
			rewriteLocal(c, rest)
			return next(c)
		}

		// Self-target → handle locally, no network hop.
		if target == s.cfg.Cluster.Self() {
			rewriteLocal(c, rest)
			return next(c)
		}

		return s.proxyTo(c, target, rest)
	}
}

// firstSegment returns the first path component after "/api/" and the remaining
// path (with a leading "/api"). For "/api/<id>/media" → ("<id>", "/api/media").
// For "/api/status" → ("status", "/api/status"). For "/api" → ("", "/api").
func firstSegment(p string) (seg, rest string) {
	const prefix = "/api/"
	if !strings.HasPrefix(p, prefix) {
		return "", p
	}
	tail := p[len(prefix):]
	i := strings.IndexByte(tail, '/')
	if i < 0 {
		return tail, p // single segment, e.g. "/api/status"
	}
	return tail[:i], "/api" + tail[i:]
}

// rewriteLocal rewrites the request URL path to rest so local handlers match.
func rewriteLocal(c echo.Context, rest string) {
	c.Request().URL.Path = rest
}

// resolveTarget maps a segment to a node id: 32-hex id, else unique alive node
// name. ambiguous is true when a name matches >1 alive node (§9.3).
func (s *Server) resolveTarget(seg string) (target id.ID, ok bool, ambiguous bool) {
	if pid, err := id.Parse(seg); err == nil {
		return pid, true, false
	}
	snap := s.cfg.Cluster.Snapshot()
	var match id.ID
	count := 0
	for _, n := range snap.Nodes {
		if n.Name == seg && n.Alive {
			match = n.ID
			count++
		}
	}
	if count == 1 {
		return match, true, false
	}
	if count > 1 {
		return id.Zero, false, true
	}
	return id.Zero, false, false
}

// proxyTo reverse-proxies the request to target's HTTP port, trying dial
// candidates in order; 502 if none connect (§9.3).
func (s *Server) proxyTo(c echo.Context, target id.ID, rest string) error {
	port := s.httpPortOf(target)
	addrs := s.cfg.Cluster.DialCandidates(target)
	if port == 0 || len(addrs) == 0 {
		return failCode(c, http.StatusBadGateway, "unreachable", "")
	}

	in := c.Request()
	var lastErr error
	for _, a := range addrs {
		hostPort := net.JoinHostPort(a.String(), strconv.Itoa(port))
		url := "http://" + hostPort + rest
		if in.URL.RawQuery != "" {
			url += "?" + in.URL.RawQuery
		}

		out, err := http.NewRequestWithContext(in.Context(), in.Method, url, in.Body)
		if err != nil {
			lastErr = err
			continue
		}
		copyHeaders(out.Header, in.Header)
		out.Header.Set(proxiedHeader, "1")
		out.Header.Set(fromHeader, s.cfg.Cluster.Self().String())

		resp, err := proxyHTTP.Do(out)
		if err != nil {
			lastErr = err
			continue
		}
		return writeProxiedResponse(c, resp)
	}
	if lastErr != nil {
		s.log.Warn("proxy failed", "target", target.String(), "err", lastErr)
	}
	return failCode(c, http.StatusBadGateway, "unreachable", "")
}

// httpPortOf finds target's advertised HTTP port from the snapshot.
func (s *Server) httpPortOf(target id.ID) int {
	snap := s.cfg.Cluster.Snapshot()
	for _, n := range snap.Nodes {
		if n.ID == target {
			return n.HTTPPort
		}
	}
	return 0
}

// copyHeaders copies request headers, skipping hop-by-hop ones.
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopByHop(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func hopByHop(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length":
		return true
	}
	return false
}

// writeProxiedResponse streams the upstream response back to the client.
func writeProxiedResponse(c echo.Context, resp *http.Response) error {
	defer resp.Body.Close()
	out := c.Response()
	for k, vs := range resp.Header {
		if hopByHop(k) {
			continue
		}
		for _, v := range vs {
			out.Header().Add(k, v)
		}
	}
	out.WriteHeader(resp.StatusCode)
	_, err := io.Copy(out, resp.Body)
	return err
}
