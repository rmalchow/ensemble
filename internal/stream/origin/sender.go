package origin

import (
	"net"
	"sync"
)

// sender owns the per-listener unicast destination registry and the fan-out write
// (05 §5.6.1 / D5). It reproduces the fan-out-to-a-destination-map shape from
// media internal/sync.BeaconServer.Publish (cited, not copied), but the
// destinations come from AddListener/RemoveListener (the group engine), the
// payload is a marshaled wire packet, and there is one socket per listener (D5).
//
// Per D5 the contract is "unicast per listener". We dial one connected
// *net.UDPConn per listener so the kernel routes by the connected peer and Write
// is a single copy into the socket; codec.Encode and fec.Protect have already run
// once for the bundle (05 §5.2.3), so only this Write is O(listeners).
//
// The registry is mutex-guarded: AddListener/RemoveListener run on the group
// engine goroutine while fanOut runs on the origin Run goroutine.
type sender struct {
	mu        sync.Mutex
	listeners map[string]*listener
	// dial builds the per-listener connection; injectable so tests can capture
	// writes without real sockets. Defaults to dialUDP.
	dial func(addr *net.UDPAddr) (packetWriter, error)
}

// packetWriter is the minimal write side of a listener connection (a connected
// *net.UDPConn in production, a fake in tests). It is closed on RemoveListener.
type packetWriter interface {
	Write(b []byte) (int, error)
	Close() error
}

// listener is one registered unicast destination.
type listener struct {
	addr     *net.UDPAddr
	conn     packetWriter
	keyframe bool // forces the keyframe flag on the next chunk for THIS listener (05 §5.6.4)
}

func newSender() *sender {
	return &sender{
		listeners: make(map[string]*listener),
		dial:      dialUDP,
	}
}

// dialUDP connects a UDP socket to addr. A connected socket lets Write copy once
// into the kernel and lets the OS pick the source port (matching the one-socket-
// per-listener model, D5).
func dialUDP(addr *net.UDPAddr) (packetWriter, error) {
	c, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// add registers id→addr. It is idempotent: re-adding a known id is a no-op and
// does NOT re-arm the keyframe flag (the group engine treats AddListener as
// idempotent by id, 05 §5.6.1). Returns whether the listener was newly added (so
// the caller can arm the join keyframe only on a real join, 05 §5.6.4).
func (s *sender) add(id string, addr *net.UDPAddr) (added bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.listeners[id]; ok {
		return false, nil
	}
	conn, err := s.dial(addr)
	if err != nil {
		return false, err
	}
	s.listeners[id] = &listener{addr: addr, conn: conn, keyframe: true}
	return true, nil
}

// remove drops id and closes its socket. Idempotent: removing an unknown id is a
// no-op (05 §5.6.1).
func (s *sender) remove(id string) {
	s.mu.Lock()
	l, ok := s.listeners[id]
	if ok {
		delete(s.listeners, id)
	}
	s.mu.Unlock()
	if ok && l.conn != nil {
		_ = l.conn.Close()
	}
}

// count returns the number of registered listeners.
func (s *sender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.listeners)
}

// armKeyframe forces the join keyframe flag for one listener (05 §5.6.4 late
// join). Idempotent; a no-op for an unknown id (the listener may have been removed
// between add and this call).
func (s *sender) armKeyframe(id string) {
	s.mu.Lock()
	if l, ok := s.listeners[id]; ok {
		l.keyframe = true
	}
	s.mu.Unlock()
}

// armKeyframeAll forces the join keyframe flag for every current listener, used by
// ResumeAt / generation change so the first chunk of the new generation is a
// keyframe for everyone (05 §5.8).
func (s *sender) armKeyframeAll() {
	s.mu.Lock()
	for _, l := range s.listeners {
		l.keyframe = true
	}
	s.mu.Unlock()
}

// needKeyframe reports whether ANY current listener still needs a forced keyframe
// (a fresh join or a generation change). The origin uses this to decide whether to
// set the keyframe flag on the next chunk; for PCM every chunk is a keyframe anyway
// (05 §5.4.1) so this only matters for inter-frame codecs.
func (s *sender) needKeyframe() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.listeners {
		if l.keyframe {
			return true
		}
	}
	return false
}

// clearKeyframe disarms the forced-keyframe flag on every listener after a
// keyframe chunk has been emitted to all of them.
func (s *sender) clearKeyframe() {
	s.mu.Lock()
	for _, l := range s.listeners {
		l.keyframe = false
	}
	s.mu.Unlock()
}

// fanOut writes each packet in the bundle to every registered listener (D5). A
// per-listener write error is swallowed (best-effort unicast over UDP); it does
// not abort the fan-out to the other listeners. Returns the number of (packet ×
// listener) writes attempted, for the D5 fan-out-count test.
func (s *sender) fanOut(packets [][]byte) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	writes := 0
	for _, l := range s.listeners {
		for _, pkt := range packets {
			_, _ = l.conn.Write(pkt)
			writes++
		}
	}
	return writes
}

// closeAll closes every listener socket (origin shutdown).
func (s *sender) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, l := range s.listeners {
		if l.conn != nil {
			_ = l.conn.Close()
		}
		delete(s.listeners, id)
	}
}
