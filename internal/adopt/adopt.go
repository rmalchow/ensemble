// Package adopt implements the A.9 bootstrap adoption handshake: the
// transport-agnostic node (adoptee) and controller halves plus the A.12 online
// brute-force guard. It runs on the plaintext-but-fingerprint-pinned self-signed
// bootstrap channel that lives OUTSIDE mTLS (03 §8, §9).
//
// The handshake (A.9, 03 §2.3) combines:
//   - X25519 ECDH (crypto/ecdh) for confidentiality of the secret bundle,
//   - HKDF-SHA256 (x/crypto/hkdf) deriving the confidentiality key k from the
//     ECDH shared secret Z and the PIN-keyed auth key kp from the PIN,
//   - HMAC-SHA256 (crypto/hmac) over the transcript NA‖NB‖nonceA‖nonceB for
//     authentication, with domain suffixes "req"/"done",
//   - ChaCha20-Poly1305 (x/crypto/chacha20poly1305) AEAD for the CSR and the
//     signed leaf‖CA‖clusterSecrets payload.
//
// Confidentiality rests on Z (ECDH), NOT the PIN (03 §2.5): a passive
// eavesdropper without Z cannot decrypt the bundle. The PIN-keyed HMAC tags only
// authenticate (defeat MITM / wrong PIN). The package imports ONLY stdlib and
// x/crypto (01 §2): it never imports web, state, cluster, pki or group, and is
// reached from both halves over byte slices, a crypto.Signer (the node leaf key)
// and a SignFunc callback so it stays decoupled and unit-testable in isolation.
package adopt

import (
	"context"
	"crypto"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// Phase identifies the bootstrap sub-exchange (03 §9: key | csr | complete).
type Phase string

const (
	PhaseKey      Phase = "key"      // A.9 steps 2-3: swap ECDH publics + nonces
	PhaseCSR      Phase = "csr"      // A.9 step 5: PIN-tagged csr_request -> AEAD_k(csrPem)
	PhaseComplete Phase = "complete" // A.9 step 6: AEAD_k(leaf‖caBundle‖clusterSecrets) + tag2
)

// HKDF info labels and HMAC domain suffixes — pinned, normative (A.9 step 4).
const (
	InfoAdopt = "ensemble-adopt-v1" // k  = HKDF(Z,   nonceA‖nonceB, InfoAdopt)
	InfoPIN   = "ensemble-pin-v1"   // kp = HKDF(PIN, nonceA‖nonceB, InfoPIN)
	tagReq    = "req"               // HMAC(kp, transcript‖tagReq)
	tagDone   = "done"              // HMAC(kp, transcript‖tagDone)
)

// ProtocolEpoch is fixed at 1 for this build: all nodes in a cluster run the same
// epoch; adoption refuses a mismatch before any PIN work (m7, 03 §2.7).
const ProtocolEpoch = 1

// derivedKeyLen is the HKDF output length for both k and kp: 32 bytes, the
// ChaCha20-Poly1305 key size.
const derivedKeyLen = chacha20poly1305.KeySize

// AEAD nonces are FIXED per direction under a single-use key k (open question #2,
// resolved in-spec): each handshake derives a fresh k from fresh ECDH ephemerals
// + dual random nonces, and only two messages are ever sealed under it — the CSR
// (csr direction) and the complete payload (complete direction). Distinct 12-byte
// nonces (…01 vs …02) guarantee non-reuse of the (k, nonce) pair across the two.
var (
	aeadNonceCSR      = aeadNonce(1)
	aeadNonceComplete = aeadNonce(2)
)

func aeadNonce(last byte) []byte {
	n := make([]byte, chacha20poly1305.NonceSize)
	n[chacha20poly1305.NonceSize-1] = last
	return n
}

// ---- wire bodies (JSON; base64 for byte fields) -----------------------------

// KeyReq is POST /bootstrap/adopt?phase=key body (controller -> node). A.9 step 2.
type KeyReq struct {
	PubB   []byte `json:"NB"`            // controller X25519 ephemeral public (32 B)
	NonceB []byte `json:"nonceB"`        // 16 B random, single-use
	Epoch  int    `json:"protocolEpoch"` // must equal ProtocolEpoch (m7)
}

// KeyResp is the node's reply. A.9 step 3.
type KeyResp struct {
	PubA   []byte `json:"NA"`     // node X25519 ephemeral public (32 B)
	NonceA []byte `json:"nonceA"` // 16 B random, single-use
}

// CSRReq is POST …?phase=csr body (controller -> node). A.9 step 5.
type CSRReq struct {
	NonceA []byte `json:"nonceA"` // session key (the node's nonce from phase=key)
	Tag    []byte `json:"tag"`    // HMAC(kp, transcript‖"req")
}

// CSRResp returns the AEAD-wrapped CSR (node -> controller).
type CSRResp struct {
	EncCSR []byte `json:"encCsr"` // ChaCha20-Poly1305 seal under k of csrPem
}

// CompleteReq is POST …?phase=complete body (controller -> node). A.9 step 6.
type CompleteReq struct {
	NonceA         []byte   `json:"nonceA"`     // session key
	EncPayload     []byte   `json:"encPayload"` // AEAD_k(leafPem‖caBundlePem‖clusterSecretsJSON)
	Tag2           []byte   `json:"tag2"`       // HMAC(kp, transcript‖"done")
	AssignedNodeID string   `json:"assignedNodeId"`
	ClusterName    string   `json:"clusterName"`
	SeedPeers      []string `json:"seedPeers"`
}

// CompleteResp is the node's final ack (-> controller). 08 §A.2 200 body.
type CompleteResp struct {
	NodeID string `json:"nodeId"`
	State  string `json:"state"` // "member"
}

// ClusterSecrets is the plaintext-replicated secret bundle delivered in step 6
// (03 §1.6). Schema/persistence owned by P2.3/state; mirrored here for the AEAD
// payload so adopt stays decoupled from state.
type ClusterSecrets struct {
	CAKeyPEM  []byte `json:"caKeyPem"`  // CA private key PEM (D18: plaintext at rest)
	GossipKey []byte `json:"gossipKey"` // 32 B memberlist SecretKey
}

// Installed is the verified, decrypted result for the node to persist (P2.3
// writes it). Complete returns it before sending its 200 ack.
type Installed struct {
	LeafPEM     []byte
	CABundlePEM []byte
	Secrets     ClusterSecrets
	SeedPeers   []string
	ClusterName string
	NodeID      string
}

// ---- shared crypto helpers --------------------------------------------------

// derive computes Z = X25519(priv, peerPub) and the two HKDF keys + transcript
// shared by both halves (A.9 step 4). It is the single place the KDF labels and
// transcript ordering live so the node and controller cannot drift.
func derive(priv *ecdh.PrivateKey, peerPub *ecdh.PublicKey, pin string, nA, nB []byte) (k, kp, transcript []byte, err error) {
	z, err := priv.ECDH(peerPub)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("adopt: ECDH: %w", err)
	}
	salt := concat(nA, nB)
	k = hkdfKey(z, salt, InfoAdopt)
	kp = hkdfKey([]byte(pin), salt, InfoPIN)
	transcript = concat(priv.PublicKey().Bytes(), peerPub.Bytes(), nA, nB)
	return k, kp, transcript, nil
}

// deriveController mirrors derive from the controller's viewpoint, where NA is
// the peer (node) public and NB is its own. The transcript MUST be NA‖NB‖… in the
// node's ordering regardless of which side computes it, so it is passed the
// explicit node/controller publics.
func deriveController(priv *ecdh.PrivateKey, nodePub *ecdh.PublicKey, pin string, nA, nB []byte) (k, kp, transcript []byte, err error) {
	z, err := priv.ECDH(nodePub)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("adopt: ECDH: %w", err)
	}
	salt := concat(nA, nB)
	k = hkdfKey(z, salt, InfoAdopt)
	kp = hkdfKey([]byte(pin), salt, InfoPIN)
	transcript = concat(nodePub.Bytes(), priv.PublicKey().Bytes(), nA, nB)
	return k, kp, transcript, nil
}

// hkdfKey reads a derivedKeyLen key from HKDF-SHA256(ikm, salt, info). HKDF over
// SHA-256 cannot fail for a 32-byte output, so a short read can only mean a
// programming error; we panic rather than silently return a weak key.
func hkdfKey(ikm, salt []byte, info string) []byte {
	r := hkdf.New(sha256.New, ikm, salt, []byte(info))
	out := make([]byte, derivedKeyLen)
	if _, err := io.ReadFull(r, out); err != nil {
		panic("adopt: HKDF read failed: " + err.Error())
	}
	return out
}

// tagFor computes the PIN-keyed HMAC-SHA256 over transcript‖suffix.
func tagFor(kp, transcript []byte, suffix string) []byte {
	mac := hmac.New(sha256.New, kp)
	mac.Write(transcript)
	mac.Write([]byte(suffix))
	return mac.Sum(nil)
}

// verifyTag constant-time compares a received tag against the expected one. It is
// the media controller.go:116 subtle.ConstantTimeCompare idiom (03 §2.3 ref).
func verifyTag(kp, transcript []byte, suffix string, got []byte) bool {
	want := tagFor(kp, transcript, suffix)
	return subtle.ConstantTimeCompare(want, got) == 1
}

// seal/open ChaCha20-Poly1305 under k with a fixed per-direction nonce (safe
// because k is single-use per handshake — see aeadNonceCSR/Complete).
func seal(k, nonce, plaintext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(k)
	if err != nil {
		return nil, fmt.Errorf("adopt: aead init: %w", err)
	}
	return aead.Seal(nil, nonce, plaintext, nil), nil
}

func open(k, nonce, ciphertext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(k)
	if err != nil {
		return nil, fmt.Errorf("adopt: aead init: %w", err)
	}
	pt, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("adopt: aead open: %w", err)
	}
	return pt, nil
}

// concat returns the concatenation of the parts (fresh buffer).
func concat(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// packPayload frames the step-6 bundle so Complete parses it unambiguously: each
// segment is prefixed with a 4-byte big-endian length
// (leafLen‖leaf‖caLen‖ca‖secretsLen‖secretsJSON), 03 §9 framing detail.
func packPayload(leaf, ca, secretsJSON []byte) []byte {
	out := make([]byte, 0, 12+len(leaf)+len(ca)+len(secretsJSON))
	out = appendSeg(out, leaf)
	out = appendSeg(out, ca)
	out = appendSeg(out, secretsJSON)
	return out
}

func appendSeg(dst, seg []byte) []byte {
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(seg)))
	dst = append(dst, l[:]...)
	return append(dst, seg...)
}

// unpackPayload reverses packPayload. ErrBadPayload on any length-prefix that
// runs past the buffer.
func unpackPayload(buf []byte) (leaf, ca, secretsJSON []byte, err error) {
	if leaf, buf, err = readSeg(buf); err != nil {
		return nil, nil, nil, err
	}
	if ca, buf, err = readSeg(buf); err != nil {
		return nil, nil, nil, err
	}
	if secretsJSON, buf, err = readSeg(buf); err != nil {
		return nil, nil, nil, err
	}
	if len(buf) != 0 {
		return nil, nil, nil, ErrBadPayload
	}
	return leaf, ca, secretsJSON, nil
}

func readSeg(buf []byte) (seg, rest []byte, err error) {
	if len(buf) < 4 {
		return nil, nil, ErrBadPayload
	}
	n := binary.BigEndian.Uint32(buf[:4])
	buf = buf[4:]
	if uint32(len(buf)) < n {
		return nil, nil, ErrBadPayload
	}
	return buf[:n], buf[n:], nil
}

// nonceKey is the map key for a session: the base64 of nonceA (its identity).
func nonceKey(nonceA []byte) string {
	return string(nonceA)
}

// ---- node side (the adoptee) ------------------------------------------------

// NodeSession holds the per-handshake state on the uninitialized node across the
// three phases, keyed by nonceA. Single-use; the node prunes it after NonceTTL.
type NodeSession struct {
	priv       *ecdh.PrivateKey
	k          []byte
	kp         []byte
	transcript []byte
	born       time.Time
}

// Node is the adoptee's handshake half. PIN is the local secret (default "0000",
// auth.DefaultPIN). leafKey is this node's Ed25519 leaf key (P1.1, never leaves
// the node); nonces is the single-use nonce store (the auth.AdoptionGuard adapter
// in production, an adopt.Guard in tests).
type Node struct {
	nodeID  string
	pin     string
	leafKey crypto.Signer
	nonces  NonceStore
	now     func() time.Time

	sessions sessionStore
}

// NewNode builds the node half. now defaults to time.Now.
func NewNode(nodeID, pin string, leafKey crypto.Signer, nonces NonceStore) *Node {
	return &Node{
		nodeID:   nodeID,
		pin:      pin,
		leafKey:  leafKey,
		nonces:   nonces,
		now:      time.Now,
		sessions: newSessionStore(),
	}
}

// BeginKey runs A.9 steps 3-4 for the node: it refuses an epoch mismatch BEFORE
// any PIN work (m7), mints (nA,NA)+nonceA, derives k,kp,transcript, stores a
// NodeSession keyed by nonceA, and returns KeyResp{NA,nonceA}.
func (n *Node) BeginKey(req KeyReq) (KeyResp, *NodeSession, error) {
	// m7: refuse a protocol-epoch mismatch before touching the PIN (03 §2.7).
	if req.Epoch != ProtocolEpoch {
		return KeyResp{}, nil, ErrEpochMismatch
	}
	peerPub, err := ecdh.X25519().NewPublicKey(req.PubB)
	if err != nil {
		return KeyResp{}, nil, fmt.Errorf("adopt: bad controller public: %w", err)
	}
	if len(req.NonceB) == 0 {
		return KeyResp{}, nil, errors.New("adopt: missing nonceB")
	}
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return KeyResp{}, nil, fmt.Errorf("adopt: gen ephemeral: %w", err)
	}
	nonceA := n.nonces.IssueNonce()
	if len(nonceA) == 0 {
		return KeyResp{}, nil, errors.New("adopt: nonce issue failed")
	}

	k, kp, transcript, err := derive(priv, peerPub, n.pin, nonceA, req.NonceB)
	if err != nil {
		return KeyResp{}, nil, err
	}
	sess := &NodeSession{priv: priv, k: k, kp: kp, transcript: transcript, born: n.now()}
	n.sessions.put(nonceKey(nonceA), sess)

	return KeyResp{PubA: priv.PublicKey().Bytes(), NonceA: nonceA}, sess, nil
}

// AcceptCSR runs A.9 step 5: it verifies tag (constant-time) against the session's
// kp/transcript, then builds and AEAD-seals this node's CSR under k. A bad tag
// returns ErrBadPIN and seals NOTHING; the handler reports the source to the guard.
// The supplied csrPEM is built by the caller via pki.NewCSR (the engine stays
// decoupled from pki).
func (n *Node) AcceptCSR(sess *NodeSession, req CSRReq, csrPEM []byte) (CSRResp, error) {
	if sess == nil {
		return CSRResp{}, ErrSessionUnknown
	}
	if !verifyTag(sess.kp, sess.transcript, tagReq, req.Tag) {
		return CSRResp{}, ErrBadPIN
	}
	enc, err := seal(sess.k, aeadNonceCSR, csrPEM)
	if err != nil {
		return CSRResp{}, err
	}
	return CSRResp{EncCSR: enc}, nil
}

// Complete runs A.9 step 6: it verifies tag2 (constant-time), opens AEAD_k(payload),
// splits leaf‖caBundle‖clusterSecrets, and returns the verified Installed bundle
// plus the 200 ack. The actual atomic install (P2.3 write, mDNS flip, gossip join)
// is the handler's job — this only authenticates + decrypts (takeover §4 atomicity
// is owned by the persistence layer).
func (n *Node) Complete(sess *NodeSession, req CompleteReq) (Installed, CompleteResp, error) {
	if sess == nil {
		return Installed{}, CompleteResp{}, ErrSessionUnknown
	}
	if !verifyTag(sess.kp, sess.transcript, tagDone, req.Tag2) {
		return Installed{}, CompleteResp{}, ErrBadPIN
	}
	plain, err := open(sess.k, aeadNonceComplete, req.EncPayload)
	if err != nil {
		return Installed{}, CompleteResp{}, err
	}
	leaf, ca, secretsJSON, err := unpackPayload(plain)
	if err != nil {
		return Installed{}, CompleteResp{}, err
	}
	var secrets ClusterSecrets
	if err := json.Unmarshal(secretsJSON, &secrets); err != nil {
		return Installed{}, CompleteResp{}, ErrBadPayload
	}

	nodeID := req.AssignedNodeID
	if nodeID == "" {
		nodeID = n.nodeID
	}
	inst := Installed{
		LeafPEM:     leaf,
		CABundlePEM: ca,
		Secrets:     secrets,
		SeedPeers:   req.SeedPeers,
		ClusterName: req.ClusterName,
		NodeID:      nodeID,
	}
	return inst, CompleteResp{NodeID: nodeID, State: "member"}, nil
}

// Lookup resolves the NodeSession for a phase=csr/complete request by its nonceA.
// It returns ErrSessionUnknown if no live session matches (expired, replayed, or
// phase out of order). The session is NOT consumed here — the guard's single-use
// nonce burn (at phase=key issue / handler-side consume) governs replay; the
// session is dropped on Complete or by TTL pruning.
func (n *Node) Lookup(nonceA []byte) (*NodeSession, error) {
	if len(nonceA) == 0 {
		return nil, ErrSessionUnknown
	}
	n.sessions.prune(n.now(), NonceSessionTTL)
	sess := n.sessions.get(nonceKey(nonceA))
	if sess == nil {
		return nil, ErrSessionUnknown
	}
	return sess, nil
}

// Drop removes a finished session (called after a successful Complete).
func (n *Node) Drop(nonceA []byte) {
	n.sessions.del(nonceKey(nonceA))
}

// NonceSessionTTL bounds how long a half-finished NodeSession lingers. It matches
// the A.12 nonce TTL (30 s) so a handshake that stalls is reaped on the same clock
// as its nonce.
const NonceSessionTTL = 30 * time.Second

// ---- controller side (holds the CA, Model B) --------------------------------

// SignFunc signs a CSR with the cluster CA (pki.CA.Sign bound by cmd; 30-day leaf,
// SANs set from the AUTHENTICATED nodeID + observed addrs, never from the CSR).
type SignFunc func(csrPEM []byte, nodeID string, addrs []net.IP) (leafPEM []byte, err error)

// PhaseRunner is the transport the Controller drives the three phases over. The
// production implementation (web/api_cluster.go) POSTs to the target's
// /bootstrap/adopt over a fingerprint-pinned self-signed TLS channel; tests pass
// an in-process runner wired straight to a Node.
type PhaseRunner interface {
	Key(ctx context.Context, req KeyReq) (KeyResp, error)
	CSR(ctx context.Context, req CSRReq) (CSRResp, error)
	Complete(ctx context.Context, req CompleteReq) (CompleteResp, error)
}

// Controller drives the A.9 exchange against a target. Sign is the CA signing
// callback; caBundle is the public CA cert PEM (ConfigDoc.Cluster); secrets is the
// replicated ClusterSecrets projection (P2.3); clusterName is the cluster's name.
type Controller struct {
	Sign        SignFunc
	CABundle    []byte
	Secrets     ClusterSecrets
	ClusterName string
}

// NodeRecordSeed is what the controller learned to write into ConfigDoc.Nodes
// (the public projection; the controller then does the If-Match config write via
// Deps/state).
type NodeRecordSeed struct {
	ID      string
	Name    string
	Addrs   []net.IP
	CertPEM []byte // the leaf the controller just signed
}

// Run executes the full A.9 exchange against the target over runner. It mints the
// controller ephemeral + nonceB, drives phase=key, derives the mirror k/kp/
// transcript, sends the PIN-tagged csr_request, opens the returned CSR ciphertext,
// signs it, seals leaf‖CA‖secrets, sends phase=complete with tag2, and returns the
// NodeRecordSeed (assigned id, name, observed addrs, signed leaf) for the caller's
// ConfigDoc write. nodeState gates a foreign target: "foreign" + force=false ->
// ErrForeign (use takeover, 03 §4).
func (c *Controller) Run(ctx context.Context, runner PhaseRunner, pin, assignedNodeID, name, nodeState string, addrs []net.IP, force bool) (NodeRecordSeed, error) {
	if nodeState == "foreign" && !force {
		return NodeRecordSeed{}, ErrForeign
	}

	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return NodeRecordSeed{}, fmt.Errorf("adopt: gen ephemeral: %w", err)
	}
	nonceB := make([]byte, nonceBytes)
	if _, err := rand.Read(nonceB); err != nil {
		return NodeRecordSeed{}, fmt.Errorf("adopt: gen nonceB: %w", err)
	}

	// Phase key.
	kr, err := runner.Key(ctx, KeyReq{PubB: priv.PublicKey().Bytes(), NonceB: nonceB, Epoch: ProtocolEpoch})
	if err != nil {
		return NodeRecordSeed{}, err
	}
	nodePub, err := ecdh.X25519().NewPublicKey(kr.PubA)
	if err != nil {
		return NodeRecordSeed{}, fmt.Errorf("adopt: bad node public: %w", err)
	}
	k, kp, transcript, err := deriveController(priv, nodePub, pin, kr.NonceA, nonceB)
	if err != nil {
		return NodeRecordSeed{}, err
	}

	// Phase csr: PIN-tagged request -> AEAD_k(csrPem).
	csrResp, err := runner.CSR(ctx, CSRReq{NonceA: kr.NonceA, Tag: tagFor(kp, transcript, tagReq)})
	if err != nil {
		return NodeRecordSeed{}, err
	}
	csrPEM, err := open(k, aeadNonceCSR, csrResp.EncCSR)
	if err != nil {
		return NodeRecordSeed{}, err
	}

	// Sign on the controller (Model B). SANs from authenticated id + observed addrs.
	leafPEM, err := c.Sign(csrPEM, assignedNodeID, addrs)
	if err != nil {
		return NodeRecordSeed{}, fmt.Errorf("adopt: sign CSR: %w", err)
	}

	// Phase complete: seal leaf‖CA‖secrets, authenticate with tag2.
	secretsJSON, err := json.Marshal(c.Secrets)
	if err != nil {
		return NodeRecordSeed{}, fmt.Errorf("adopt: marshal secrets: %w", err)
	}
	enc, err := seal(k, aeadNonceComplete, packPayload(leafPEM, c.CABundle, secretsJSON))
	if err != nil {
		return NodeRecordSeed{}, err
	}
	if _, err := runner.Complete(ctx, CompleteReq{
		NonceA:         kr.NonceA,
		EncPayload:     enc,
		Tag2:           tagFor(kp, transcript, tagDone),
		AssignedNodeID: assignedNodeID,
		ClusterName:    c.ClusterName,
		SeedPeers:      nil, // filled by the caller's seed projection if any
	}); err != nil {
		return NodeRecordSeed{}, err
	}

	return NodeRecordSeed{ID: assignedNodeID, Name: name, Addrs: addrs, CertPEM: leafPEM}, nil
}
