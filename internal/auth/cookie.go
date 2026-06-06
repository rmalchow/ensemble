package auth

import (
	"net/http"
	"time"
)

// SessionCookieName is the name of the session cookie (03 §7.2 / 08 §B.2).
const SessionCookieName = "ensemble_session"

// Session TTLs (03 §7.2): 12 h sliding (idle) and a 7 d absolute cap. Exported as
// IdleTTL/AbsoluteTTL so the web layer can render the login response's absolute
// expiresAt (08 §B.2) without duplicating the constant.
const (
	IdleTTL     = 12 * time.Hour
	AbsoluteTTL = 7 * 24 * time.Hour

	idleTTL     = IdleTTL
	absoluteTTL = AbsoluteTTL
)

// SetSessionCookie writes the session cookie with the 03 §7.2 attributes:
// HttpOnly, Secure, SameSite=Strict, Path=/, MaxAge from the idle TTL. The
// control plane is always TLS (mTLS, D10) so Secure holds unconditionally.
func SetSessionCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   int(idleTTL / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

// ClearSessionCookie expires the session cookie on logout (MaxAge=-1), keeping
// the rest of the attributes so the browser matches and deletes it.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}
