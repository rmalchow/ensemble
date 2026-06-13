package source

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
	"ensemble/internal/stream"
)

// keepaliveTTL is how long a subscriber may go unseen before expiry (§8.7).
const keepaliveTTL = 15 * time.Second

// sweepInterval is the expiry sweeper tick.
const sweepInterval = 1 * time.Second

// tcpWriteTimeout bounds a fan-out/prime write so a wedged TCP subscriber can't
// stall H's release ticker (§8.4, D13). On timeout the conn is marked dead.
const tcpWriteTimeout = 50 * time.Millisecond

// Frame is one released audio frame handed to the source for fan-out. The
// server stamps Seq/Gen; H supplies pts + payload via ReleaseFrame.
type Frame struct {
	Seq     uint64
	PTS     int64
	Payload []byte
}

// sourceCounters are the atomic stats read lock-free by Stats.
type sourceCounters struct {
	connects, restarts, primes, parity atomic.Uint64
}

// Server is the audio source: SOURCE_PORT control intake + per-frame fan-out to
// the subscriber registry, with a ring buffer for burst priming.
type Server struct {
	mu    sync.Mutex
	self  id.ID
	udp   *net.UDPConn
	tcpLn *net.TCPListener
	reg   registry
	ring  ringBuffer
	fec   fecBlock

	active    bool
	gen       uint32
	transport stream.Transport
	bufferMs  int
	seq       uint64

	// statuses holds the last STATUS (D55) each playback node reported, keyed by
	// its node id (from the payload). Read by the master for per-room health +
	// STATUS-recency liveness (D60). Guarded by mu.
	statuses map[id.ID]PlaybackStatus

	scratch []byte // reusable encode buffer (under mu)

	stats    sourceCounters
	onStatus func(id.ID) // D60: refresh playback-node liveness on STATUS (nil-safe)
	done     chan struct{}
	wg       sync.WaitGroup
	once     sync.Once
	log      *slog.Logger
}

// Config wires a Server to its already-bound SOURCE_PORT sockets (owned by K).
type Config struct {
	Self id.ID
	UDP  *net.UDPConn
	TCP  *net.TCPListener
	Log  *slog.Logger
	// OnStatus is called with a playback node's id each time its STATUS arrives
	// (D55/D60): the master uses it to refresh that node's liveness so an actively-
	// playing node stays alive even if its mDNS re-announce lapses. nil-safe.
	OnStatus func(id.ID)
}

// NewServer builds a Server; no goroutines yet.
func NewServer(cfg Config) *Server {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		self:     cfg.Self,
		udp:      cfg.UDP,
		tcpLn:    cfg.TCP,
		reg:      newRegistry(),
		statuses: make(map[id.ID]PlaybackStatus),
		onStatus: cfg.OnStatus,
		scratch:  make([]byte, 0, stream.HeaderSize+stream.FrameBytes),
		done:     make(chan struct{}),
		log:      log.With("comp", "source"),
	}
}

// PlaybackStatus is one playback node's last-reported STATUS (D55) plus when it
// arrived (for STATUS-recency liveness, D60).
type PlaybackStatus struct {
	Status   stream.StatusPayload
	LastSeen time.Time
}

// Statuses returns a copy of the latest STATUS reported by each playback node,
// keyed by node id. Safe to call from any goroutine.
func (s *Server) Statuses() map[id.ID]PlaybackStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[id.ID]PlaybackStatus, len(s.statuses))
	for k, v := range s.statuses {
		out[k] = v
	}
	return out
}

// Run starts the UDP control read loop, the TCP accept loop, and the expiry
// sweeper. Non-blocking; call once.
func (s *Server) Run() {
	s.wg.Add(3)
	go s.udpLoop()
	go s.acceptLoop()
	go s.sweepLoop()
}

// StartSession arms a new play session: bumps to gen, sets transport/bufferMs,
// resizes/clears the ring, resets FEC + Seq, marks active, and broadcasts a
// non-stop RECONFIG so existing subscribers resubscribe under the new gen (D23).
func (s *Server) StartSession(gen uint32, t stream.Transport, bufferMs int) {
	s.mu.Lock()
	s.gen = gen
	s.transport = t
	s.bufferMs = bufferMs
	s.ring.resize(bufferMs)
	s.fec.reset(gen)
	s.seq = 0
	s.active = true
	ringSize := len(s.ring.frames)
	clients := s.reg.count()
	s.mu.Unlock()

	s.log.Info("session start", "gen", gen, "transport", t.String(),
		"bufferMs", bufferMs, "ringSize", ringSize, "clients", clients)
	s.Reconfig()
}

// ReleaseFrame fans out one frame: stamps Seq, appends to the ring, sends to
// every live subscriber on the session transport, and (UDP) folds it into the
// FEC block — emitting a parity datagram after every 4th frame. Returns the Seq
// used, or 0 if no session is active. Never errors: per-subscriber write
// failures are counted, not propagated.
func (s *Server) ReleaseFrame(pts int64, payload []byte) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return 0
	}
	seq := s.seq
	s.seq++
	s.ring.push(seq, pts, payload)

	h := stream.Header{
		Magic:      stream.Magic,
		Type:       stream.TypeAudio,
		Gen:        s.gen,
		Seq:        seq,
		PTS:        pts,
		PayloadLen: uint16(len(payload)),
	}
	pkt := h.AppendFrame(s.scratch[:0], payload)

	subs := s.reg.live()
	for _, sub := range subs {
		s.sendTo(sub, pkt)
	}

	if s.transport == stream.TransportUDP {
		s.fec.fold(s.gen, seq, payload)
		if s.fec.ready() {
			if par := s.fec.parityPacket(s.scratch[:0]); par != nil {
				s.stats.parity.Add(1)
				for _, sub := range subs {
					if sub.tr == stream.TransportUDP {
						s.writeUDP(par, sub.addr)
					}
				}
			}
		}
	}
	return seq
}

// sendTo writes one audio packet to a single subscriber on its transport.
// Caller holds s.mu (for the registry snapshot consistency); the actual writes
// are non-blocking (UDP sendto) or deadline-bounded (TCP).
func (s *Server) sendTo(sub *subscriber, pkt []byte) {
	if sub.dead {
		return
	}
	if sub.tr == stream.TransportTCP {
		s.writeTCP(sub, pkt)
		return
	}
	s.writeUDP(pkt, sub.addr)
}

func (s *Server) writeUDP(pkt []byte, addr netip.AddrPort) {
	if _, err := s.udp.WriteToUDPAddrPort(pkt, addr); err != nil {
		s.log.Debug("udp fan-out write failed", "addr", addr, "err", err)
	}
}

// writeTCP writes a length-prefixed chunk to a TCP subscriber under its wmu,
// with a short write deadline. A timeout/error marks the conn dead so the
// reader removes it.
func (s *Server) writeTCP(sub *subscriber, pkt []byte) {
	sub.wmu.Lock()
	defer sub.wmu.Unlock()
	if sub.conn == nil || sub.dead {
		return
	}
	_ = sub.conn.SetWriteDeadline(time.Now().Add(tcpWriteTimeout))
	if err := writeTCPFrame(sub.conn, pkt); err != nil {
		s.log.Debug("tcp fan-out write failed; marking dead", "addr", sub.addr, "err", err)
		sub.dead = true
		_ = sub.conn.Close()
	}
}

// Reconfig broadcasts a non-stop RECONFIG to every subscriber (D23).
func (s *Server) Reconfig() {
	s.mu.Lock()
	gen := s.gen
	clients := s.reg.count()
	s.broadcastReconfig(false)
	s.mu.Unlock()
	s.log.Info("reconfig broadcast", "gen", gen, "stop", false, "clients", clients)
}

// StopSession ends the current session: flushes any partial FEC tail block,
// broadcasts RECONFIG with the STOP flag, marks inactive, and clears the ring.
// Subscribers are NOT removed. Idempotent.
func (s *Server) StopSession() {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return
	}
	gen := s.gen
	frames := s.seq
	clients := s.reg.count()
	if s.transport == stream.TransportUDP {
		if par := s.fec.flushPartial(s.scratch[:0]); par != nil {
			s.stats.parity.Add(1)
			for _, sub := range s.reg.live() {
				if sub.tr == stream.TransportUDP {
					s.writeUDP(par, sub.addr)
				}
			}
		}
	}
	s.broadcastReconfig(true)
	s.active = false
	s.ring.clear()
	s.mu.Unlock()
	s.log.Info("session stop", "gen", gen, "framesReleased", frames, "clients", clients)
	s.log.Info("reconfig broadcast", "gen", gen, "stop", true, "clients", clients)
}

// broadcastReconfig sends a RECONFIG control to all subscribers. Caller holds mu.
func (s *Server) broadcastReconfig(stop bool) {
	var flag byte
	if stop {
		flag = stream.FlagStop
	}
	h := stream.Header{
		Magic:      stream.Magic,
		Type:       stream.TypeReconfig,
		Gen:        s.gen,
		PayloadLen: 1,
	}
	pkt := h.AppendFrame(make([]byte, 0, stream.HeaderSize+1), []byte{flag})
	for _, sub := range s.reg.live() {
		if sub.tr == stream.TransportTCP {
			s.writeTCP(sub, pkt)
		} else {
			s.writeUDP(pkt, sub.addr)
		}
	}
}

// Stats returns a snapshot of source stats (D28).
func (s *Server) Stats() contracts.SourceStats {
	s.mu.Lock()
	clients := s.reg.count()
	released := s.seq
	s.mu.Unlock()
	return contracts.SourceStats{
		Clients:  clients,
		Connects: s.stats.connects.Load(),
		Restarts: s.stats.restarts.Load(),
		Primes:   s.stats.primes.Load(),
		Released: released,
		Parity:   s.stats.parity.Load(),
	}
}

// Close stops all goroutines and closes tracked TCP subscriber conns. It does
// NOT close the SOURCE_PORT sockets — K owns them. Idempotent.
func (s *Server) Close() error {
	s.once.Do(func() {
		close(s.done)
		// Unblock the read/accept loops.
		_ = s.udp.SetReadDeadline(time.Now())
		_ = s.tcpLn.SetDeadline(time.Now())
		s.mu.Lock()
		for _, sub := range s.reg.live() {
			if sub.conn != nil {
				_ = sub.conn.Close()
			}
		}
		s.mu.Unlock()
		s.wg.Wait()
		// Clear the deadlines so K can keep using the sockets if it wants.
		_ = s.udp.SetReadDeadline(time.Time{})
	})
	return nil
}

// --- goroutine loops --------------------------------------------------------

// udpLoop reads control datagrams on the SOURCE_PORT UDP socket.
func (s *Server) udpLoop() {
	defer s.wg.Done()
	buf := make([]byte, 64*1024)
	for {
		select {
		case <-s.done:
			return
		default:
		}
		n, from, err := s.udp.ReadFromUDPAddrPort(buf)
		// Canonicalize v4-mapped senders (see stream.Mux): keeps registry keys
		// and observed addrs in plain IPv4 form on dual-stack sockets.
		from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				continue
			}
		}
		s.handleControlUDP(buf[:n], from)
	}
}

func (s *Server) handleControlUDP(pkt []byte, from netip.AddrPort) {
	h, payload, err := stream.DecodeFrame(pkt)
	if err != nil {
		return
	}
	now := time.Now()
	switch h.Type {
	case stream.TypeHello:
		primeMe := len(payload) > 0 && payload[0]&stream.FlagPrimeMe != 0
		s.onSubscribe(from, stream.TransportUDP, nil, now, primeMe, false)
	case stream.TypeRestart:
		s.onSubscribe(from, stream.TransportUDP, nil, now, true, true)
	case stream.TypeBye:
		s.mu.Lock()
		removed := s.reg.get(from) != nil
		s.reg.remove(from)
		s.mu.Unlock()
		if removed {
			s.log.Info("client left (BYE)", "addr", from.String(), "transport", "udp")
		}
	case stream.TypeStatus:
		// A playback node's telemetry (D55). It arrives from the node's CONTROL_PORT
		// (not its audio source addr), so correlate by the node id in the payload.
		if st, err := stream.DecodeStatus(payload); err == nil {
			nid := id.ID(st.NodeID)
			s.mu.Lock()
			s.statuses[nid] = PlaybackStatus{Status: st, LastSeen: now}
			s.mu.Unlock()
			if s.onStatus != nil {
				s.onStatus(nid) // refresh liveness (D60): STATUS keeps a driven node alive
			}
		} else if s.log != nil && s.log.Enabled(context.Background(), slog.LevelDebug) {
			// Only under -v: a STATUS that won't decode is almost always a
			// version/wire-format mismatch (a playback node on stale firmware) —
			// telemetry silently vanishes otherwise. payloadLen pins the cause.
			s.log.Warn("STATUS decode failed (version/wire mismatch?)",
				"payloadLen", len(payload), "from", from.String(), "err", err)
		}
	}
}

// onSubscribe records a HELLO/RESTART and, if requested, launches a prime burst.
func (s *Server) onSubscribe(addr netip.AddrPort, t stream.Transport, conn net.Conn, now time.Time, primeMe, isRestart bool) {
	s.mu.Lock()
	sub, isNew := s.reg.upsert(addr, t, conn, now)
	if isNew {
		s.stats.connects.Add(1)
	}
	if isRestart {
		s.stats.restarts.Add(1)
	}
	var primeFrames int
	var primeSpanMs int64
	primed := false
	if primeMe && s.active && !sub.priming {
		if frames := s.ring.prime(); len(frames) > 0 {
			// Mark priming under the lock so live fan-out skips this sub
			// until the burst catches up to the live edge (see prime.go).
			sub.priming = true
			s.stats.primes.Add(1)
			primed = true
			primeFrames = len(frames)
			primeSpanMs = (frames[len(frames)-1].pts - frames[0].pts) / 1_000_000
			s.wg.Add(1)
			go s.prime(sub, frames, s.gen)
		}
	}
	s.mu.Unlock()

	switch {
	case isRestart:
		s.log.Info("client RESTART", "addr", addr.String(), "transport", t.String(), "prime", primed)
	case isNew:
		s.log.Info("client joined (HELLO)", "addr", addr.String(), "transport", t.String(), "primeRequested", primeMe)
	}
	if primed {
		s.log.Info("prime burst", "addr", addr.String(), "frames", primeFrames, "spanMs", primeSpanMs)
	}
}

// acceptLoop accepts TCP subscriber connections.
func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.tcpLn.AcceptTCP()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				continue
			}
		}
		s.wg.Add(1)
		go s.tcpConnReader(conn)
	}
}

// tcpConnReader reads length-prefixed control frames from a TCP subscriber.
func (s *Server) tcpConnReader(conn *net.TCPConn) {
	defer s.wg.Done()
	defer conn.Close()

	ap, _ := netip.ParseAddrPort(conn.RemoteAddr().String())
	s.log.Debug("tcp subscriber conn accepted", "addr", ap.String())

	for {
		select {
		case <-s.done:
			return
		default:
		}
		chunk, err := readTCPFrame(conn)
		if err != nil {
			s.mu.Lock()
			existed := s.reg.get(ap) != nil
			s.reg.remove(ap)
			s.mu.Unlock()
			if existed {
				s.log.Info("tcp subscriber conn closed", "addr", ap.String(), "err", err)
			}
			return
		}
		h, payload, derr := stream.DecodeFrame(chunk)
		if derr != nil {
			continue
		}
		now := time.Now()
		switch h.Type {
		case stream.TypeHello:
			primeMe := len(payload) > 0 && payload[0]&stream.FlagPrimeMe != 0
			s.onSubscribe(ap, stream.TransportTCP, conn, now, primeMe, false)
		case stream.TypeRestart:
			s.onSubscribe(ap, stream.TransportTCP, conn, now, true, true)
		case stream.TypeBye:
			s.mu.Lock()
			s.reg.remove(ap)
			s.mu.Unlock()
			s.log.Info("client left (BYE)", "addr", ap.String(), "transport", "tcp")
			return
		}
	}
}

// sweepLoop expires subscribers unseen for keepaliveTTL.
func (s *Server) sweepLoop() {
	defer s.wg.Done()
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case now := <-t.C:
			s.mu.Lock()
			conns, expired := s.reg.expire(now, keepaliveTTL)
			s.mu.Unlock()
			for _, c := range conns {
				_ = c.Close()
			}
			for _, addr := range expired {
				s.log.Info("client left (keepalive expiry)", "addr", addr.String(), "ttl", keepaliveTTL.String())
			}
			s.logRoomSkew(now)
			s.logSourceStats()
		}
	}
}

// logSourceStats emits a 1 s source heartbeat (D64 telemetry) while a session is
// active: subscribers, frames released, primes/restarts served, FEC parity sent.
func (s *Server) logSourceStats() {
	s.mu.Lock()
	active := s.active
	gen := s.gen
	s.mu.Unlock()
	if !active {
		return
	}
	st := s.Stats()
	s.log.Debug("stats", "gen", gen,
		"clients", st.Clients, "released", st.Released, "connects", st.Connects,
		"primes", st.Primes, "restarts", st.Restarts, "parity", st.Parity)
}

// logRoomSkew surfaces inter-room sync (D63 telemetry). For rooms currently playing
// and recently heard, the master-clock speaker time ≈ pts + bufferMs − comp +
// deviceDelay (comp is uniform), so the audible skew is the SPREAD of deviceDelay
// across rooms. It logs that spread plus each room's device latency / clock offset /
// RTT / synced, so a desync is visible without diffing per-node logs. No-op for <2
// playing rooms.
func (s *Server) logRoomSkew(now time.Time) {
	type room struct {
		id                          id.ID
		ddMs, offMs, rttMs, phaseMs int64
		synced, calibrated          bool
	}
	s.mu.Lock()
	var rooms []room
	for nid, ps := range s.statuses {
		if !ps.Status.Playing || now.Sub(ps.LastSeen) > keepaliveTTL {
			continue
		}
		rooms = append(rooms, room{
			id:         nid,
			ddMs:       ps.Status.DeviceDelayNs / 1_000_000,
			offMs:      ps.Status.OffsetNs / 1_000_000,
			rttMs:      ps.Status.RTTNs / 1_000_000,
			phaseMs:    ps.Status.PhaseErrNs / 1_000_000,
			synced:     ps.Status.Synced,
			calibrated: ps.Status.Calibrated,
		})
	}
	s.mu.Unlock()
	if len(rooms) < 2 {
		return
	}
	min, max := rooms[0].ddMs, rooms[0].ddMs
	parts := make([]string, 0, len(rooms))
	for _, r := range rooms {
		if r.ddMs < min {
			min = r.ddMs
		}
		if r.ddMs > max {
			max = r.ddMs
		}
		parts = append(parts, fmt.Sprintf("%s(dd=%d phase=%d off=%d rtt=%d sync=%t cal=%t)",
			r.id.String()[:8], r.ddMs, r.phaseMs, r.offMs, r.rttMs, r.synced, r.calibrated))
	}
	s.log.Info("room sync", "skewMs", max-min, "rooms", len(rooms), "detail", strings.Join(parts, " "))
}
