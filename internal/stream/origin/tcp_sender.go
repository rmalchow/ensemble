package origin

// tcp_sender.go (P7.1) is the TCP-fallback write half (05 §5.9, D2). It is the
// reliable-stream analogue of sender.go: it keeps a per-listener destination map
// (the fan-out-to-a-destination-map shape cited from media internal/sync
// BeaconServer.Publish — pattern, not copied) but instead of one connected UDP
// socket per listener it dials one *net.TCPConn per listener and writes 2-byte
// big-endian length-prefixed marshaled wire packets back-to-back (sink_net
// writeFrame). TCP frames the stream; the prefix delimits packets.
//
// FEC is forced None on this path (the Origin builds with fec.None when
// Transport==TCP, 05 §5.9), so each chunk is exactly one packet — the fan-out is
// one frame per listener. Pacing is unchanged: the Run loop still parks on
// playout(idx)−Lead, so the writer does not flood the socket and inflate the TCP
// queue (05 §5.9 "pacing still applies").
//
// Like sender.go, the registry is mutex-guarded so AddListener/RemoveListener
// (group-engine goroutine) are safe against fanOut (origin Run goroutine).

import (
	"net"
	"sync"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/sink_net"
)

// tcpListener is one registered TCP destination. keyframe mirrors the UDP
// listener's per-listener forced-keyframe flag (05 §5.6.4) so the sender behind
// either transport shares the same keyframe accounting.
type tcpListener struct {
	addr     *net.TCPAddr
	conn     streamWriter
	keyframe bool
}

// streamWriter is the minimal write side of a TCP listener connection (a
// connected *net.TCPConn in production, a fake in tests). Closed on remove.
type streamWriter interface {
	Write(b []byte) (int, error)
	Close() error
}

// tcpSender owns the per-listener TCP connection registry and the length-prefixed
// fan-out write. It is the Transport==TCP twin of *sender; the Origin holds
// exactly one of the two (selected at New by cfg.Transport).
type tcpSender struct {
	mu        sync.Mutex
	listeners map[string]*tcpListener
	// dial builds the per-listener connection; injectable so tests capture frames
	// without real sockets. Defaults to dialTCPListener.
	dial func(addr *net.TCPAddr) (streamWriter, error)
}

func newTCPSender() *tcpSender {
	return &tcpSender{
		listeners: make(map[string]*tcpListener),
		dial:      dialTCPListener,
	}
}

// dialTCPListener connects a TCP socket to addr (one stream per listener, 05 §5.9).
func dialTCPListener(addr *net.TCPAddr) (streamWriter, error) {
	c, err := net.DialTCP("tcp", nil, addr)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// add registers id→addr, dialing the TCP connection. Idempotent by id (re-adding
// a known id is a no-op and does not re-arm the keyframe). Returns whether it was
// newly added so the caller arms the join keyframe only on a real join.
func (s *tcpSender) add(id string, addr *net.TCPAddr) (added bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.listeners[id]; ok {
		return false, nil
	}
	conn, err := s.dial(addr)
	if err != nil {
		return false, err
	}
	s.listeners[id] = &tcpListener{addr: addr, conn: conn, keyframe: true}
	return true, nil
}

// remove drops id and closes its connection. Idempotent.
func (s *tcpSender) remove(id string) {
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

func (s *tcpSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.listeners)
}

func (s *tcpSender) armKeyframe(id string) {
	s.mu.Lock()
	if l, ok := s.listeners[id]; ok {
		l.keyframe = true
	}
	s.mu.Unlock()
}

func (s *tcpSender) armKeyframeAll() {
	s.mu.Lock()
	for _, l := range s.listeners {
		l.keyframe = true
	}
	s.mu.Unlock()
}

func (s *tcpSender) needKeyframe() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.listeners {
		if l.keyframe {
			return true
		}
	}
	return false
}

func (s *tcpSender) clearKeyframe() {
	s.mu.Lock()
	for _, l := range s.listeners {
		l.keyframe = false
	}
	s.mu.Unlock()
}

// fanOut writes each packet in the bundle to every listener, length-prefixed
// (05 §5.9). On the TCP path FEC is None so the bundle is a single source packet;
// the loop tolerates a multi-packet bundle for symmetry with the UDP sender. A
// per-listener write error closes and drops that listener (a broken TCP stream
// cannot self-recover, unlike a best-effort UDP send) without aborting the
// fan-out to the others. Returns the number of (packet × listener) frames written.
func (s *tcpSender) fanOut(packets [][]byte) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	writes := 0
	for id, l := range s.listeners {
		failed := false
		for _, pkt := range packets {
			if err := sink_net.WriteFrame(l.conn, pkt); err != nil {
				failed = true
				break
			}
			writes++
		}
		if failed {
			if l.conn != nil {
				_ = l.conn.Close()
			}
			delete(s.listeners, id)
		}
	}
	return writes
}

// closeAll closes every listener connection (origin shutdown).
func (s *tcpSender) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, l := range s.listeners {
		if l.conn != nil {
			_ = l.conn.Close()
		}
		delete(s.listeners, id)
	}
}
