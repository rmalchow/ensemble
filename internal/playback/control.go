package playback

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"ensemble/internal/stream"
)

// statusInterval is the STATUS heartbeat cadence (PLAYER §6.3).
const statusInterval = 1 * time.Second

// The real CONTROL_PORT socket K injects must satisfy controlConn.
var _ controlConn = (*net.UDPConn)(nil)

// controlConn is the slice of *net.UDPConn the Listener needs (CONTROL_PORT
// socket). An interface so tests inject a fake without real sockets.
type controlConn interface {
	ReadFromUDPAddrPort(b []byte) (int, netip.AddrPort, error)
	WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error)
	SetReadDeadline(t time.Time) error
}

// Listener is the playback-side control plane (D58, PLAYER §6): it reads
// master→playback commands on the CONTROL_PORT, drives a Player, and emits STATUS
// back to the master ~1 Hz. It is the front-end a non-gossiping playback node uses
// instead of the group engine; both drive the identical Player (D61).
//
// Soft-state (D58): the master re-asserts ATTACH/SETVOL/SETDELAY on a heartbeat, so
// the Listener applies each command IDEMPOTENTLY — it forwards to the Player only on
// a real change, so a 1 Hz re-assert never re-arms the sink (ATTACH) or re-anchors
// playout (SETDELAY).
type Listener struct {
	conn   controlConn
	player Player
	log    *slog.Logger

	mu        sync.Mutex
	cur       applied        // last-applied command state (dedup)
	statusDst netip.AddrPort // master source endpoint, from the last ATTACH

	done chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

// applied is the last command state the Listener forwarded to the Player, so a
// repeated soft-state assertion with identical values is a no-op.
type applied struct {
	attached  bool
	source    netip.AddrPort
	clock     netip.AddrPort
	codec     stream.Codec
	transport stream.Transport
	bufferMs  uint16

	haveVol bool
	volPct  uint8
	mute    bool

	haveDelay bool
	delayMs   int16

	haveEq bool
	eqMs   uint16
}

// ListenerConfig wires a Listener. Conn is the CONTROL_PORT UDP socket (owned by K,
// like the source server's sockets); Player is the local playout component.
type ListenerConfig struct {
	Conn   *net.UDPConn
	Player Player
	Log    *slog.Logger
}

// NewListener builds a Listener; starts no goroutines (call Run).
func NewListener(cfg ListenerConfig) *Listener {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Listener{
		conn:   cfg.Conn,
		player: cfg.Player,
		log:    log.With("comp", "pb-control"),
		done:   make(chan struct{}),
	}
}

// Run launches the command read loop and the STATUS heartbeat. Non-blocking.
func (l *Listener) Run() {
	l.wg.Add(2)
	go l.readLoop()
	go l.statusLoop()
}

// Close stops the goroutines. It does NOT close the socket (K owns it). Idempotent.
func (l *Listener) Close() error {
	l.once.Do(func() {
		close(l.done)
		_ = l.conn.SetReadDeadline(time.Now()) // unblock the read loop
		l.wg.Wait()
		_ = l.conn.SetReadDeadline(time.Time{})
	})
	return nil
}

func (l *Listener) readLoop() {
	defer l.wg.Done()
	buf := make([]byte, 64*1024)
	for {
		select {
		case <-l.done:
			return
		default:
		}
		_ = l.conn.SetReadDeadline(time.Now().Add(time.Second))
		n, from, err := l.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			select {
			case <-l.done:
				return
			default:
				continue
			}
		}
		from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		h, payload, derr := stream.DecodeFrame(buf[:n])
		if derr != nil {
			continue // not ours / malformed → ignore
		}
		l.handle(h.Type, payload, from)
	}
}

// warnDecode logs a control-packet decode failure at WARN, but ONLY when verbose
// (-v / debug) logging is enabled. A wrong payload length here is the fingerprint
// of a version / wire-format mismatch between this node and its master (e.g. a Pi
// left on stale firmware) — otherwise these are dropped silently.
func (l *Listener) warnDecode(typ byte, payload []byte, from netip.AddrPort, err error) {
	if l.log == nil || !l.log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	l.log.Warn("control decode failed (version/wire mismatch?)",
		"type", fmt.Sprintf("0x%02x", typ), "payloadLen", len(payload),
		"from", from.String(), "err", err)
}

// handle dispatches one decoded control packet. Pure w.r.t. sockets (unit-tested
// directly). Unknown types are ignored (forward-compat, PLAYER §2).
func (l *Listener) handle(typ byte, payload []byte, from netip.AddrPort) {
	switch typ {
	case stream.TypeAttach:
		a, err := stream.DecodeAttach(payload)
		if err != nil {
			l.warnDecode(typ, payload, from, err)
			return
		}
		l.onAttach(a)
	case stream.TypeDetach:
		l.onDetach()
	case stream.TypeSetVol:
		v, err := stream.DecodeSetVol(payload)
		if err != nil {
			l.warnDecode(typ, payload, from, err)
			return
		}
		l.onSetVol(v)
	case stream.TypeSetDelay:
		d, err := stream.DecodeSetDelay(payload)
		if err != nil {
			l.warnDecode(typ, payload, from, err)
			return
		}
		l.onSetDelay(d)
	case stream.TypeSetEq:
		e, err := stream.DecodeSetEqualize(payload)
		if err != nil {
			l.warnDecode(typ, payload, from, err)
			return
		}
		l.onSetEqualize(e)
	case stream.TypeSetCap:
		c, err := stream.DecodeSetCap(payload)
		if err != nil {
			l.warnDecode(typ, payload, from, err)
			return
		}
		// Caps are rare toggles; forward without dedup (the Player is idempotent).
		l.player.SetCap(c.CapID, c.On)
	case stream.TypeStatusReq:
		// Liveness poll from a master (D60): reply with STATUS to the requester,
		// regardless of attachment, so the master confirms we're alive even when
		// idle (following no one). This — not mDNS — is the authoritative liveness
		// signal for a known node; mDNS only does first discovery.
		l.sendStatusTo(from)
	default:
		// ignore (incl. data-plane types that arrived here by mistake)
	}
}

func (l *Listener) onAttach(a stream.AttachPayload) {
	l.mu.Lock()
	// statusDst tracks the master's source endpoint even on a pure heartbeat.
	l.statusDst = a.Source
	changed := !l.cur.attached ||
		l.cur.source != a.Source || l.cur.clock != a.Clock ||
		l.cur.codec != a.Codec || l.cur.transport != a.Transport
	l.cur.attached = true
	l.cur.source, l.cur.clock = a.Source, a.Clock
	l.cur.codec, l.cur.transport, l.cur.bufferMs = a.Codec, a.Transport, a.BufferMs
	l.mu.Unlock()

	if !changed {
		return // soft-state heartbeat for an unchanged attachment: no-op
	}
	// ATTACH carries no gen (PLAYER §6.1): subscribe at gen 0 and let the
	// first audio frame / RECONFIG re-anchor upward, exactly as the group engine
	// does on a master change (watch.go).
	l.player.Attach(Attach{
		Source:    a.Source,
		Clock:     a.Clock,
		Gen:       0,
		Codec:     a.Codec,
		Transport: a.Transport,
		BufferMs:  int(a.BufferMs),
	})
	l.log.Info("attached", "source", a.Source.String(), "clock", a.Clock.String(),
		"codec", a.Codec.String(), "transport", a.Transport.String(), "bufferMs", a.BufferMs)
}

func (l *Listener) onDetach() {
	l.mu.Lock()
	wasAttached := l.cur.attached
	l.cur.attached = false
	l.mu.Unlock()
	if !wasAttached {
		return
	}
	l.player.Detach()
	l.log.Info("detached")
}

func (l *Listener) onSetVol(v stream.SetVolPayload) {
	l.mu.Lock()
	changed := !l.cur.haveVol || l.cur.volPct != v.VolumePct || l.cur.mute != v.Mute
	l.cur.haveVol = true
	l.cur.volPct, l.cur.mute = v.VolumePct, v.Mute
	l.mu.Unlock()
	if !changed {
		return
	}
	l.player.SetVolume(v.VolumePct, v.Mute)
}

func (l *Listener) onSetDelay(d stream.SetDelayPayload) {
	l.mu.Lock()
	changed := !l.cur.haveDelay || l.cur.delayMs != d.DelayMs
	l.cur.haveDelay = true
	l.cur.delayMs = d.DelayMs
	l.mu.Unlock()
	if !changed {
		return // re-anchoring playout every heartbeat would be audible
	}
	l.player.SetDelay(int(d.DelayMs))
}

// onSetEqualize applies the master's cross-room equalization delay (D65). Like
// SETDELAY it dedups: the master re-asserts it every heartbeat for loss recovery,
// but only a CHANGED value re-anchors playout (re-anchoring every tick is audible).
func (l *Listener) onSetEqualize(e stream.SetEqualizePayload) {
	l.mu.Lock()
	changed := !l.cur.haveEq || l.cur.eqMs != e.DelayMs
	l.cur.haveEq = true
	l.cur.eqMs = e.DelayMs
	l.mu.Unlock()
	if !changed {
		return
	}
	l.player.SetEqualize(int(e.DelayMs))
}

func (l *Listener) statusLoop() {
	defer l.wg.Done()
	t := time.NewTicker(statusInterval)
	defer t.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-t.C:
			l.sendStatus()
		}
	}
}

// sendStatus emits one STATUS packet to the master's source endpoint, if attached
// (the periodic heartbeat while playing).
func (l *Listener) sendStatus() {
	l.mu.Lock()
	dst := l.statusDst
	attached := l.cur.attached
	l.mu.Unlock()
	if !attached {
		return
	}
	l.sendStatusTo(dst)
}

// sendStatusTo emits one STATUS packet to dst (used by both the attached heartbeat
// and the reply to a master's liveness poll, which may arrive while idle).
func (l *Listener) sendStatusTo(dst netip.AddrPort) {
	if !dst.IsValid() || dst.Port() == 0 {
		return
	}
	h := stream.Header{Magic: stream.Magic, Type: stream.TypeStatus}
	pkt := h.AppendFrame(make([]byte, 0, stream.HeaderSize+stream.StatusLen), l.player.Status().AppendTo(nil))
	if _, err := l.conn.WriteToUDPAddrPort(pkt, dst); err != nil {
		l.log.Debug("status send failed", "to", dst, "err", err)
	}
}
