package web

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// newServerErrorLog adapts the http.Server error stream onto the Deps.Logf
// verbose sink. With no sink wired (verbose off) the server errors are
// discarded — the handlers own their request-level error reporting; the
// remaining stream is dominated by per-poll TLS handshake aborts from
// browsers that have not accepted the self-signed cert exception yet.
func newServerErrorLog(logf func(format string, args ...any)) *log.Logger {
	return log.New(logfWriter(logf), "", 0)
}

// logfWriter funnels log.Logger lines into a Deps.Logf-style closure.
type logfWriter func(format string, args ...any)

func (w logfWriter) Write(p []byte) (int, error) {
	if w != nil {
		w("%s", strings.TrimRight(string(p), "\n"))
	}
	return len(p), nil
}

// Serve runs the control plane on a PRE-BOUND listener until ctx is cancelled,
// then graceful-shuts (5 s drain). cmd owns port selection so it can advertise
// the bound port over mDNS / in cluster Meta before anything else starts (the
// mpvsync listenWeb +1-on-conflict pattern), then hands the listener here.
//
// This is the net-new TLS seam (doc 01 §3.1): if deps.TLSConfig is non-nil the
// listener is wrapped via tls.NewListener with the mTLS config that pki builds
// in a later piece. When nil (dev/test, before pki lands) the bare listener is
// served, so the skeleton is runnable without certs. Serve also starts the
// websocket hub bound to ctx.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if s.deps.TLSConfig != nil {
		if cfg := s.deps.TLSConfig(); cfg != nil {
			ln = tls.NewListener(ln, cfg)
		}
	}

	// TLS handshake failures are EXPECTED background noise on a self-signed
	// control plane: every browser poll from an origin whose certificate
	// exception has not been accepted yet (each host:port is its own origin)
	// aborts the handshake with a bad-certificate alert, once per request.
	// Route them to the verbose log sink (Deps.Logf) instead of stderr so a
	// production node does not spam its log at the SPA's poll rate; every
	// other http.Server error still reaches the same sink.
	srv := &http.Server{Handler: s.mux, ErrorLog: newServerErrorLog(s.deps.Logf)}

	go s.runHub(ctx)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
