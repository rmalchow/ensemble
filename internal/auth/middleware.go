package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"runtime/debug"
	"strings"
)

// AuthMethod is the authenticated principal class attached to the request
// context (03 §7.4).
type AuthMethod string

const (
	MethodNode    AuthMethod = "node"
	MethodSession AuthMethod = "session"
	MethodAPIKey  AuthMethod = "apiKey"
)

// Canonical error codes (08 §0.4).
const (
	codeNotReady        = "not_ready"       // 503: uninitialized gate
	codeUnauthenticated = "unauthenticated" // 401: no valid human credential
	codeForbidden       = "forbidden"       // 403: authenticated but not authorized
	codeInternal        = "internal"        // 500: panic recovered
)

// ctxKey is the unexported context-key type for this package's request values.
type ctxKey int

const (
	ctxMethod ctxKey = iota
	ctxNodeID
	ctxKeyID
)

// Deps are the injected seams the chain needs. It imports no pki/state/web
// concretes: the node/cert check is a closure (pki.PeerVerifier-backed) and the
// API-key check is a closure (bound to state.APIKeys via auth.VerifyAPIKey).
type Deps struct {
	// Initialized reports whether this node has a cluster (CA + admin hash). A
	// false result triggers the uninitialized gate [1]. Nil => treated as true
	// (initialized) so the chain is usable in narrow tests.
	Initialized func() bool

	// NodeAuth authenticates the mTLS node path [2]: the client leaf chains to
	// the cluster CA AND its fingerprint is not revoked. Returns the node id on
	// success. Nil => no node path (human-only).
	NodeAuth func(r *http.Request) (nodeID string, ok bool)

	// Sessions is the human session store [3]. Nil => no session path.
	Sessions *Sessions

	// VerifyKey authenticates a Bearer API key [3], returning the matching key
	// id. Nil => no API-key path.
	VerifyKey func(plaintext string) (id string, ok bool)

	// PublicPaths are request paths reachable in the uninitialized state and
	// without a human credential (they MINT credentials): the setup wizard and
	// login. /bootstrap/* lives outside this chain entirely. A request whose
	// path is in this set skips [1] refusal and [3] human auth. If nil, the
	// pinned defaults (DefaultPublicPaths) are used.
	PublicPaths map[string]bool

	// ProbePaths are auth-OPTIONAL read paths (08 §B.4 "who am I"): they still
	// honor the [1] uninitialized gate (503) and still attach the method when a
	// credential IS presented, but an anonymous request passes through with no
	// method instead of 401 — the handler answers authenticated:false. This is
	// what lets the SPA boot probe distinguish "not logged in" (render the login
	// form) from "unreachable" (render the error card).
	ProbePaths map[string]bool
}

// DefaultPublicPaths are the chain's bootstrap-of-the-human-path exceptions
// (03 §7.4 / 7.5): setup mints the admin credential, login mints a session.
// /bootstrap/* is handled by a separate (PIN-gated) server, not this chain.
var DefaultPublicPaths = map[string]bool{
	"/api/v1/setup": true,
	"/api/v1/login": true,
}

// isPublic reports whether r's path is an unauthenticated, credential-minting
// exception.
func (d Deps) isPublic(r *http.Request) bool {
	paths := d.PublicPaths
	if paths == nil {
		paths = DefaultPublicPaths
	}
	return paths[r.URL.Path]
}

// Authenticate runs steps [1]-[3] of 03 §7.4 — uninitialized gate, node path,
// human path — and on success stores the AuthMethod (plus node/key id) in the
// request context. On failure it writes the JSON error envelope with the correct
// status and returns without calling next.
func (d Deps) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		public := d.isPublic(r)

		// [1] Uninitialized gate: with no CA / no admin hash, allow only the
		// public credential-minting endpoints; everything else => 503.
		if d.Initialized != nil && !d.Initialized() {
			if !public {
				WriteError(w, http.StatusServiceUnavailable, codeNotReady,
					"node not initialized")
				return
			}
			// Public + uninitialized (e.g. POST /setup) proceeds with no method.
			next.ServeHTTP(w, r)
			return
		}

		// [2] Node path: an mTLS peer short-circuits the human checks.
		if d.NodeAuth != nil {
			if nodeID, ok := d.NodeAuth(r); ok {
				ctx := context.WithValue(r.Context(), ctxMethod, MethodNode)
				ctx = context.WithValue(ctx, ctxNodeID, nodeID)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		// Public human-path exceptions (login) proceed un-authenticated: they
		// mint the credential the rest of the chain would require.
		if public {
			next.ServeHTTP(w, r)
			return
		}

		// [3] Human path: a valid session cookie, OR a valid Bearer API key.
		if d.Sessions != nil {
			if c, err := r.Cookie(SessionCookieName); err == nil && d.Sessions.Validate(c.Value) {
				ctx := context.WithValue(r.Context(), ctxMethod, MethodSession)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}
		if d.VerifyKey != nil {
			if key, ok := bearerToken(r); ok {
				if id, ok := d.VerifyKey(key); ok {
					ctx := context.WithValue(r.Context(), ctxMethod, MethodAPIKey)
					ctx = context.WithValue(ctx, ctxKeyID, id)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}
		}

		// Auth-optional probe paths proceed anonymously (no method in context);
		// the handler reports authenticated:false instead of this 401.
		if d.ProbePaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}

		WriteError(w, http.StatusUnauthorized, codeUnauthenticated,
			"authentication required")
	})
}

// RequireAdminSession is step [4] AuthZ: it gates admin-session-only endpoints
// (mint/list API keys, 08 §B.5/B.6). A node cert authenticates but does NOT
// authorize these, so anything other than method:"session" gets 403.
func RequireAdminSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m, ok := MethodFromContext(r.Context()); ok && m == MethodSession {
			next.ServeHTTP(w, r)
			return
		}
		WriteError(w, http.StatusForbidden, codeForbidden,
			"admin session required")
	})
}

// Chain composes [0] recover+log, then [1-3] Authenticate, then the per-endpoint
// [4]/[5] wrappers in order. The returned middleware wraps a handler so the
// outermost layer ([0]) runs first.
func Chain(d Deps, mw ...func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		// Apply the per-endpoint wrappers innermost-last so mw[0] runs before
		// mw[1] for a given request.
		for i := len(mw) - 1; i >= 0; i-- {
			if mw[i] != nil {
				h = mw[i](h)
			}
		}
		h = d.Authenticate(h)
		return recoverLog(h)
	}
}

// recoverLog is step [0]: it logs the request and converts a handler panic into
// a 500 error envelope instead of crashing the process.
func recoverLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("auth: panic serving %s %s: %v\n%s",
					r.Method, r.URL.Path, rec, debug.Stack())
				WriteError(w, http.StatusInternalServerError, codeInternal,
					"internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// MethodFromContext returns the AuthMethod stored by Authenticate, if any.
func MethodFromContext(ctx context.Context) (AuthMethod, bool) {
	m, ok := ctx.Value(ctxMethod).(AuthMethod)
	return m, ok
}

// NodeIDFromContext returns the node id stored for a method:"node" request.
func NodeIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxNodeID).(string)
	return id, ok
}

// KeyIDFromContext returns the API-key id stored for a method:"apiKey" request.
func KeyIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKeyID).(string)
	return id, ok
}

// errorEnvelope is the locked non-2xx response shape (README §6.6, 08 §0.4).
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteError emits the canonical envelope {"error":{"code","message"}} with the
// given HTTP status.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// Best-effort: the connection may already be gone; nothing to do on error.
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: errorBody{Code: code, Message: message}})
}

// bearerToken extracts a "Bearer <token>" Authorization header, if present.
func bearerToken(r *http.Request) (token string, ok bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// constantTimeEqualString compares two strings in constant time relative to the
// longer input, used for secret material (e.g. the default PIN).
func constantTimeEqualString(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
