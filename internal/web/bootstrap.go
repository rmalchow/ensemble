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
func (s *Server) handleBootstrapInfo(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Bootstrap == nil || s.deps.Bootstrap.Info == nil {
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "bootstrap unavailable")
		return
	}
	info := s.deps.Bootstrap.Info()
	if info.State == "member" {
		writeErr(w, http.StatusForbidden, codeForbidden, "node is a cluster member; bootstrap is closed")
		return
	}
	if info.ProtocolEpoch == 0 {
		info.ProtocolEpoch = adopt.ProtocolEpoch
	}
	writeJSON(w, info)
}

// handleBootstrapAdopt serves POST /bootstrap/adopt?phase={key,csr,complete} (08
// §A.2): the A.9 6-step handshake mapped to three phases. The body is capped at
// bodyLimit; an unknown phase is 400 invalid_request. Each phase dispatches to the
// adopt.Node half through the Bootstrap seam.
func (s *Server) handleBootstrapAdopt(w http.ResponseWriter, r *http.Request) {
	bd := s.deps.Bootstrap
	if bd == nil || bd.Node == nil || bd.Guard == nil {
		writeErr(w, http.StatusServiceUnavailable, codeNotReady, "bootstrap unavailable")
		return
	}
	// A member no longer adopts: bootstrap is closed (mirror of /info's 403).
	if bd.Info != nil && bd.Info().State == "member" {
		writeErr(w, http.StatusForbidden, codeForbidden, "node is a cluster member; bootstrap is closed")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, bodyLimit))
	if err != nil {
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "could not read body")
		return
	}
	src := srcAddr(r.RemoteAddr)

	switch adopt.Phase(r.URL.Query().Get("phase")) {
	case adopt.PhaseKey:
		s.bootstrapKey(w, body)
	case adopt.PhaseCSR:
		s.bootstrapCSR(w, body, src)
	case adopt.PhaseComplete:
		s.bootstrapComplete(w, body, src)
	default:
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
			writeErr(w, http.StatusUnauthorized, codeUnauthenticated, "bad PIN proof")
			return
		}
		writeErr(w, http.StatusBadRequest, codeInvalidRequest, "csr phase failed")
		return
	}
	bd.Guard.RecordSuccess(src)
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
			writeErr(w, http.StatusUnauthorized, codeUnauthenticated, "bad PIN proof")
		case errors.Is(err, adopt.ErrBadPayload):
			writeErr(w, http.StatusUnprocessableEntity, codeUnprocessable, "malformed adoption payload")
		default:
			writeErr(w, http.StatusUnprocessableEntity, codeUnprocessable, "complete phase failed")
		}
		return
	}
	// Atomic install (leaf+CA+secrets 0600 → join gossip → flip mDNS → close
	// bootstrap). On failure the node stays uninitialized (takeover atomicity).
	if bd.Install != nil {
		if err := bd.Install(inst); err != nil {
			writeErr(w, http.StatusInternalServerError, codeInternal, "install failed: "+err.Error())
			return
		}
	}
	bd.Guard.RecordSuccess(src)
	bd.Node.Drop(req.NonceA)
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
	writeErr(w, http.StatusTooManyRequests, codeRateLimited, msg)
	return false
}
