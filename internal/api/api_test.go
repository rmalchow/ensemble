package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"ensemble/internal/id"
)

func newLocalListener() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}

func strReader(s string) io.Reader { return strings.NewReader(s) }

// dialTimeout2s is a DialContext with a short timeout so failover tests don't
// hang on blackholed addresses.
func dialTimeout2s(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	return d.DialContext(ctx, network, addr)
}

// testServer builds a Server with the given fakes and an in-memory placeholder
// SPA, ready for httptest. The returned *httptest.Server serves the Echo router.
func testServer(t *testing.T, cfg Config) (*Server, *httptest.Server) {
	t.Helper()
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.DistFS == nil {
		cfg.DistFS = fstest.MapFS{
			"index.html": &fstest.MapFile{Data: []byte("<html><!-- ensemble-placeholder --></html>")},
		}
	}
	s := New(cfg)
	ts := httptest.NewServer(s.e)
	t.Cleanup(func() {
		ts.Close()
		_ = s.Shutdown(context.Background())
	})
	return s, ts
}

// baseConfig builds a minimal valid Config with default fakes.
func baseConfig(self id.ID) (Config, *fakeCluster, *fakeGroup) {
	fc := newFakeCluster(self)
	fg := &fakeGroup{}
	cfg := Config{
		Cluster: fc,
		Group:   fg,
		Media:   &fakeMedia{},
		NodeCfg: &fakeNodeConfig{},
		Stats:   func() StatusStats { return StatusStats{} },
		Sink:    func() SinkControl { return nil },
		Ports:   PortsResp{HTTP: 8080, Stream: 9090, Source: 9200, Gossip: 7946},
	}
	return cfg, fc, fg
}

func doJSON(t *testing.T, ts *httptest.Server, method, path string, body any) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ts.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeErr(t *testing.T, resp *http.Response) ErrorResp {
	t.Helper()
	var e ErrorResp
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	resp.Body.Close()
	return e
}

func TestNewRegistersAllRoutes(t *testing.T) {
	cfg, _, _ := baseConfig(id.New())
	s := New(cfg)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	want := map[string]bool{
		"GET /api/status":          false,
		"PATCH /api/node":          false,
		"GET /api/cluster":         false,
		"GET /api/media":           false,
		"POST /api/follow":         false,
		"POST /api/unfollow":       false,
		"POST /api/group/name":     false,
		"POST /api/group/master":   false,
		"POST /api/play":           false,
		"POST /api/stop":           false,
		"POST /api/pause":          false,
		"POST /api/resume":         false,
		"GET /api/group/settings":  false,
		"POST /api/group/settings": false,
		"GET /api/ws":              false,
	}
	for _, r := range s.e.Routes() {
		key := r.Method + " " + r.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("route not registered: %s", k)
		}
	}
}

func TestStartShutdownClean(t *testing.T) {
	cfg, _, _ := baseConfig(id.New())
	ln, err := newLocalListener()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listener = ln
	cfg.DistFS = fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok")}}
	s := New(cfg)

	errc := make(chan error, 1)
	go func() { errc <- s.Start() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Start returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after Shutdown")
	}
}

func TestErrorHandlerEmitsJSON(t *testing.T) {
	cfg, _, _ := baseConfig(id.New())
	_, ts := testServer(t, cfg)

	// An unknown /api route → 404 with JSON envelope, not HTML.
	resp := doJSON(t, ts, http.MethodGet, "/api/nope-not-a-route-xx", nil)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q, want json", ct)
	}
}

func TestBodyLimitRejectsOversize(t *testing.T) {
	cfg, _, _ := baseConfig(id.New())
	_, ts := testServer(t, cfg)

	big := strings.Repeat("a", 2<<20) // 2 MiB
	req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/node", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

// readBody reads and closes a response body.
func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return b
}

// distWith builds a MapFS dist with the given files.
func distWith(files map[string]string) fs.FS {
	m := fstest.MapFS{}
	for k, v := range files {
		m[k] = &fstest.MapFile{Data: []byte(v)}
	}
	return m
}
