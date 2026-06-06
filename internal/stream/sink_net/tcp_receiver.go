package sink_net

// tcp_receiver.go (P7.1) is the TCP-fallback read half (05 §5.9, D2). It runs the
// SAME post-unwrap pipeline as the UDP receiver — allowlist gate, streamGen
// gate (flush + re-prime on a newer gen), keyframe-first prime, decode, push by
// sampleIndex — but DROPS the FEC.Recover and the reorder/dedupe window: TCP
// delivers every byte exactly once and in order, so there is nothing to recover
// or reorder (05 §5.9 "the same pipeline minus FEC and minus UDP reordering").
//
// The listener is still allowlisted by peer IP (03 §6.1 "and the TCP-fallback
// audio listener"): a connection whose RemoteAddr().IP is not in the
// allowlist.Set is closed at Accept without serving a single frame.
//
// Layering: this file adds no import beyond what receiver.go already pulls
// (net, context, allowlist, wire) — it imports no FEC because the TCP path needs
// none.

import (
	"context"
	"errors"
	"net"
	"net/netip"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/streamgen"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/wire"
)

// RunTCP listens for follower connections on ln (the audio-plane TCP socket,
// :9100) and serves the length-prefixed stream until ctx is cancelled (05 §5.9).
// It is the Transport==TCP analogue of Run; the group engine selects one based on
// Config.Transport. Each accepted connection is allowlisted by peer IP, then
// deframed and fed through the gen-gated, keyframe-first decode path.
//
// Exactly one origin connects per follower (the master fans out one TCP stream
// per listener), so RunTCP serves connections serially: a single master stream
// at a time is the steady state. A second concurrent connection (e.g. a stale
// master that has not yet been torn down) is served after the first closes;
// streamGen fencing drops its stale-generation packets regardless.
func (r *Receiver) RunTCP(ctx context.Context, ln *net.TCPListener) error {
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, err := ln.AcceptTCP()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if errors.Is(err, net.ErrClosed) {
				return ctx.Err()
			}
			continue // transient accept error
		}
		r.serveConn(ctx, conn)
	}
}

// serveConn allowlists the peer then deframes + decodes its stream until EOF or
// ctx cancellation. A non-allowlisted peer is closed immediately without serving
// a frame (03 §6.1). The connection is closed on return.
func (r *Receiver) serveConn(ctx context.Context, conn *net.TCPConn) {
	defer conn.Close()

	peer := peerIP(conn.RemoteAddr())
	if !r.allow.AllowedAddr(peer) {
		return // DROP: source IP not in the allowlist (03 §6.1)
	}

	// Close the conn on ctx cancellation so the blocking deframe read unblocks.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	fr := newFrameReader(conn)
	for {
		pkt, err := fr.next()
		if err != nil {
			return // io.EOF (clean), cancellation, or a corrupt/oversize frame
		}
		r.handleStream(pkt)
		if ctx.Err() != nil {
			return
		}
	}
}

// handleStream runs the per-packet pipeline for a TCP-delivered (in-order, loss-
// free) frame: unwrap → streamGen gate → keyframe-first → decode → push. It is the
// TCP twin of handle (receiver.go) with FEC.Recover and the reorder/dedupe window
// removed. Exposed (lowercase) for the table tests to feed deframed packets
// without a live socket.
func (r *Receiver) handleStream(buf []byte) {
	// unwrap + structural validation (magic/version/payloadLen).
	hdr, payload, err := wire.Unmarshal(buf)
	if err != nil {
		return // bad packet — dropped without panic
	}

	// streamGen gate (05 §5.8 / §5.9 "still honors streamGen"): a newer gen
	// flushes + re-primes; a lower gen is a stale straggler (a not-yet-torn-down
	// master) and is dropped.
	switch r.gate.Accept(hdr.StreamGen) {
	case streamgen.Adopt:
		r.flushAndReprime()
		r.metaMu.Lock()
		r.genSnap = r.gate.Current()
		r.metaMu.Unlock()
	case streamgen.Drop:
		return
	}

	// keyframe-first: after a (re)prime, hold non-keyframe chunks until the first
	// keyframe of the (new) generation lands (05 §5.6.4). PCM is always a keyframe.
	if r.awaitKeyframe {
		if !hdr.Flags.Keyframe() {
			return
		}
		r.awaitKeyframe = false
	}

	// decode → push by sampleIndex. TCP is in-order and loss-free, so there is no
	// gap to conceal and no reorder window to drain — push directly.
	r.deliver(wire.Packet{Header: hdr, Payload: payload})
}

// peerIP extracts the netip.Addr from a net.Addr (a *net.TCPAddr at accept time),
// unmapped, for the allowlist lookup. An unparseable address yields the zero
// Addr, which AllowedAddr denies.
func peerIP(a net.Addr) netip.Addr {
	if ta, ok := a.(*net.TCPAddr); ok {
		if addr, ok := netip.AddrFromSlice(ta.IP); ok {
			return addr.Unmap()
		}
	}
	return netip.Addr{}
}
