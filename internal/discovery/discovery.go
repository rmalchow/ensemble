// Package discovery registers and browses the _ensemble._tcp mDNS service and
// emits discovered peers on a channel (§3 / §3.1). It is the mDNS half of
// discovery only: it does no gossip joining — the cluster piece (C) consumes
// the deduplicated Peer channel and joins peers itself. The dependency arrow is
// C→B, never B→C: this package imports only stdlib, internal/id, and zeroconf.
package discovery

import (
	"context"
	"log/slog"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"ensemble/internal/id"

	"github.com/grandcat/zeroconf"
)

// ServiceName / Domain are the fixed mDNS coordinates for ensemble (§3).
const (
	ServiceName = "_ensemble._tcp"
	Domain      = "local."
)

// Tunables (package vars so tests can shrink them).
var (
	// reEmitInterval re-emits an unchanged peer for liveness refresh (§2 rule 3).
	reEmitInterval = 30 * time.Second
	// retryInterval is the backoff between register/browse retries (§3 "always on").
	retryInterval = 5 * time.Second
	// peersBuffer is the capacity of the Peers channel (§ edge cases).
	peersBuffer = 64
)

// zeroconfServiceEntry aliases the library entry type so parse.go (and its
// table tests) need not import zeroconf directly.
type zeroconfServiceEntry = zeroconf.ServiceEntry

// Peer is one discovered ensemble node, as advertised in its mDNS TXT record
// (§3 / DUMB-CLIENT §5) plus the address the responder was reached on. It is the
// unit C consumes: a Master peer is gossip-joined; a playback-only peer (D50) is
// represented as a non-gossiping member instead.
//
// A node advertises EITHER role=master (gossips; carries gossip/http/stream/source)
// OR role=playback (wire-driven; carries control + caps). A combined master+playback
// node advertises as master — its local playout is driven in-process (D61), so it is
// just a master to the network.
type Peer struct {
	ID   id.ID      // from TXT "id=" (32 hex); never Zero on a valid Peer
	Addr netip.Addr // chosen responder IP (IPv4 preferred, then IPv6)

	Master   bool   // role=master: gossips
	Playback bool   // role=playback: wire-driven, non-gossiping (D50)
	Name     string // advertised room/speaker name (masters + playback nodes, from TXT)

	// Master fields (zero when !Master).
	GossipPort int // from TXT "gossip="
	HTTPPort   int // from TXT "http="
	StreamPort int // from TXT "stream="
	SourcePort int // from TXT "source=" (§8.7 SOURCE_PORT)

	// Playback fields (zero/empty when !Playback).
	ControlPort int  // from TXT "control=" (master→playback commands, D58)
	Caps        Caps // advertised capabilities (DUMB-CLIENT §5)
}

// Caps are a playback node's advertised capabilities (DUMB-CLIENT §5). They inform
// the master/operator (e.g. a UI warning when assigning a pcm-only speaker to an
// opus group); they never gate membership or fan-out (D51).
type Caps struct {
	Codecs         []string // decodable codecs, preference order ("codecs=pcm,opus")
	MaxRate        int      // max output sample rate ("rate=48000")
	HWVolume       bool     // hardware volume control present ("hwvol=1")
	FixedDelayMs   int      // unchangeable output delay, ms ("delayms=30")
	CanReportQueue bool     // can report output-queue depth → runs the servo ("queue=1")
	Input          bool     // has line-in capture ("input=1")
}

// GossipAddrPort is the address C dials to join the peer's gossip cluster (§3).
// Only meaningful for a Master peer.
func (p Peer) GossipAddrPort() netip.AddrPort {
	return netip.AddrPortFrom(p.Addr, uint16(p.GossipPort))
}

// PlaybackOnly reports whether this peer is a wire-driven, non-gossiping playback
// node (D50): the master ingests it as a member and drives it over the control
// plane rather than gossip-joining it.
func (p Peer) PlaybackOnly() bool {
	return p.Playback && !p.Master && p.ControlPort > 0
}

// Config carries this node's own advertised identity. A node advertises as a
// master (gossip/http/stream/source) when Master is set, else as a playback node
// (control + caps). Master and Playback come from the node's role (D49).
type Config struct {
	ID       id.ID  // this node's immutable node ID (§1)
	Instance string // mDNS instance name; use ID.String() for uniqueness

	Master   bool   // advertise role=master + its ports
	Playback bool   // (when !Master) advertise role=playback + control + caps
	Name     string // advertised room/speaker name (playback-only advert)

	GossipPort int // actually-bound ports (§2 bind-or-increment result)
	HTTPPort   int
	StreamPort int
	SourcePort int // actually-bound SOURCE_PORT (§2/§8.7)

	ControlPort int  // bound CONTROL_PORT (playback-only advert, D58)
	Caps        Caps // advertised capabilities (playback-only advert)

	Log *slog.Logger // optional; defaults to slog.Default() with comp=discovery
}

// seenEntry is per-peer dedup state (§ control flow).
type seenEntry struct {
	peer     Peer      // last emitted value
	lastSeen time.Time // last time we OBSERVED this peer (browse hit)
	emitted  time.Time // last time we EMITTED on the channel
}

// Discovery registers this node over mDNS and continuously browses for peers,
// emitting deduplicated Peer events. One per node. Zero-value is not usable;
// construct with New.
type Discovery struct {
	cfg   Config
	log   *slog.Logger
	peers chan Peer

	// newResolver is a test seam; defaults to zeroconf.NewResolver.
	newResolver func() (*zeroconf.Resolver, error)

	mu     sync.Mutex          // guards seen + server + closed
	seen   map[id.ID]seenEntry // dedup / throttle state, keyed by peer ID
	server *zeroconf.Server    // mDNS registration handle (for Shutdown)
	closed bool
	drops  uint64 // non-blocking sends dropped on a full buffer (diagnostic)

	ctx    context.Context // cancelled by Close
	cancel context.CancelFunc
	wg     sync.WaitGroup // browse + register-refresh goroutines
}

// New constructs a Discovery. It does not touch the network; call Run to start.
func New(cfg Config) *Discovery {
	if cfg.Log == nil {
		cfg.Log = slog.Default().With("comp", "discovery")
	}
	if cfg.Instance == "" {
		cfg.Instance = cfg.ID.String()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Discovery{
		cfg:         cfg,
		log:         cfg.Log,
		peers:       make(chan Peer, peersBuffer),
		newResolver: func() (*zeroconf.Resolver, error) { return zeroconf.NewResolver() },
		seen:        make(map[id.ID]seenEntry),
		ctx:         ctx,
		cancel:      cancel,
	}
}

// Run registers the mDNS service and starts the continuous browse loop in the
// background. It is non-blocking. A registration error is logged and retried
// inside Run's goroutines, not returned, so a transient mDNS failure never
// aborts node startup (§3: "both always on"). Run is called once; calling it
// after Close is a no-op.
func (d *Discovery) Run() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()

	d.wg.Add(2)
	go d.registerKeeper()
	go d.browseKeeper()
}

// Peers returns the receive end of the deduplicated peer-event channel. C ranges
// over it. The channel is closed when Close completes.
func (d *Discovery) Peers() <-chan Peer {
	return d.peers
}

// Close stops browsing, unregisters the mDNS service, waits for goroutines, and
// closes the Peers channel. Idempotent; safe to call once concurrently.
func (d *Discovery) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	server := d.server
	d.mu.Unlock()

	d.cancel()
	if server != nil {
		server.Shutdown()
	}
	d.wg.Wait()
	close(d.peers)
	return nil
}

// txtRecords builds the advertised TXT key=value strings (§3 / DUMB-CLIENT §5).
// A master advertises its four ports; a (non-master) playback node advertises its
// control port + capabilities. The `role` key is always present.
func txtRecords(cfg Config) []string {
	recs := []string{
		"id=" + cfg.ID.String(),
		"role=" + advertRole(cfg),
	}
	if playbackOnly(cfg) {
		// Playback-only advert (D50/D51): control + capabilities, no gossip.
		return append(recs,
			"control="+strconv.Itoa(cfg.ControlPort),
			"name="+cfg.Name,
			"codecs="+strings.Join(cfg.Caps.Codecs, ","),
			"rate="+strconv.Itoa(cfg.Caps.MaxRate),
			"hwvol="+boolBit(cfg.Caps.HWVolume),
			"delayms="+strconv.Itoa(cfg.Caps.FixedDelayMs),
			"queue="+boolBit(cfg.Caps.CanReportQueue),
			"input="+boolBit(cfg.Caps.Input),
		)
	}
	// Master advert (the default for a combined node and any legacy/zero Config).
	// `name` lets a mDNS-only client (the Android companion app) label masters in its
	// picker without an extra /api/status round-trip — matching the playback advert.
	return append(recs,
		"name="+cfg.Name,
		"gossip="+strconv.Itoa(cfg.GossipPort),
		"http="+strconv.Itoa(cfg.HTTPPort),
		"stream="+strconv.Itoa(cfg.StreamPort),
		"source="+strconv.Itoa(cfg.SourcePort),
	)
}

// playbackOnly reports whether the node advertises as a wire-driven playback node:
// it has the playback role and NOT the master role. Everything else (combined,
// master-only, zero/legacy Config) advertises as a master (D61).
func playbackOnly(cfg Config) bool {
	return cfg.Playback && !cfg.Master
}

// advertRole renders the mDNS role from the advert kind.
func advertRole(cfg Config) string {
	if playbackOnly(cfg) {
		return "playback"
	}
	return "master"
}

func boolBit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// srvPort is the (informational) mDNS SRV port: a master's HTTP port, else a
// playback node's control port, else any bound port — never 0 (zeroconf rejects
// "Missing port"). The authoritative ports are always in TXT (D4).
func srvPort(cfg Config) int {
	for _, p := range []int{cfg.HTTPPort, cfg.ControlPort, cfg.StreamPort, cfg.GossipPort, cfg.SourcePort} {
		if p > 0 {
			return p
		}
	}
	return 1 // last-resort placeholder; TXT carries the real ports
}

// registerKeeper registers the mDNS service, retrying on failure until ctx is
// done. zeroconf's Server self-maintains announcements once registered, so this
// goroutine mostly sleeps after the first success; it exists so a *failed*
// initial registration heals (§3).
func (d *Discovery) registerKeeper() {
	defer d.wg.Done()

	txt := txtRecords(d.cfg)
	for {
		if d.ctx.Err() != nil {
			return
		}
		// Advertised SRV port is informational only — real ports are in TXT (D4) —
		// but zeroconf rejects port 0 ("Missing port"). A master advertises its HTTP
		// port; a playback-only node (no HTTP) falls back to its control port.
		// nil ifaces => all multicast-capable interfaces.
		srv, err := zeroconf.Register(d.cfg.Instance, ServiceName, Domain, srvPort(d.cfg), txt, nil)
		if err == nil {
			d.mu.Lock()
			if d.closed {
				// Raced with Close: tear the fresh registration down.
				d.mu.Unlock()
				srv.Shutdown()
				return
			}
			d.server = srv
			d.mu.Unlock()
			d.log.Info("mDNS registered", "instance", d.cfg.Instance)
			d.reannounceLoop(srv, txt)
			return
		}
		d.log.Warn("mDNS register failed, retrying", "err", err)
		if !d.sleep(retryInterval) {
			return
		}
	}
}

// reannounceLoop periodically re-emits the mDNS announcement so a peer stays fresh
// between the responder's sparse RFC6762 bursts (a player node that no longer
// follows anyone still announces, so the master's browse keeps seeing it and
// liveness holds). SetText re-announces the TXT with cache-flush — no Shutdown, so
// no "goodbye"/gap. Complements the control-plane liveness poll (D60). Blocks until
// ctx is done.
func (d *Discovery) reannounceLoop(srv *zeroconf.Server, txt []string) {
	for d.sleep(reEmitInterval) {
		srv.SetText(txt)
	}
}

// browseKeeper runs browseOnce in a loop, restarting on error until ctx is done
// (§3 restart-on-error).
func (d *Discovery) browseKeeper() {
	defer d.wg.Done()

	for {
		if d.ctx.Err() != nil {
			return
		}
		if err := d.browseOnce(); err != nil && d.ctx.Err() == nil {
			d.log.Warn("mDNS browse error, restarting", "err", err)
		}
		if d.ctx.Err() != nil {
			return
		}
		if !d.sleep(retryInterval) {
			return
		}
	}
}

// browseOnce creates a fresh resolver, drains its entries through parseEntry +
// maybeEmit, and blocks in Browse until ctx is cancelled or it errors.
func (d *Discovery) browseOnce() error {
	resolver, err := d.newResolver()
	if err != nil {
		return err
	}

	entries := make(chan *zeroconf.ServiceEntry, 16)
	var drain sync.WaitGroup
	drain.Add(1)
	go func() {
		defer drain.Done()
		for e := range entries {
			if peer, ok := parseEntry(e, d.cfg.ID); ok {
				d.maybeEmit(peer)
			}
		}
	}()

	// Browse closes entries before returning (or we rely on ctx cancellation to
	// unblock it). It owns the entries channel lifecycle.
	err = resolver.Browse(d.ctx, ServiceName, Domain, entries)
	drain.Wait()
	return err
}

// maybeEmit applies the three-rule dedup test (§2) and does a non-blocking send
// on the peers channel. The lock is released before the send.
func (d *Discovery) maybeEmit(peer Peer) {
	now := time.Now()

	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	prev, known := d.seen[peer.ID]

	emit := !known ||
		changed(prev.peer, peer) ||
		now.Sub(prev.emitted) >= reEmitInterval

	prev.lastSeen = now
	if emit {
		prev.peer = peer
		prev.emitted = now
	}
	d.seen[peer.ID] = prev
	d.mu.Unlock()

	if !emit {
		return
	}

	select {
	case d.peers <- peer:
	default:
		d.mu.Lock()
		d.drops++
		drops := d.drops
		d.mu.Unlock()
		d.log.Warn("peers channel full, dropping event (will re-offer)", "peer", peer.ID, "drops", drops)
	}
}

// changed reports whether any material field differs between two Peers with the
// same ID (§2 rule 2).
func changed(a, b Peer) bool {
	return a.Addr != b.Addr ||
		a.Master != b.Master || a.Playback != b.Playback ||
		a.GossipPort != b.GossipPort ||
		a.HTTPPort != b.HTTPPort ||
		a.StreamPort != b.StreamPort ||
		a.SourcePort != b.SourcePort ||
		a.ControlPort != b.ControlPort ||
		a.Name != b.Name ||
		capsChanged(a.Caps, b.Caps)
}

// capsChanged reports whether two advertised capability sets differ.
func capsChanged(a, b Caps) bool {
	return !slices.Equal(a.Codecs, b.Codecs) ||
		a.MaxRate != b.MaxRate ||
		a.HWVolume != b.HWVolume ||
		a.FixedDelayMs != b.FixedDelayMs ||
		a.CanReportQueue != b.CanReportQueue ||
		a.Input != b.Input
}

// sleep waits d or returns false early if ctx is cancelled.
func (d *Discovery) sleep(dur time.Duration) bool {
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-d.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
