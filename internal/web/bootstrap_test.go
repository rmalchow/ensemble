package web

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/adopt"
)

// muxRunner is an adopt.PhaseRunner that drives the three phases against the web
// Server's /bootstrap/adopt handlers over the in-process mux (httptest), so the
// controller half exercises the real node-side HTTP path. It records the last
// status so guard-lockout tests can assert the 429.
type muxRunner struct {
	t          *testing.T
	s          *Server
	lastStatus int
}

func (r *muxRunner) phase(p string, body, out any) error {
	rec := doJSON(r.t, r.s, http.MethodPost, "/bootstrap/adopt?phase="+p, body, nil)
	r.lastStatus = rec.Code
	if rec.Code != http.StatusOK {
		return statusErr(rec.Code)
	}
	return json.Unmarshal(rec.Body.Bytes(), out)
}

func (r *muxRunner) Key(_ context.Context, req adopt.KeyReq) (adopt.KeyResp, error) {
	var resp adopt.KeyResp
	return resp, r.phase("key", req, &resp)
}
func (r *muxRunner) CSR(_ context.Context, req adopt.CSRReq) (adopt.CSRResp, error) {
	var resp adopt.CSRResp
	return resp, r.phase("csr", req, &resp)
}
func (r *muxRunner) Complete(_ context.Context, req adopt.CompleteReq) (adopt.CompleteResp, error) {
	var resp adopt.CompleteResp
	return resp, r.phase("complete", req, &resp)
}

type statusErr int

func (e statusErr) Error() string { return http.StatusText(int(e)) }

// bootstrapHarness builds a Server whose Bootstrap seam is a live adopt.Node +
// adopt.Guard, plus a controller + CA that drives it. installed captures the node's
// decrypted bundle.
type bootstrapHarness struct {
	s         *Server
	ctrl      *adopt.Controller
	runner    *muxRunner
	caCert    *x509.Certificate
	installed *adopt.Installed
	guard     *adopt.Guard
	state     string
}

func newBootstrapHarness(t *testing.T) *bootstrapHarness {
	t.Helper()
	_, leafKey, _ := ed25519.GenerateKey(rand.Reader)
	guard := adopt.NewGuard(adopt.DefaultGuardParams(), nil)
	node := adopt.NewNode("n-node", "0000", leafKey, guard)

	h := &bootstrapHarness{installed: new(adopt.Installed), guard: guard, state: "uninitialized"}
	bd := &BootstrapDeps{
		Node:  node,
		Guard: guard,
		CSR: func() ([]byte, error) {
			der, err := x509.CreateCertificateRequest(rand.Reader,
				&x509.CertificateRequest{Subject: pkix.Name{CommonName: "n-node"}}, leafKey)
			if err != nil {
				return nil, err
			}
			return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
		},
		Install: func(inst adopt.Installed) error { *h.installed = inst; return nil },
		Info: func() BootstrapInfo {
			return BootstrapInfo{NodeID: "n-node", Name: "ensemble-node",
				Fingerprint: "sha256:deadbeef", State: h.state, SoftwareVersion: "0.1.0",
				ProtocolEpoch: adopt.ProtocolEpoch, Caps: Capabilities{Render: true, MaxRate: 48000}}
		},
	}
	h.s = New(Deps{NodeID: "n-node", Bootstrap: bd}, "")

	// Controller + CA.
	_, caKey, _ := ed25519.GenerateKey(rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caKey.Public(), caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	h.caCert = caCert
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	sign := func(csrPEM []byte, nodeID string, addrs []net.IP) ([]byte, error) {
		block, _ := pem.Decode(csrPEM)
		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			return nil, err
		}
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: nodeID},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
			IPAddresses: addrs, KeyUsage: x509.KeyUsageDigitalSignature}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, csr.PublicKey, caKey)
		if err != nil {
			return nil, err
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
	}
	h.ctrl = &adopt.Controller{Sign: sign, CABundle: caPEM,
		Secrets: adopt.ClusterSecrets{CAKeyPEM: []byte("CAKEY"), GossipKey: make([]byte, 32)}, ClusterName: "home"}
	h.runner = &muxRunner{t: t, s: h.s}
	return h
}

func TestBootstrapInfoShape(t *testing.T) {
	h := newBootstrapHarness(t)
	rec := doJSON(t, h.s, http.MethodGet, "/bootstrap/info", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("info status = %d, want 200", rec.Code)
	}
	var info BootstrapInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.NodeID != "n-node" || info.ProtocolEpoch != 1 {
		t.Errorf("info = %+v", info)
	}
	if info.Fingerprint == "" {
		t.Error("missing fingerprint")
	}

	// Once a member, bootstrap is closed: 403.
	h.state = "member"
	rec = doJSON(t, h.s, http.MethodGet, "/bootstrap/info", nil, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member info status = %d, want 403", rec.Code)
	}
}

func TestBootstrapAdoptPhases(t *testing.T) {
	h := newBootstrapHarness(t)

	// Full happy-path through the real HTTP handlers.
	if _, err := h.ctrl.Run(context.Background(), h.runner, "0000", "n-node", "Bedroom",
		"uninitialized", []net.IP{net.ParseIP("192.168.1.9")}, false); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(h.installed.Secrets.CAKeyPEM) != "CAKEY" {
		t.Errorf("node did not install the expected secrets: %+v", h.installed.Secrets)
	}

	// Unknown phase -> 400 invalid_request.
	rec := doJSON(t, h.s, http.MethodPost, "/bootstrap/adopt?phase=bogus", map[string]string{}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown phase status = %d, want 400", rec.Code)
	}
}

func TestBootstrapAdoptWrongPIN(t *testing.T) {
	h := newBootstrapHarness(t)
	// Controller PIN "0001" != node "0000": phase=csr -> 401.
	err := mustRunErr(t, h.ctrl, h.runner, "0001")
	if err == nil {
		t.Fatal("wrong PIN accepted")
	}
	if se, ok := err.(statusErr); !ok || int(se) != http.StatusUnauthorized {
		t.Fatalf("err = %v, want 401 statusErr", err)
	}
}

func TestBootstrapAdoptGuardLockout(t *testing.T) {
	h := newBootstrapHarness(t)
	// Drive enough wrong-PIN attempts to trip the soft backoff (3 consecutive).
	for i := 0; i < 3; i++ {
		_ = mustRunErr(t, h.ctrl, h.runner, "9999")
	}
	// The next PIN-bearing phase is refused with 429 even before the tag check.
	// Run a fresh key phase then a csr that should hit the guard.
	priv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	nonceB := make([]byte, 16)
	rand.Read(nonceB)
	var kr adopt.KeyResp
	if err := h.runner.phase("key", adopt.KeyReq{PubB: priv.PublicKey().Bytes(), NonceB: nonceB, Epoch: 1}, &kr); err != nil {
		t.Fatalf("key phase during lockout: %v", err) // key is not PIN-gated by soft backoff
	}
	var cr adopt.CSRResp
	err := h.runner.phase("csr", adopt.CSRReq{NonceA: kr.NonceA, Tag: make([]byte, 32)}, &cr)
	if se, ok := err.(statusErr); !ok || int(se) != http.StatusTooManyRequests {
		t.Fatalf("csr during backoff err = %v, want 429", err)
	}

	// /bootstrap/info still answers 200 during the backoff.
	rec := doJSON(t, h.s, http.MethodGet, "/bootstrap/info", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("info during lockout = %d, want 200", rec.Code)
	}
}

func TestBootstrapAdoptEpochMismatch(t *testing.T) {
	h := newBootstrapHarness(t)
	priv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	nonceB := make([]byte, 16)
	rand.Read(nonceB)
	var kr adopt.KeyResp
	err := h.runner.phase("key", adopt.KeyReq{PubB: priv.PublicKey().Bytes(), NonceB: nonceB, Epoch: 2}, &kr)
	if se, ok := err.(statusErr); !ok || int(se) != http.StatusUnprocessableEntity {
		t.Fatalf("epoch mismatch err = %v, want 422", err)
	}
}

func TestBootstrapUnavailable(t *testing.T) {
	s := New(Deps{NodeID: "n-x"}, "") // no Bootstrap seam
	rec := doJSON(t, s, http.MethodGet, "/bootstrap/info", nil, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("info without seam = %d, want 503", rec.Code)
	}
	rec = doJSON(t, s, http.MethodPost, "/bootstrap/adopt?phase=key", map[string]string{}, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("adopt without seam = %d, want 503", rec.Code)
	}
}

func mustRunErr(t *testing.T, ctrl *adopt.Controller, runner *muxRunner, pin string) error {
	t.Helper()
	_, err := ctrl.Run(context.Background(), runner, pin, "n-node", "B", "uninitialized", nil, false)
	return err
}
