package adopt

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"testing"
)

// TestHKDFDeterministic asserts hkdfKey is a pure function of (ikm, salt, info):
// identical inputs yield identical 32-byte keys; a changed label yields a
// different key (domain separation of k vs kp).
func TestHKDFDeterministic(t *testing.T) {
	ikm := bytes.Repeat([]byte{0x42}, 32)
	salt := []byte("nonceA||nonceB")
	a := hkdfKey(ikm, salt, InfoAdopt)
	b := hkdfKey(ikm, salt, InfoAdopt)
	if !bytes.Equal(a, b) {
		t.Fatal("HKDF not deterministic for identical inputs")
	}
	if len(a) != derivedKeyLen {
		t.Fatalf("key len = %d, want %d", len(a), derivedKeyLen)
	}
	if bytes.Equal(a, hkdfKey(ikm, salt, InfoPIN)) {
		t.Fatal("InfoAdopt and InfoPIN produce the same key (no domain separation)")
	}
}

// TestECDHSharedSecretMatches asserts both halves derive the same k/kp/transcript
// from mirrored derive/deriveController calls (the core handshake invariant).
func TestECDHSharedSecretMatches(t *testing.T) {
	nA := bytes.Repeat([]byte{1}, 16)
	nB := bytes.Repeat([]byte{2}, 16)
	const pin = "0000"

	nodePriv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	ctrlPriv, _ := ecdh.X25519().GenerateKey(rand.Reader)

	kN, kpN, tN, err := derive(nodePriv, ctrlPriv.PublicKey(), pin, nA, nB)
	if err != nil {
		t.Fatalf("node derive: %v", err)
	}
	kC, kpC, tC, err := deriveController(ctrlPriv, nodePriv.PublicKey(), pin, nA, nB)
	if err != nil {
		t.Fatalf("controller derive: %v", err)
	}
	if !bytes.Equal(kN, kC) {
		t.Error("confidentiality key k differs between halves")
	}
	if !bytes.Equal(kpN, kpC) {
		t.Error("PIN key kp differs between halves")
	}
	if !bytes.Equal(tN, tC) {
		t.Error("transcript differs between halves")
	}
	// Transcript ordering is NA‖NB‖nonceA‖nonceB (the node's publics first).
	want := concat(nodePriv.PublicKey().Bytes(), ctrlPriv.PublicKey().Bytes(), nA, nB)
	if !bytes.Equal(tN, want) {
		t.Error("transcript not NA‖NB‖nonceA‖nonceB")
	}
}

// TestHMACTagDomainSeparation asserts the "req" and "done" tags differ under the
// same kp/transcript, and that verifyTag is exact.
func TestHMACTagDomainSeparation(t *testing.T) {
	kp := bytes.Repeat([]byte{7}, 32)
	tr := []byte("transcript")
	req := tagFor(kp, tr, tagReq)
	done := tagFor(kp, tr, tagDone)
	if bytes.Equal(req, done) {
		t.Fatal("req and done tags collide")
	}
	if !verifyTag(kp, tr, tagReq, req) {
		t.Fatal("verifyTag rejected a valid req tag")
	}
	bad := append([]byte(nil), req...)
	bad[0] ^= 1
	if verifyTag(kp, tr, tagReq, bad) {
		t.Fatal("verifyTag accepted a tampered tag")
	}
}

// TestAEADRoundTrip asserts seal/open round-trips under k and fails under a wrong
// key or a flipped ciphertext byte.
func TestAEADRoundTrip(t *testing.T) {
	k := bytes.Repeat([]byte{0x11}, derivedKeyLen)
	msg := []byte("leaf||ca||secrets")
	ct, err := seal(k, aeadNonceComplete, msg)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	pt, err := open(k, aeadNonceComplete, ct)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(pt, msg) {
		t.Fatal("round-trip mismatch")
	}
	wrong := bytes.Repeat([]byte{0x22}, derivedKeyLen)
	if _, err := open(wrong, aeadNonceComplete, ct); err == nil {
		t.Fatal("open under wrong key succeeded")
	}
	bad := append([]byte(nil), ct...)
	bad[0] ^= 1
	if _, err := open(k, aeadNonceComplete, bad); err == nil {
		t.Fatal("open of tampered ciphertext succeeded")
	}
}

// TestAEADNoncesDistinct asserts the csr and complete directions never reuse the
// (k, nonce) pair — the open-question-#2 resolution.
func TestAEADNoncesDistinct(t *testing.T) {
	if bytes.Equal(aeadNonceCSR, aeadNonceComplete) {
		t.Fatal("csr and complete AEAD nonces are identical (reuse under one k)")
	}
	if len(aeadNonceCSR) != 12 || len(aeadNonceComplete) != 12 {
		t.Fatal("AEAD nonce length must be 12")
	}
}

// TestPayloadFraming round-trips packPayload/unpackPayload and rejects truncation.
func TestPayloadFraming(t *testing.T) {
	leaf := []byte("LEAF-PEM")
	ca := []byte("CA-PEM")
	secrets := []byte(`{"caKeyPem":"AA==","gossipKey":"BB=="}`)

	packed := packPayload(leaf, ca, secrets)
	gotLeaf, gotCA, gotSec, err := unpackPayload(packed)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if !bytes.Equal(gotLeaf, leaf) || !bytes.Equal(gotCA, ca) || !bytes.Equal(gotSec, secrets) {
		t.Fatal("framing round-trip mismatch")
	}
	// Truncated buffer -> ErrBadPayload.
	if _, _, _, err := unpackPayload(packed[:len(packed)-1]); err != ErrBadPayload {
		t.Fatalf("truncated err = %v, want ErrBadPayload", err)
	}
	// Trailing garbage -> ErrBadPayload.
	if _, _, _, err := unpackPayload(append(packed, 0xFF)); err != ErrBadPayload {
		t.Fatalf("trailing-garbage err = %v, want ErrBadPayload", err)
	}
}
