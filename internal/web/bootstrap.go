package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/adopt"
)

// This file implements the node-side /bootstrap/* surface (08 §A) that lives
// OUTSIDE mTLS, on the self-signed fingerprint-pinned channel (03 §8/§9). It is
// the adoptee half of the A.9 handshake: GET /bootstrap/info (the unauthenticated
// probe that hands back the cert fingerprint to pin) and the three-phase POST
// /bootstrap/adopt?phase={key,csr,complete}. Structure (the io.LimitReader body
// cap, the writeJSON helper, the deps==nil → 503 guard, the error→HTTP mapping
// idiom) is adopted from media internal/web/api_local.go; the group-password
// probe/adopt bodies are dropped for the PKI phase dispatch.
//
// Guard wiring (A.12, 03 §3.4): phase=key carries no PIN, so it is gated only by
// an active lockout (the Allow check still runs to refuse a locked-out source);
// phase=csr/complete are PIN-bearing, so they call Allow first and
// RecordFail/RecordSuccess on the outcome. The source IP is logged on every
// failed proof (audit) by the caller of RecordFail here.

// handleBootstrapInfo serves GET /bootstrap/info (08 §A.1): the unauthenticated
// probe a controller reads to learn this node's id, self-signed cert fingerprint
// (to pin before the PIN), init state, protocol epoch, and caps. It returns 403
// once this node is a healthy member — bootstrap is then closed.
func (s *Server) handleBootstrapInfo(w http.ResponseWriter, r *http.Request) {
	src := srcAddr(r.RemoteAddr)
	if s.deps.Bootstrap == nil || s.deps.Bootstrap.Info == nil {
		s.logf("bootstrap/info from %s -> 503 (bootstrap seam not wired)", src)
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "bootstrap unavailable")
		return
	}
	info := s.deps.Bootstrap.Info()
	if info.State == "member" {
		s.logf("bootstrap/info from %s -> 403 (node is a member; bootstrap closed)", src)
		writeErr(w, http.StatusForbidden, codeForbidden, "node is a cluster member; bootstrap is closed")
		return
	}
	if info.ProtocolEpoch == 0 {
		info.ProtocolEpoch = adopt.ProtocolEpoch
	}
	s.logf("bootstrap/info from %s -> 200 (id=%s state=%s epoch=%d)", src, info.NodeID, info.State, info.ProtocolEpoch)
	writeJSON(w, info)
}

// handleBootstrapAdopt serves POST /bootstrap/adopt?phase={key,csr,complete} (08
// §A.2): the A.9 6-step handshake mapped to three phases. The body is capped at
// bodyLimit; an unknown phase is 400 invalid_request. Each phase dispatches to the
// adopt.Node half through the Bootstrap seam.
func (s *Server) handleBootstrapAdopt(w http.ResponseWriter, r *http.Request) {
	src := srcAddr(r.RemoteAddr)
	bd := s.deps.Bootstrap
	if bd == nil || bd.Node == nil || bd.Guard == nil {
		s.logf("bootstrap/adopt from %s -> 503 (bootstrap seam not wired)", src)
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "bootstrap unavailable")
		return
	}
	// A member no longer adopts: bootstrap is closed (mirror of /info's 403).
	if bd.Info != nil && bd.Info().State == "member" {
		s.logf("bootstrap/adopt from %s -> 403 (node is a member; bootstrap closed)", src)
		writeErr(w, http.StatusForbidden, codeForbidden, "node is a cluster member; bootstrap is closed")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, bodyLimit))
	if err != nil {
		s.logf("bootstrap/adopt from %s -> 400 (body read error: %v)", src, err)
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "could not read body")
		return
	}

	phase := adopt.Phase(r.URL.Query().Get("phase"))
	s.logf("bootstrap/adopt phase=%q from %s", phase, src)
	switch phase {
	case adopt.PhaseKey:
		s.bootstrapKey(w, body)
	case adopt.PhaseCSR:
		s.bootstrapCSR(w, body, src)
	case adopt.PhaseComplete:
		s.bootstrapComplete(w, body, src)
	default:
		s.logf("bootstrap/adopt from %s -> 400 (unknown phase %q)", src, phase)
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "unknown adopt phase")
	}
}

// bootstrapKey runs A.9 steps 2-3 (no PIN). It refuses an epoch mismatch (422,
// m7) before any PIN work; a locked-out source is still refused (429) so a
// scanning attacker cannot churn ephemerals during a lockout.
func (s *Server) bootstrapKey(w http.ResponseWriter, body []byte) {
	bd := s.deps.Bootstrap
	var req adopt.KeyReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "malformed JSON body")
		return
	}
	resp, _, err := bd.Node.BeginKey(req)
	if err != nil {
		switch {
		case errors.Is(err, adopt.ErrEpochMismatch):
			writeErr(w, http.StatusUnprocessableEntity, codeUnprocessable, "protocol epoch mismatch")
		default:
			writeErr(w, http.StatusBadRequest, codeInvalidRequest, "bad key exchange")
		}
		return
	}
	writeJSON(w, resp)
}

// bootstrapCSR runs A.9 step 5 (PIN-bearing). It calls Allow first; on a bad tag
// it reports the source to the guard (RecordFail) and returns 401; on success it
// clears the source counters.
func (s *Server) bootstrapCSR(w http.ResponseWriter, body []byte, src string) {
	bd := s.deps.Bootstrap
	if !s.guardAllow(w, src) {
		return
	}
	var req adopt.CSRReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "malformed JSON body")
		return
	}
	sess, err := bd.Node.Lookup(req.NonceA)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, codeUnauthenticated, "unknown or expired handshake")
		return
	}
	csrPEM, err := bd.CSR()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, codeInternal, "could not build CSR")
		return
	}
	resp, err := bd.Node.AcceptCSR(sess, req, csrPEM)
	if err != nil {
		if errors.Is(err, adopt.ErrBadPIN) {
			bd.Guard.RecordFail(src) // audit: failed PIN proof from src
			s.logf("bootstrap/adopt csr from %s -> 401 (bad PIN proof; recorded fail)", src)
			writeErr(w, http.StatusUnauthorized, codeUnauthenticated, "bad PIN proof")
			return
		}
		s.logf("bootstrap/adopt csr from %s -> 400 (csr phase failed: %v)", src, err)
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "csr phase failed")
		return
	}
	bd.Guard.RecordSuccess(src)
	s.logf("bootstrap/adopt csr from %s -> 200 (PIN proof OK)", src)
	writeJSON(w, resp)
}

// bootstrapComplete runs A.9 step 6 (PIN-bearing). It verifies tag2, decrypts the
// payload via Node.Complete, installs atomically via the Install hook, and on a
// good proof clears the source counters. A bad tag2 reports to the guard (401);
// an install failure leaves the node uninitialized (500).
func (s *Server) bootstrapComplete(w http.ResponseWriter, body []byte, src string) {
	bd := s.deps.Bootstrap
	if !s.guardAllow(w, src) {
		return
	}
	var req adopt.CompleteReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "malformed JSON body")
		return
	}
	sess, err := bd.Node.Lookup(req.NonceA)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, codeUnauthenticated, "unknown or expired handshake")
		return
	}
	inst, resp, err := bd.Node.Complete(sess, req)
	if err != nil {
		switch {
		case errors.Is(err, adopt.ErrBadPIN):
			bd.Guard.RecordFail(src)
			s.logf("bootstrap/adopt complete from %s -> 401 (bad PIN proof; recorded fail)", src)
			writeErr(w, http.StatusUnauthorized, codeUnauthenticated, "bad PIN proof")
		case errors.Is(err, adopt.ErrBadPayload):
			s.logf("bootstrap/adopt complete from %s -> 422 (malformed adoption payload)", src)
			writeErr(w, http.StatusUnprocessableEntity, codeUnprocessable, "malformed adoption payload")
		default:
			s.logf("bootstrap/adopt complete from %s -> 422 (complete phase failed: %v)", src, err)
			writeErr(w, http.StatusUnprocessableEntity, codeUnprocessable, "complete phase failed")
		}
		return
	}
	// Atomic install (leaf+CA+secrets 0600 → join gossip → flip mDNS → close
	// bootstrap). On failure the node stays uninitialized (takeover atomicity).
	if bd.Install != nil {
		if err := bd.Install(inst); err != nil {
			s.logf("bootstrap/adopt complete from %s -> 500 (install failed: %v); node stays uninitialized", src, err)
			writeErr(w, http.StatusInternalServerError, codeInternal, "install failed: "+err.Error())
			return
		}
	}
	bd.Guard.RecordSuccess(src)
	bd.Node.Drop(req.NonceA)
	s.logf("bootstrap/adopt complete from %s -> 200 (adoption installed; bootstrap closing)", src)
	writeJSON(w, resp)
}

// guardAllow runs the A.12 guard's Allow for a PIN-bearing phase, writing the 429
// envelope (with Retry-After) on a refusal. It returns true iff the attempt may
// proceed.
func (s *Server) guardAllow(w http.ResponseWriter, src string) bool {
	ok, retry, err := s.deps.Bootstrap.Guard.Allow(src)
	if ok {
		return true
	}
	if sec := retryAfterSeconds(retry); sec > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(sec))
	}
	msg := "too many attempts, retry later"
	if errors.Is(err, adopt.ErrLockedOut) {
		msg = "locked out, retry later"
	}
	s.logf("bootstrap/adopt from %s -> 429 (%s; retry in %ds)", src, msg, retryAfterSeconds(retry))
	writeErr(w, http.StatusTooManyRequests, codeRateLimited, msg)
	return false
}

// takeoverRequest is the POST /bootstrap/takeover body (03 §4): the CURRENT
// cluster's admin password authorizes this node's release for re-adoption.
type takeoverRequest struct {
	Password string `json:"password"`
}

// handleBootstrapTakeover serves POST /bootstrap/takeover: a foreign controller
// presents THIS node's current cluster admin password; on success the node
// self-releases (wipes its cluster state) and reopens its bootstrap surface so
// the normal A.9 adopt can run. Only meaningful on a member (409 otherwise).
// Password attempts ride the same A.12 brute-force guard as the adoption PIN.
func (s *Server) handleBootstrapTakeover(w http.ResponseWriter, r *http.Request) {
	src := srcAddr(r.RemoteAddr)
	bd := s.deps.Bootstrap
	if bd == nil || bd.Guard == nil || bd.VerifyPassword == nil || bd.Release == nil {
		s.logf("bootstrap/takeover from %s -> 503 (takeover seam not wired)", src)
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "takeover unavailable")
		return
	}
	if bd.Info != nil && bd.Info().State != "member" {
		s.logf("bootstrap/takeover from %s -> 409 (node is not a member)", src)
		writeErr(w, http.StatusConflict, codeConflict, "node is not a cluster member; use adopt")
		return
	}
	if !s.guardAllow(w, src) {
		return
	}
	var req takeoverRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, bodyLimit)).Decode(&req); err != nil {
		s.logf("bootstrap/takeover from %s -> 400 (malformed body)", src)
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "malformed JSON body")
		return
	}
	if !bd.VerifyPassword(req.Password) {
		bd.Guard.RecordFail(src) // audit: failed takeover password from src
		s.logf("bootstrap/takeover from %s -> 401 (wrong password)", src)
		writeErr(w, http.StatusUnauthorized, codeUnauthenticated, "wrong password")
		return
	}
	if err := bd.Release(); err != nil {
		s.logf("bootstrap/takeover from %s -> 500 (release failed: %v)", src, err)
		writeErr(w, http.StatusInternalServerError, codeInternal, "release failed")
		return
	}
	bd.Guard.RecordSuccess(src)
	s.logf("bootstrap/takeover from %s -> 200 (cluster released; bootstrap reopened)", src)
	writeJSON(w, struct {
		Released bool `json:"released"`
	}{Released: true})
}
