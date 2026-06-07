package source

import (
	"net"
	"net/netip"
	"sync"
	"time"

	"ensemble/internal/stream"
)

// registry holds the live subscribers, keyed by their OBSERVED source address
// (§8.7: UDP audio flows back to the addr the HELLO came from; TCP audio rides
// the accepted conn). Guarded by Server.mu (no own lock).
type registry struct {
	subs map[netip.AddrPort]*subscriber
}

func newRegistry() registry {
	return registry{subs: make(map[netip.AddrPort]*subscriber)}
}

// subscriber is one live destination.
type subscriber struct {
	addr     netip.AddrPort // UDP: HELLO source addr; TCP: RemoteAddr
	tr       stream.Transport
	conn     net.Conn   // TCP only (nil for UDP)
	lastSeen time.Time  // last HELLO/RESTART; expiry at +ttl
	wmu      sync.Mutex // TCP: serializes writes to conn (fan-out + prime)
	dead     bool       // TCP write error observed; skip on fan-out
	priming  bool       // a prime burst is catching up to the live edge;
	//                     excluded from live fan-out until then (Server.mu)
}

// upsert records a HELLO from addr. Returns (sub, isNew): isNew is true on a
// previously-unknown addr (Connects++). Refreshes lastSeen otherwise.
func (r *registry) upsert(addr netip.AddrPort, t stream.Transport, conn net.Conn, now time.Time) (sub *subscriber, isNew bool) {
	if s, ok := r.subs[addr]; ok {
		s.lastSeen = now
		return s, false
	}
	s := &subscriber{addr: addr, tr: t, conn: conn, lastSeen: now}
	r.subs[addr] = s
	return s, true
}

// get returns the subscriber for addr (RESTART/BYE lookups), or nil.
func (r *registry) get(addr netip.AddrPort) *subscriber { return r.subs[addr] }

// remove drops a subscriber (BYE, or TCP conn error/close).
func (r *registry) remove(addr netip.AddrPort) { delete(r.subs, addr) }

// expire removes subscribers whose lastSeen < now-ttl; returns the removed TCP
// conns so the caller can close them outside the map mutation.
func (r *registry) expire(now time.Time, ttl time.Duration) []net.Conn {
	var conns []net.Conn
	for addr, s := range r.subs {
		if now.Sub(s.lastSeen) > ttl {
			if s.conn != nil {
				conns = append(conns, s.conn)
			}
			delete(r.subs, addr)
		}
	}
	return conns
}

// live returns a snapshot slice of current subscribers for fan-out. Subs with
// a prime burst in flight are excluded — the burst delivers their frames (in
// seq order, catching up via the ring) until it reaches the live edge, so the
// newcomer's reorder window and sink anchor on the OLDEST primed seq, never on
// an interleaved live frame.
func (r *registry) live() []*subscriber {
	out := make([]*subscriber, 0, len(r.subs))
	for _, s := range r.subs {
		if s.priming {
			continue
		}
		out = append(out, s)
	}
	return out
}

// count returns the current subscriber count.
func (r *registry) count() int { return len(r.subs) }
