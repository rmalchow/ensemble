package adopt

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"
)

// --- test fixtures -----------------------------------------------------------

// newLeafKey returns a fresh Ed25519 signer (the node leaf key, never on the wire).
func newLeafKey(t *testing.T) crypto.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen leaf key: %v", err)
	}
	return key
}

// csrPEMFor builds a self-signed CSR PEM for key (the pki.NewCSR shape, inlined so
// the adopt unit tests stay free of the pki import per the layering rule).
func csrPEMFor(t *testing.T, key crypto.Signer, nodeID string) []byte {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: nodeID},
	}, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

// fakeCA is a minimal in-test CA so the controller's SignFunc returns a real leaf
// that chains to it — without importing internal/pki.
type fakeCA struct {
	cert *x509.Certificate
	key  crypto.Signer
}

func newFakeCA(t *testing.T) *fakeCA {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatalf("self-sign CA: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &fakeCA{cert: cert, key: key}
}

func (ca *fakeCA) certPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}

// sign mimics pki.CA.Sign: SANs from authenticated nodeID/addrs, not the CSR.
func (ca *fakeCA) sign(csrPEM []byte, nodeID string, addrs []net.IP) ([]byte, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		return nil, errors.New("bad CSR PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: nodeID},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IPAddresses:  addrs,
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// inProcRunner is a PhaseRunner wired directly to a Node, so the handshake can be
// driven without HTTP. It tracks the per-handshake session the Node hands back at
// phase=key and threads it into csr/complete (the HTTP transport re-Looks it up by
// nonceA; here we keep the pointer for directness).
type inProcRunner struct {
	t       *testing.T
	node    *Node
	leafKey crypto.Signer
	sess    *NodeSession

	// hooks let a test corrupt a request mid-flight (tamper tests).
	mutateCSR      func(*CSRReq)
	mutateComplete func(*CompleteReq)
}

func (r *inProcRunner) Key(_ context.Context, req KeyReq) (KeyResp, error) {
	resp, sess, err := r.node.BeginKey(req)
	if err != nil {
		return KeyResp{}, err
	}
	r.sess = sess
	return resp, nil
}

func (r *inProcRunner) CSR(_ context.Context, req CSRReq) (CSRResp, error) {
	if r.mutateCSR != nil {
		r.mutateCSR(&req)
	}
	sess, err := r.node.Lookup(req.NonceA)
	if err != nil {
		return CSRResp{}, err
	}
	csrPEM := csrPEMFor(r.t, r.leafKey, r.node.nodeID)
	return r.node.AcceptCSR(sess, req, csrPEM)
}

func (r *inProcRunner) Complete(_ context.Context, req CompleteReq) (CompleteResp, error) {
	if r.mutateComplete != nil {
		r.mutateComplete(&req)
	}
	sess, err := r.node.Lookup(req.NonceA)
	if err != nil {
		return CompleteResp{}, err
	}
	_, resp, err := r.node.Complete(sess, req)
	if err != nil {
		return CompleteResp{}, err
	}
	r.node.Drop(req.NonceA)
	return resp, nil
}

// harness builds a node + controller + in-process runner sharing one guard.
func harness(t *testing.T, nodePIN, ctrlPIN string) (*Node, *Controller, *inProcRunner, *fakeCA) {
	t.Helper()
	g := NewGuard(DefaultGuardParams(), nil)
	leafKey := newLeafKey(t)
	node := NewNode("n-test", nodePIN, leafKey, g)
	ca := newFakeCA(t)
	ctrl := &Controller{
		Sign:        ca.sign,
		CABundle:    ca.certPEM(),
		Secrets:     ClusterSecrets{CAKeyPEM: []byte("CA-KEY-PEM"), GossipKey: bytes.Repeat([]byte{7}, 32)},
		ClusterName: "home",
	}
	runner := &inProcRunner{t: t, node: node, leafKey: leafKey}
	_ = ctrlPIN
	return node, ctrl, runner, ca
}

// --- handshake tests ---------------------------------------------------------

func TestHandshakeHappyPath(t *testing.T) {
	node, ctrl, runner, ca := harness(t, "0000", "0000")

	seed, err := ctrl.Run(context.Background(), runner, "0000", "n-test", "Bedroom", "uninitialized",
		[]net.IP{net.ParseIP("192.168.1.55")}, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if seed.ID != "n-test" || seed.Name != "Bedroom" {
		t.Fatalf("seed id/name = %q/%q", seed.ID, seed.Name)
	}
	// The signed leaf must chain to the test CA.
	block, _ := pem.Decode(seed.CertPEM)
	if block == nil {
		t.Fatal("no leaf PEM in seed")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		t.Fatalf("leaf does not chain to CA: %v", err)
	}

	// The node must have decrypted the same secrets the controller sealed: replay
	// the complete by hand against a fresh session to assert Installed contents.
	_ = node
}

func TestInstalledBundleMatches(t *testing.T) {
	// Drive the handshake and capture what the node's Complete decrypts by using a
	// runner that returns the Installed result.
	g := NewGuard(DefaultGuardParams(), nil)
	leafKey := newLeafKey(t)
	node := NewNode("n-x", "0000", leafKey, g)
	ca := newFakeCA(t)
	ctrl := &Controller{Sign: ca.sign, CABundle: ca.certPEM(),
		Secrets: ClusterSecrets{CAKeyPEM: []byte("KEYPEM"), GossipKey: bytes.Repeat([]byte{9}, 32)}, ClusterName: "home"}

	var got Installed
	runner := &captureRunner{t: t, node: node, leafKey: leafKey, installed: &got}
	if _, err := ctrl.Run(context.Background(), runner, "0000", "n-x", "Kitchen", "uninitialized", nil, false); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !bytes.Equal(got.Secrets.CAKeyPEM, []byte("KEYPEM")) {
		t.Errorf("CAKeyPEM = %q", got.Secrets.CAKeyPEM)
	}
	if !bytes.Equal(got.Secrets.GossipKey, bytes.Repeat([]byte{9}, 32)) {
		t.Errorf("GossipKey mismatch")
	}
	if !bytes.Equal(got.CABundlePEM, ca.certPEM()) {
		t.Errorf("CABundle mismatch")
	}
	if got.ClusterName != "home" || got.NodeID != "n-x" {
		t.Errorf("cluster/node = %q/%q", got.ClusterName, got.NodeID)
	}
}

func TestWrongPINRejected(t *testing.T) {
	_, ctrl, runner, _ := harness(t, "0000", "0001")
	// Controller uses the wrong PIN; the node holds "0000". phase=csr -> ErrBadPIN.
	_, err := ctrl.Run(context.Background(), runner, "0001", "n-test", "B", "uninitialized", nil, false)
	if !errors.Is(err, ErrBadPIN) {
		t.Fatalf("err = %v, want ErrBadPIN", err)
	}
}

func TestEpochMismatchAbortsBeforePIN(t *testing.T) {
	g := NewGuard(DefaultGuardParams(), nil)
	node := NewNode("n-test", "0000", newLeafKey(t), g)
	priv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	_, _, err := node.BeginKey(KeyReq{PubB: priv.PublicKey().Bytes(), NonceB: bytes.Repeat([]byte{1}, 16), Epoch: 2})
	if !errors.Is(err, ErrEpochMismatch) {
		t.Fatalf("err = %v, want ErrEpochMismatch", err)
	}
}

func TestReplayRejected(t *testing.T) {
	g := NewGuard(DefaultGuardParams(), nil)
	leafKey := newLeafKey(t)
	node := NewNode("n-test", "0000", leafKey, g)

	priv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	resp, _, err := node.BeginKey(KeyReq{PubB: priv.PublicKey().Bytes(), NonceB: bytes.Repeat([]byte{2}, 16), Epoch: 1})
	if err != nil {
		t.Fatalf("BeginKey: %v", err)
	}
	// A captured nonceA that was never registered as a session (simulate replay of
	// a stale/foreign nonce): Lookup must reject it.
	if _, err := node.Lookup([]byte("not-a-real-nonce")); !errors.Is(err, ErrSessionUnknown) {
		t.Fatalf("Lookup unknown nonce err = %v, want ErrSessionUnknown", err)
	}
	// The legit session is found once.
	if _, err := node.Lookup(resp.NonceA); err != nil {
		t.Fatalf("Lookup live session: %v", err)
	}
	// After Complete drops it, a replay of the same nonceA finds nothing.
	node.Drop(resp.NonceA)
	if _, err := node.Lookup(resp.NonceA); !errors.Is(err, ErrSessionUnknown) {
		t.Fatalf("Lookup after Drop err = %v, want ErrSessionUnknown", err)
	}
}

func TestPassiveEavesdropperCannotDecrypt(t *testing.T) {
	// Capture the wire (NA,NB,nonceA,nonceB,encPayload) but NOT Z: AEAD Open with a
	// random/independent key fails — confidentiality rests on ECDH, not the PIN.
	g := NewGuard(DefaultGuardParams(), nil)
	leafKey := newLeafKey(t)
	node := NewNode("n-x", "0000", leafKey, g)
	ca := newFakeCA(t)
	ctrl := &Controller{Sign: ca.sign, CABundle: ca.certPEM(),
		Secrets: ClusterSecrets{CAKeyPEM: []byte("SECRET")}, ClusterName: "home"}

	cap := &eavesRunner{t: t, node: node, leafKey: leafKey}
	if _, err := ctrl.Run(context.Background(), runner2(cap), "0000", "n-x", "B", "uninitialized", nil, false); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cap.encPayload) == 0 {
		t.Fatal("no encPayload captured")
	}
	// An eavesdropper without Z cannot derive k; a wrong key fails to Open.
	wrong := bytes.Repeat([]byte{0xAB}, derivedKeyLen)
	if _, err := open(wrong, aeadNonceComplete, cap.encPayload); err == nil {
		t.Fatal("AEAD opened under a wrong key (confidentiality broken)")
	}
}

func TestTamperedTagFails(t *testing.T) {
	// Flip one bit of the csr-phase tag: the node's const-time verify must fail.
	_, ctrl, runner, _ := harness(t, "0000", "0000")
	runner.mutateCSR = func(r *CSRReq) {
		if len(r.Tag) > 0 {
			r.Tag[0] ^= 0x01
		}
	}
	if _, err := ctrl.Run(context.Background(), runner, "0000", "n-test", "B", "uninitialized", nil, false); !errors.Is(err, ErrBadPIN) {
		t.Fatalf("tampered tag err = %v, want ErrBadPIN", err)
	}

	// Flip one bit of the complete-phase ciphertext: AEAD Open must fail.
	_, ctrl2, runner2, _ := harness(t, "0000", "0000")
	runner2.mutateComplete = func(r *CompleteReq) {
		if len(r.EncPayload) > 0 {
			r.EncPayload[0] ^= 0x01
		}
	}
	err := mustRunErr(t, ctrl2, runner2)
	if err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}

func TestTamperedTag2Fails(t *testing.T) {
	_, ctrl, runner, _ := harness(t, "0000", "0000")
	runner.mutateComplete = func(r *CompleteReq) {
		if len(r.Tag2) > 0 {
			r.Tag2[0] ^= 0x01
		}
	}
	if err := mustRunErr(t, ctrl, runner); !errors.Is(err, ErrBadPIN) {
		t.Fatalf("tampered tag2 err = %v, want ErrBadPIN", err)
	}
}

func TestForeignTargetNeedsForce(t *testing.T) {
	_, ctrl, runner, _ := harness(t, "0000", "0000")
	if _, err := ctrl.Run(context.Background(), runner, "0000", "n-test", "B", "foreign", nil, false); !errors.Is(err, ErrForeign) {
		t.Fatalf("foreign force=false err = %v, want ErrForeign", err)
	}
	// force=true proceeds (takeover) — fresh harness because the first call aborted
	// before issuing a nonce.
	_, ctrl2, runner2, _ := harness(t, "0000", "0000")
	if _, err := ctrl2.Run(context.Background(), runner2, "0000", "n-test", "B", "foreign", nil, true); err != nil {
		t.Fatalf("foreign force=true: %v", err)
	}
}

func mustRunErr(t *testing.T, ctrl *Controller, runner *inProcRunner) error {
	t.Helper()
	_, err := ctrl.Run(context.Background(), runner, "0000", "n-test", "B", "uninitialized", nil, false)
	return err
}

// captureRunner records the Installed bundle the node decrypted in Complete.
type captureRunner struct {
	t         *testing.T
	node      *Node
	leafKey   crypto.Signer
	installed *Installed
}

func (r *captureRunner) Key(_ context.Context, req KeyReq) (KeyResp, error) {
	resp, _, err := r.node.BeginKey(req)
	return resp, err
}
func (r *captureRunner) CSR(_ context.Context, req CSRReq) (CSRResp, error) {
	sess, err := r.node.Lookup(req.NonceA)
	if err != nil {
		return CSRResp{}, err
	}
	return r.node.AcceptCSR(sess, req, csrPEMFor(r.t, r.leafKey, r.node.nodeID))
}
func (r *captureRunner) Complete(_ context.Context, req CompleteReq) (CompleteResp, error) {
	sess, err := r.node.Lookup(req.NonceA)
	if err != nil {
		return CompleteResp{}, err
	}
	inst, resp, err := r.node.Complete(sess, req)
	if err == nil {
		*r.installed = inst
	}
	return resp, err
}

// eavesRunner records the complete-phase ciphertext off the wire.
type eavesRunner struct {
	t          *testing.T
	node       *Node
	leafKey    crypto.Signer
	encPayload []byte
}

func runner2(e *eavesRunner) PhaseRunner { return e }

func (r *eavesRunner) Key(_ context.Context, req KeyReq) (KeyResp, error) {
	resp, _, err := r.node.BeginKey(req)
	return resp, err
}
func (r *eavesRunner) CSR(_ context.Context, req CSRReq) (CSRResp, error) {
	sess, err := r.node.Lookup(req.NonceA)
	if err != nil {
		return CSRResp{}, err
	}
	return r.node.AcceptCSR(sess, req, csrPEMFor(r.t, r.leafKey, r.node.nodeID))
}
func (r *eavesRunner) Complete(_ context.Context, req CompleteReq) (CompleteResp, error) {
	r.encPayload = append([]byte(nil), req.EncPayload...)
	sess, err := r.node.Lookup(req.NonceA)
	if err != nil {
		return CompleteResp{}, err
	}
	_, resp, err := r.node.Complete(sess, req)
	return resp, err
}
