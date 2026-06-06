package web

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"time"
)

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

	srv := &http.Server{Handler: s.mux}

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
