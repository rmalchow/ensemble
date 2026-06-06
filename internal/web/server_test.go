package web

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/config"
)

// newTestServer builds a Server backed by the embedded placeholder dist with a
// minimally-wired Deps.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	return New(Deps{
		NodeID: "testnode",
		Paths:  config.Paths{},
		Status: func() NodeStatus { return NodeStatus{Role: "starting"} },
	}, "")
}

func TestHealthz(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz: got %d want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "ok" {
		t.Fatalf("/healthz body: got %q want %q", got, "ok")
	}
}

func TestAssetServeAndSPAFallback(t *testing.T) {
	s := newTestServer(t)
	tests := []struct {
		name      string
		path      string
		wantCode  int
		wantHTML  bool
		wantCTHas string
	}{
		{name: "root serves index with html content-type", path: "/", wantCode: 200, wantHTML: true, wantCTHas: "text/html"},
		{name: "unknown client route -> SPA index", path: "/groups/x", wantCode: 200, wantHTML: true},
		// /api/v1/* is now claimed by the auth chain (P1.3): an unmatched path on a
		// node treated-as-initialized (test Deps has Initialized==nil) reaches the
		// [3] human-auth step with no credential => 401. The asset catch-all's
		// /api/ guard still 404s any OTHER /api/ prefix (e.g. a future /api/v2).
		{name: "gated api path -> 401", path: "/api/v1/foo", wantCode: 401},
		{name: "non-v1 api prefix falls through to 404", path: "/api/v2/foo", wantCode: 404},
		{name: "non-/ws path is a normal SPA route", path: "/ws-not", wantCode: 200, wantHTML: true}, // not exactly "/ws"
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			s.mux.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("%s: got %d want %d", tc.path, rec.Code, tc.wantCode)
			}
			if tc.wantHTML && !strings.Contains(rec.Body.String(), "<html") {
				t.Fatalf("%s: body did not look like HTML: %q", tc.path, rec.Body.String())
			}
			if tc.wantCTHas != "" && !strings.Contains(rec.Header().Get("Content-Type"), tc.wantCTHas) {
				t.Fatalf("%s: content-type %q does not contain %q", tc.path, rec.Header().Get("Content-Type"), tc.wantCTHas)
			}
		})
	}
}

// TestWSFallthrough404 checks the exact "/ws" path falls through to 404 only
// when handleWS is not engaged (i.e. a non-upgrade GET reaching the asset
// catch-all guard). Since "/ws" is a registered route handled by handleWS, a
// plain GET without upgrade headers returns the websocket library's 400/426
// rather than the asset 404; this test pins the asset-guard behaviour for the
// "/ws" literal by hitting the guard directly via assetHandler.
func TestWSAssetGuard(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	s.assetHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/ws asset guard: got %d want 404", rec.Code)
	}
}

// selfSignedTLS returns a TLS server config with a fresh self-signed leaf for
// "localhost"/127.0.0.1 and TLS1.3, and a client config that trusts it.
func selfSignedTLS(t *testing.T) (server *tls.Config, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	server = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	}
	client = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}
	return server, client
}

func TestServeTLSWrap(t *testing.T) {
	tests := []struct {
		name string
		tls  bool
	}{
		{name: "plain (TLSConfig nil)", tls: false},
		{name: "TLS1.3 (TLSConfig set)", tls: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var serverCfg, clientCfg *tls.Config
			deps := Deps{NodeID: "n"}
			if tc.tls {
				serverCfg, clientCfg = selfSignedTLS(t)
				deps.TLSConfig = func() *tls.Config { return serverCfg }
			}
			s := New(deps, "")

			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- s.Serve(ctx, ln) }()

			scheme := "http"
			httpClient := &http.Client{Timeout: 3 * time.Second}
			if tc.tls {
				scheme = "https"
				httpClient.Transport = &http.Transport{TLSClientConfig: clientCfg}
			}
			url := scheme + "://" + ln.Addr().String() + "/healthz"

			resp, err := getWithRetry(httpClient, url)
			if err != nil {
				t.Fatalf("GET %s: %v", url, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status: got %d want 200", resp.StatusCode)
			}
			if tc.tls {
				if resp.TLS == nil {
					t.Fatal("expected TLS connection state, got nil")
				}
				if resp.TLS.Version != tls.VersionTLS13 {
					t.Fatalf("TLS version: got %x want %x", resp.TLS.Version, tls.VersionTLS13)
				}
			}
			body, _ := io.ReadAll(resp.Body)
			if strings.TrimSpace(string(body)) != "ok" {
				t.Fatalf("body: got %q want ok", body)
			}

			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Serve returned: %v", err)
				}
			case <-time.After(6 * time.Second):
				t.Fatal("Serve did not return within 6s of cancel")
			}
		})
	}
}

// getWithRetry retries briefly because Serve starts its listener loop in a
// goroutine; the first dial can race the accept loop becoming ready.
func getWithRetry(c *http.Client, url string) (*http.Response, error) {
	var lastErr error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := c.Get(url)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	return nil, lastErr
}

func TestServeGracefulShutdown(t *testing.T) {
	s := New(Deps{NodeID: "n"}, "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()

	client := &http.Client{Timeout: 3 * time.Second}
	url := "http://" + ln.Addr().String() + "/healthz"
	resp, err := getWithRetry(client, url)
	if err != nil {
		t.Fatalf("pre-shutdown GET: %v", err)
	}
	resp.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s of cancel")
	}
}

func TestNilDepsSafety(t *testing.T) {
	s := New(Deps{}, "")
	// BuildSnapshot must not panic with a fully-empty Deps.
	snap := s.BuildSnapshot()
	if snap.T != "state" {
		t.Fatalf("snapshot.T: got %q want state", snap.T)
	}

	// A WS connect against a nil-Deps server must not panic.
	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	c, _, err := dialWS(t, wsURL+"/ws")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Read one frame (the on-connect snapshot) to prove the conn is live.
	snap2 := readSnapshot(t, c)
	if snap2.T != "state" {
		t.Fatalf("first frame t: got %q want state", snap2.T)
	}
	closeWS(c)
}
