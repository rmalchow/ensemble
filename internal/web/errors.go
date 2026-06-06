package web

import (
	"encoding/json"
	"net/http"
)

// This file owns the JSON response writers for the /api/v1 surface: the success
// encoder (writeJSON, the idiom adopted from media internal/web/api_local.go) and
// the canonical error-envelope writer (writeErr, 08 §0.4). Keeping both here means
// every handler emits the locked shape {"error":{"code","message"}} for non-2xx
// and a charset-tagged application/json for 2xx, with no per-handler boilerplate.

// Canonical error codes (08 §0.4). These mirror the strings the SPA and the
// cross-node proxy switch on; never invent new ones in a handler.
const (
	codeInvalidRequest  = "invalid_request"       // 400 malformed JSON / failed validation
	codeUnauthenticated = "unauthenticated"       // 401 no/invalid credential
	codeForbidden       = "forbidden"             // 403 authenticated but not permitted
	codeNotFound        = "not_found"             // 404 unknown id
	codeVersionConflict = "version_conflict"      // 409 If-Match mismatch
	codeConflict        = "conflict"              // 409 non-version state conflict
	codePreconditionReq = "precondition_required" // 412 mutating call without If-Match
	codeUnprocessable   = "unprocessable"         // 422 semantically invalid
	codeRateLimited     = "rate_limited"          // 429 brute-force guard
	codeProxyFailed     = "proxy_failed"          // 502 cross-node proxy unreachable/rejected
	codeNotReady        = "not_ready"             // 503 uninitialized / no cluster
	codeInternal        = "internal"              // 500 unexpected
)

// writeJSON encodes v as application/json with a 200 status. Adopted verbatim
// from media internal/web/api_local.go (the writeJSON idiom), extended only with
// the charset tag the SPA expects. Encode errors are best-effort: the connection
// may already be gone, and there is nothing useful to report mid-body.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONStatus is writeJSON with an explicit status code (e.g. 201 Created).
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// errorEnvelope is the locked non-2xx response shape (README §6.6 / 08 §0.4).
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeErr emits the canonical envelope {"error":{"code","message"}} with the
// given HTTP status (08 §0.4). It is the single error path for every /api/v1
// handler so the wire shape stays uniform.
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: errorBody{Code: code, Message: msg}})
}
