package stream

import (
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// keepaliveInterval is how often a subscription re-HELLOs (§8.7).
const keepaliveInterval = 5 * time.Second

// watchdogTimeout is the starvation threshold: no frame for this long trips the
// watchdog (§8.6).
const watchdogTimeout = 2 * time.Second

// DeliverFunc receives one ordered, de-duplicated, FEC-recovered frame: the
// parsed Header and its payload. payload aliases the client's buffer and is
// ONLY valid for the duration of the call — the Sink (E) copies on Push. Called
// serialized per subscription; must not block long.
type DeliverFunc func(h Header, payload []byte)

// ReconfigFunc is invoked when a RECONFIG control arrives from the source. stop
// is true for the end-of-session notice (§8.6). H re-reads replicated settings
// and re-Subscribes (non-stop) or clears playback (stop). Optional.
type ReconfigFunc func(stop bool)

// Counters are monotonic per client (NOT per session — the sink resets its own
// per-session stats; these are lifetime transport health).
type Counters struct {
	Delivered uint64 // frames handed to DeliverFunc
	Recovered uint64 // frames reconstructed by FEC
	Lost      uint64 // gaps the reorder window gave up on (E plays silence)
	Duplicate uint64 // frames dropped as already-delivered
	StaleGen  uint64 // frames dropped: gen below the active subscription gen
	Malformed uint64 // datagrams/chunks that failed Decode (UDP garbage)
	FECParity uint64 // parity datagrams received (type 0x02)
	Restarts  uint64 // RESTART controls this client issued (starvation, §8.6)
}

type clientCounters struct {
	delivered, recovered, lost, duplicate, staleGen, malformed, fecParity, restarts atomic.Uint64
}

func (c *clientCounters) snapshot() Counters {
	return Counters{
		Delivered: c.delivered.Load(),
		Recovered: c.recovered.Load(),
		Lost:      c.lost.Load(),
		Duplicate: c.duplicate.Load(),
		StaleGen:  c.staleGen.Load(),
		Malformed: c.malformed.Load(),
		FECParity: c.fecParity.Load(),
		Restarts:  c.restarts.Load(),
	}
}

// Client is the member-side subscriber: it HELLOs a master's SOURCE_PORT, keeps
// the subscription alive, receives audio (UDP via the mux, or TCP), recovers/
// reorders, and delivers frames. It also runs the starvation watchdog that
// issues RESTART, then gives up (§8.6).
type Client struct {
	mu       sync.Mutex
	mux      *Mux
	sub      *subscription // current active subscription (nil if none)
	subPtr   atomic.Pointer[subscription]
	ctr      clientCounters
	deliver  DeliverFunc
	reconfig ReconfigFunc
	log      *slog.Logger
	closed   bool
}

// ClientConfig wires a Client. Mux + Deliver are required.
type ClientConfig struct {
	Mux      *Mux
	Deliver  DeliverFunc
	Reconfig ReconfigFunc // optional RECONFIG hook
	Log      *slog.Logger
}

// NewClient builds a Client and registers the mux handlers for TypeAudio,
// TypeFEC and TypeReconfig. No subscription yet.
func NewClient(cfg ClientConfig) *Client {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	c := &Client{
		mux:      cfg.Mux,
		deliver:  cfg.Deliver,
		reconfig: cfg.Reconfig,
		log:      log.With("comp", "stream-client"),
	}
	cfg.Mux.Register(TypeAudio, c.onUDP)
	cfg.Mux.Register(TypeFEC, c.onUDP)
	cfg.Mux.Register(TypeReconfig, c.onReconfigUDP)
	return c
}

// Subscribe starts (or replaces) the active subscription to master at
// sourceAddr with the given session generation and transport. It tears down any
// prior subscription (BYE if reachable), then sends an initial prime-me HELLO,
// starts the keepalive loop and the starvation watchdog, and (TCP) dials
// sourceAddr and starts the conn reader. Idempotent for an unchanged
// (addr,gen,transport). Returns an error only on an immediate TCP dial failure.
func (c *Client) Subscribe(sourceAddr netip.AddrPort, gen uint32, t Transport) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	if cur := c.sub; cur != nil && cur.addr == sourceAddr && cur.gen == gen && cur.tr == t {
		c.mu.Unlock()
		return nil // idempotent
	}
	old := c.sub
	c.sub = nil
	c.subPtr.Store(nil)
	c.mu.Unlock()

	if old != nil {
		old.shutdown(true) // BYE + stop loops
	}

	s := &subscription{
		addr:    sourceAddr,
		gen:     gen,
		tr:      t,
		mux:     c.mux,
		window:  newReorderBuffer(gen),
		fecwin:  newRecoveryWindow(gen),
		deliver: c.deliver,
		ctr:     &c.ctr,
		client:  c,
		done:    make(chan struct{}),
		log:     c.log.With("master", sourceAddr.String(), "gen", gen, "tr", t.String()),
	}

	if t == TransportTCP {
		if err := s.dialTCP(); err != nil {
			return err
		}
	} else {
		s.helloUDP(TypeHello, true)
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		s.shutdown(false)
		return nil
	}
	c.sub = s
	c.subPtr.Store(s)
	c.mu.Unlock()

	s.startLoops()
	return nil
}

// Unsubscribe sends BYE, stops the keepalive/watchdog/reader, and clears the
// active subscription. Idempotent; safe if no subscription is active.
func (c *Client) Unsubscribe() {
	c.mu.Lock()
	old := c.sub
	c.sub = nil
	c.subPtr.Store(nil)
	c.mu.Unlock()
	if old != nil {
		old.shutdown(true)
	}
}

// Counters returns lifetime transport-health counters for /api/status.
func (c *Client) Counters() Counters { return c.ctr.snapshot() }

// Close unsubscribes and stops everything. Idempotent.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	old := c.sub
	c.sub = nil
	c.subPtr.Store(nil)
	c.mu.Unlock()
	if old != nil {
		old.shutdown(true)
	}
	return nil
}

// onUDP dispatches an audio/FEC datagram from the mux read goroutine to the
// active subscription (drops if none, or from a different addr).
func (c *Client) onUDP(pkt []byte, from netip.AddrPort) {
	h, payload, err := DecodeFrame(pkt)
	if err != nil {
		c.ctr.malformed.Add(1)
		return
	}
	s := c.subPtr.Load()
	if s == nil || from != s.addr {
		return
	}
	switch h.Type {
	case TypeAudio:
		s.ingest(h, payload, true)
	case TypeFEC:
		c.ctr.fecParity.Add(1)
		s.ingestParity(h, payload)
	}
}

// onReconfigUDP handles a RECONFIG datagram delivered on the member mux.
func (c *Client) onReconfigUDP(pkt []byte, from netip.AddrPort) {
	h, payload, err := DecodeFrame(pkt)
	if err != nil {
		c.ctr.malformed.Add(1)
		return
	}
	s := c.subPtr.Load()
	if s == nil || from != s.addr {
		return
	}
	stop := len(payload) > 0 && payload[0]&FlagStop != 0
	_ = h
	if c.reconfig != nil {
		c.reconfig(stop)
	}
}

// fireReconfig is called by the TCP reader on an inline RECONFIG.
func (c *Client) fireReconfig(stop bool) {
	if c.reconfig != nil {
		c.reconfig(stop)
	}
}

// subscription is one active link to a master's source. It holds the wire state
// (reorder window + FEC recovery), the keepalive/watchdog timers, and (TCP) the
// conn. Guarded by its own mutex; Client.mu only guards the *subscription
// pointer.
type subscription struct {
	mu   sync.Mutex
	addr netip.AddrPort
	gen  uint32
	tr   Transport
	mux  *Mux
	conn net.Conn // TCP only

	window   reorderBuffer
	fecwin   recoveryWindow
	lastRecv time.Time // local time of the most recent accepted frame
	gotFrame bool      // at least one frame ever received

	deliver DeliverFunc
	ctr     *clientCounters
	client  *Client

	done chan struct{}
	wg   sync.WaitGroup
	once sync.Once
	log  *slog.Logger
}

// startLoops launches keepalive + watchdog (+ TCP reader is started in dialTCP).
func (s *subscription) startLoops() {
	s.wg.Add(2)
	go s.keepaliveLoop()
	go s.watchdogLoop()
}

// shutdown stops all loops and, if sendBye, sends a BYE. Idempotent.
func (s *subscription) shutdown(sendBye bool) {
	s.once.Do(func() {
		if sendBye {
			s.sendBye()
		}
		close(s.done)
		if s.conn != nil {
			_ = s.conn.Close()
		}
		s.wg.Wait()
	})
}

func (s *subscription) sendBye() {
	if s.tr == TransportTCP {
		if s.conn != nil {
			_ = s.writeControlTCP(TypeBye, 0)
		}
		return
	}
	s.helloUDP(TypeBye, false)
}

// keepaliveLoop re-HELLOs every keepaliveInterval until shutdown.
func (s *subscription) keepaliveLoop() {
	defer s.wg.Done()
	t := time.NewTicker(keepaliveInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			if s.tr == TransportTCP {
				_ = s.writeControlTCP(TypeHello, 0)
			} else {
				s.helloUDP(TypeHello, false)
			}
		}
	}
}

// watchdogLoop trips on starvation (§8.6): first trip -> RESTART; second
// consecutive trip -> give up (Unsubscribe), group self-heal takes over.
func (s *subscription) watchdogLoop() {
	defer s.wg.Done()
	t := time.NewTicker(watchdogTimeout / 2)
	defer t.Stop()
	restarted := false
	for {
		select {
		case <-s.done:
			return
		case now := <-t.C:
			s.mu.Lock()
			got := s.gotFrame
			last := s.lastRecv
			s.mu.Unlock()
			if !got {
				continue // never started; nothing to starve
			}
			if now.Sub(last) <= watchdogTimeout {
				restarted = false
				continue
			}
			if !restarted {
				s.issueRestart()
				restarted = true
				s.mu.Lock()
				s.lastRecv = now // reset deadline so we wait another window
				s.mu.Unlock()
			} else {
				// Still starved after RESTART: the master is gone.
				s.log.Warn("source starved after restart; giving up")
				go s.client.Unsubscribe()
				return
			}
		}
	}
}

func (s *subscription) issueRestart() {
	s.ctr.restarts.Add(1)
	if s.tr == TransportTCP {
		_ = s.writeControlTCP(TypeRestart, FlagPrimeMe)
	} else {
		s.helloUDP(TypeRestart, true)
	}
	s.log.Info("issued RESTART (starvation)")
}

// ingest admits one audio frame (real or FEC-recovered) into the reorder
// window and delivers any now-ordered frames. Takes s.mu.
func (s *subscription) ingest(h Header, payload []byte, real bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if h.Gen < s.gen {
		s.ctr.staleGen.Add(1)
		return
	}
	s.lastRecv = time.Now()
	s.gotFrame = true

	if real {
		// Feed the FEC window; a completed block may recover one frame.
		if rseq, rpts, rpay, ok := s.fecwin.observeData(h.Gen, h.Seq, h.PTS, payload); ok {
			s.admitRecovered(h.Gen, rseq, rpts, rpay)
		}
	}

	deliver, lost, dup, stale := s.window.admit(h.Gen, h.Seq, h.PTS, payload)
	if stale {
		s.ctr.staleGen.Add(1)
		return
	}
	if dup {
		s.ctr.duplicate.Add(1)
	}
	if lost > 0 {
		s.ctr.lost.Add(uint64(lost))
	}
	s.flush(h.Gen, deliver)
}

// ingestParity feeds a parity packet into the FEC window. Takes s.mu.
func (s *subscription) ingestParity(h Header, parity []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h.Gen < s.gen {
		s.ctr.staleGen.Add(1)
		return
	}
	if rseq, rpts, rpay, ok := s.fecwin.observeParity(h.Gen, h.Seq, parity); ok {
		s.admitRecovered(h.Gen, rseq, rpts, rpay)
	}
}

// admitRecovered routes an FEC-recovered frame through the reorder window.
// Caller holds s.mu.
func (s *subscription) admitRecovered(gen uint32, seq uint64, pts int64, payload []byte) {
	s.ctr.recovered.Add(1)
	s.lastRecv = time.Now()
	s.gotFrame = true
	deliver, lost, dup, stale := s.window.admit(gen, seq, pts, payload)
	if stale {
		return
	}
	if dup {
		s.ctr.duplicate.Add(1)
	}
	if lost > 0 {
		s.ctr.lost.Add(uint64(lost))
	}
	s.flush(gen, deliver)
}

// flush delivers ordered frames. Caller holds s.mu (delivery serialized).
func (s *subscription) flush(gen uint32, recs []frameRec) {
	for _, r := range recs {
		h := Header{
			Magic:      Magic,
			Type:       TypeAudio,
			Gen:        gen,
			Seq:        r.seq,
			PTS:        r.pts,
			PayloadLen: uint16(len(r.payload)),
		}
		if s.deliver != nil {
			s.deliver(h, r.payload)
		}
		s.ctr.delivered.Add(1)
	}
}
