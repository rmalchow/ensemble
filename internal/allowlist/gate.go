package allowlist

import (
	"context"
	"net"
	"net/netip"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// maxPkt is the read-buffer size for GateUDP. It must not truncate the largest
// realtime datagram (R, §5.3): the canonical audio wire frame is the §6.4 header
// (44 B) + S16LE PCM for 480 frames / 2 channels = 44 + 480*2*2 = 1964 B (A.10
// "m5", A.12); clock packets are the fixed 40-byte mpvsync format (A.1). 2048 is
// a single safe constant covering both with headroom — exact sizing is the
// clock/audio pieces' concern, the gate just must not truncate. If a future
// profile raises FramesPerChunk or rate beyond A.12 defaults, grow this.
const maxPkt = 2048

// GateUDP runs a blocking read loop over conn: it reads datagrams and, for each
// whose source IP is in set, calls deliver(src, payload); packets from
// disallowed sources are dropped silently (no reply) BEFORE deliver, so
// untrusted bytes never reach the clock/audio decoders/FEC (03 §6.3, A.8). It
// returns when conn is closed or ctx is done (the caller closes conn on ctx
// cancel to unblock the read; GateUDP also returns ctx.Err() if it observes the
// cancellation between packets).
//
// The []byte handed to deliver is only valid for the duration of the call (it
// aliases the reused read buffer); deliver must copy if it retains it. src is a
// netip.AddrPort (no per-packet *net.UDPAddr alloc on the Pi hot path, Q2); a
// consumer needing the net.UDPAddr reply form can net.UDPAddrFromAddrPort(src).
func GateUDP(ctx context.Context, conn *net.UDPConn, set *Set, deliver func(src netip.AddrPort, b []byte)) error {
	buf := make([]byte, maxPkt)
	for {
		// Cheap pre-read cancellation check so a cancelled ctx returns promptly
		// even if the caller hasn't yet closed conn.
		if err := ctx.Err(); err != nil {
			return err
		}
		n, src, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			// Conn closed (caller's ctx-cancel path) or a fatal read error.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}
		if !set.AllowedAddr(src.Addr()) {
			continue // DROP silently, no reply (A.8)
		}
		deliver(src, buf[:n]) // only now do bytes reach the decoders
	}
}

// Run is the recompute loop (07 §3.2): on every state.Store.Changed() or
// membershipChanged signal it rebuilds set from store.Get() (a deep copy, 07
// §5.1) and liveFn(). It primes the set with one rebuild before entering the
// loop so the gate is non-empty at startup (the zero Set denies all until the
// first Update). It blocks until ctx is done, then returns ctx.Err().
//
// liveFn returns the current alive member addrs (the cmd adapter maps
// cluster.Members() → []MemberAddr). The two channels are the SAME coalesced
// Changed() signals used elsewhere (state P2.3, cluster P2.1); recompute is
// cheap and rare (a few dozen IPs, human-driven config + occasional membership
// flaps), so coalescing through a single signal is sufficient (07 §3.2).
func Run(ctx context.Context, set *Set, store *state.Store,
	membershipChanged <-chan struct{}, liveFn func() []MemberAddr) error {

	storeChanged := store.Changed()

	// Prime once so the gate is populated at startup.
	set.Update(store.Get(), liveFn())

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-storeChanged: // ConfigDoc advanced (Apply or Merge), A.6
		case <-membershipChanged: // live peer set changed (P2.1 / 02 §3.1)
		}
		set.Update(store.Get(), liveFn())
	}
}
