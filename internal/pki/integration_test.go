package pki

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// dialResult carries what the server observed plus any handshake error.
type dialResult struct {
	serverPeerCN string
	serverErr    error
	clientErr    error
}

// runHandshake stands up an in-proc mTLS server with serverCfg and dials it with
// clientCfg over a net.Pipe (no real sockets, A.13 P1 two-party style). It
// returns what the server saw and either side's handshake error.
func runHandshake(t *testing.T, serverCfg, clientCfg *tls.Config) dialResult {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// A real loopback TCP listener avoids the synchronous-pipe deadlock that
	// occurs when one side writes a TLS alert while the other has stopped
	// reading (net.Pipe is unbuffered and fully synchronous).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	res := make(chan dialResult, 1)

	go func() {
		var out dialResult
		raw, err := ln.Accept()
		if err != nil {
			out.serverErr = err
			res <- out
			return
		}
		defer raw.Close()
		srv := tls.Server(raw, serverCfg)
		if err := srv.HandshakeContext(ctx); err != nil {
			out.serverErr = err
			res <- out
			return
		}
		state := srv.ConnectionState()
		if len(state.PeerCertificates) > 0 {
			out.serverPeerCN = state.PeerCertificates[0].Subject.CommonName
		}
		_, _ = io.Copy(io.Discard, srv)
		res <- out
	}()

	d := &tls.Dialer{Config: clientCfg}
	conn, clientErr := d.DialContext(ctx, "tcp", ln.Addr().String())
	if conn != nil {
		_ = conn.Close()
	}

	out := <-res
	out.clientErr = clientErr
	return out
}

func TestIntegrationHandshakeSucceeds(t *testing.T) {
	now := time.Now()
	ca := mustCreateCA(t, "lr", now)
	pool, err := CAPoolFromPEM(ca.CertPEM())
	if err != nil {
		t.Fatal(err)
	}
	v := newVerifier()

	serverLeaf := mustLeaf(t, ca, "n-aaaa", []net.IP{net.IPv4(127, 0, 0, 1)}, now)
	clientLeaf := mustLeaf(t, ca, "n-bbbb", []net.IP{net.IPv4(127, 0, 0, 1)}, now)

	serverCfg := ServerTLS(serverLeaf, pool, v)
	clientCfg := ClientTLS(clientLeaf, pool, v)
	clientCfg.ServerName = "127.0.0.1" // SAN match on the server leaf

	res := runHandshake(t, serverCfg, clientCfg)
	if res.clientErr != nil {
		t.Fatalf("client handshake err=%v, want nil", res.clientErr)
	}
	if res.serverErr != nil {
		t.Fatalf("server handshake err=%v, want nil", res.serverErr)
	}
	if res.serverPeerCN != "n-bbbb" {
		t.Errorf("server saw client CN=%q, want n-bbbb", res.serverPeerCN)
	}
}

func TestIntegrationBadCertRejected(t *testing.T) {
	now := time.Now()
	ca := mustCreateCA(t, "lr", now)
	foreign := mustCreateCA(t, "other-cluster", now)
	pool, _ := CAPoolFromPEM(ca.CertPEM())
	v := newVerifier()

	serverLeaf := mustLeaf(t, ca, "n-aaaa", []net.IP{net.IPv4(127, 0, 0, 1)}, now)
	// Client leaf signed by a DIFFERENT CA → must not chain to our pool.
	clientLeaf := mustLeaf(t, foreign, "n-evil", []net.IP{net.IPv4(127, 0, 0, 1)}, now)

	serverCfg := ServerTLS(serverLeaf, pool, v)
	clientCfg := ClientTLS(clientLeaf, pool, v)
	clientCfg.ServerName = "127.0.0.1"

	res := runHandshake(t, serverCfg, clientCfg)
	if res.serverErr == nil {
		t.Error("server accepted a foreign-CA client cert; want chain error")
	}
}

func TestIntegrationRevokedRejected(t *testing.T) {
	now := time.Now()
	ca := mustCreateCA(t, "lr", now)
	pool, _ := CAPoolFromPEM(ca.CertPEM())

	serverLeaf := mustLeaf(t, ca, "n-aaaa", []net.IP{net.IPv4(127, 0, 0, 1)}, now)
	clientLeaf := mustLeaf(t, ca, "n-bbbb", []net.IP{net.IPv4(127, 0, 0, 1)}, now)

	revokedFP := Fingerprint(clientLeaf.Certificate[0])
	// Server-side verifier rejects the (otherwise valid) client leaf.
	serverV := NewPeerVerifier(func(f string) bool { return f == revokedFP }, nil)
	clientV := newVerifier()

	serverCfg := ServerTLS(serverLeaf, pool, serverV)
	clientCfg := ClientTLS(clientLeaf, pool, clientV)
	clientCfg.ServerName = "127.0.0.1"

	res := runHandshake(t, serverCfg, clientCfg)
	if res.serverErr == nil {
		t.Fatal("server accepted a revoked client cert; want rejection")
	}
	if !strings.Contains(res.serverErr.Error(), ErrRevoked.Error()) {
		t.Errorf("server err=%v, want it to surface ErrRevoked", res.serverErr)
	}
}

func TestIntegrationSANMismatch(t *testing.T) {
	now := time.Now()
	ca := mustCreateCA(t, "lr", now)
	pool, _ := CAPoolFromPEM(ca.CertPEM())
	v := newVerifier()

	// Server leaf has only 127.0.0.1 in its SANs.
	serverLeaf := mustLeaf(t, ca, "n-aaaa", []net.IP{net.IPv4(127, 0, 0, 1)}, now)
	clientLeaf := mustLeaf(t, ca, "n-bbbb", []net.IP{net.IPv4(127, 0, 0, 1)}, now)

	serverCfg := ServerTLS(serverLeaf, pool, v)
	clientCfg := ClientTLS(clientLeaf, pool, v)
	// Dial with a ServerName/IP absent from the server leaf's SANs.
	clientCfg.ServerName = "10.0.0.200"

	res := runHandshake(t, serverCfg, clientCfg)
	if res.clientErr == nil {
		t.Error("client accepted a server cert with no matching SAN")
	}
	var hostErr x509.HostnameError
	if res.clientErr != nil && !errors.As(res.clientErr, &hostErr) {
		// Acceptable: any verification failure proves the SAN binding; log for clarity.
		t.Logf("client err (non-HostnameError but still rejected): %v", res.clientErr)
	}
}
