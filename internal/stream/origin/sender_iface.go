package origin

// sender_iface.go (P7.1) abstracts the two fan-out senders (UDP, sender.go; TCP,
// tcp_sender.go) behind one interface so origin.Run is transport-agnostic: the
// produce/fan-out loop calls fanOutSender regardless of which transport the group
// negotiated (05 §5.9). The Origin holds exactly one concrete sender, chosen at
// New from cfg.Transport.
//
// AddListener keeps its public *net.UDPAddr signature (P4.8 §4.1 — no API churn);
// the TCP adapter reuses that addr's IP+Port as the TCP destination (both planes
// share the audio port, A.12 :9100), so cmd/ensemble wires one listener address
// per follower irrespective of transport.

import "net"

// fanOutSender is the transport-agnostic send registry + fan-out used by Run. Both
// *sender (UDP) and the TCP adapter satisfy it.
type fanOutSender interface {
	add(id string, addr *net.UDPAddr) (added bool, err error)
	remove(id string)
	count() int
	armKeyframe(id string)
	armKeyframeAll()
	needKeyframe() bool
	clearKeyframe()
	fanOut(packets [][]byte) int
	closeAll()
}

// tcpSenderAdapter adapts *tcpSender (whose add takes a *net.TCPAddr) to the
// fanOutSender interface (whose add takes a *net.UDPAddr, the Origin's public
// listener address type). It converts the UDP address to the equivalent TCP
// address — same IP and port — at registration time.
type tcpSenderAdapter struct{ *tcpSender }

func (a tcpSenderAdapter) add(id string, addr *net.UDPAddr) (bool, error) {
	tcpAddr := &net.TCPAddr{IP: addr.IP, Port: addr.Port, Zone: addr.Zone}
	return a.tcpSender.add(id, tcpAddr)
}

// compile-time assertions that both senders satisfy fanOutSender.
var (
	_ fanOutSender = (*sender)(nil)
	_ fanOutSender = tcpSenderAdapter{}
)
