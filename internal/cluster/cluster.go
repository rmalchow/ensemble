// Package cluster wraps memberlist gossip and holds the replicated LWW cluster
// document, observed-IP tracking, group derivation, and change notifications
// (§3/§3.1/§4/§5). Implemented by piece C.
package cluster

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"

	"ensemble/internal/contracts"
	"ensemble/internal/discovery"
	"ensemble/internal/id"
)

// observeBroadcastInterval throttles re-broadcasting the same (peer, ip)
// observation to avoid gossip churn from every HTTP request (§3.1).
const observeBroadcastInterval = 30 * time.Second

// staleAfter marks a node "stale" in the snapshot when its last liveness event
// is older than this (UI hint, §9.1).
const staleAfter = 60 * time.Second

// Cluster is the gossip-backed replicated state for one node. It implements
// contracts.StateStore. One per process.
type Cluster struct {
	self id.ID
	log  *slog.Logger

	mu       sync.Mutex      // ONE mutex: guards doc, observed, subs, closed
	doc      *Document       // the replicated LWW document (own + peers)
	observed observedTable   // peerID -> last broadcast time (throttle bookkeeping)
	subs     []chan struct{} // Subscribe() coalesced notify channels
	closed   bool

	live *liveness // alive/lastSeen from memberlist events (own lock)

	ml    *memberlist.Memberlist
	queue *memberlist.TransmitLimitedQueue
	deleg *delegate
	mlCfg *memberlist.Config

	peers      <-chan discovery.Peer
	clock      func() time.Time
	purgeEvery time.Duration
	maxAge     time.Duration

	done chan struct{}
	wg   sync.WaitGroup
}

// observedTable holds throttle bookkeeping for our own observations: the last
// time we broadcast a (peer, ip) observation. The authoritative observed map is
// in our own NodeRecord.Observed.
type observedTable map[id.ID]obsThrottle

type obsThrottle struct {
	ip            string
	lastBroadcast time.Time
}

// Config wires the cluster. All ports/addrs are the ACTUALLY-bound values from
// netx (K passes them after bind-or-increment, §2).
type Config struct {
	Self          id.ID
	Name          string
	Volume        float64
	OutputDelayMs int
	Caps          contracts.Capabilities
	Addrs         []string
	HTTPPort      int
	StreamPort    int
	SourcePort    int
	GossipPort    int
	BindAddr      string
	Peers         <-chan discovery.Peer
	Logger        *slog.Logger

	// Optional test hooks (nil → production defaults).
	Now        func() time.Time
	PurgeEvery time.Duration
	MaxAge     time.Duration
}

// New builds the Cluster and its memberlist config, seeding this node's own
// record (version 1) from cfg. It does NOT start networking; call Start.
func New(cfg Config) (*Cluster, error) {
	if cfg.Self.IsZero() {
		return nil, errors.New("cluster: Config.Self is zero")
	}
	if cfg.GossipPort <= 0 {
		return nil, errors.New("cluster: Config.GossipPort must be > 0")
	}

	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("comp", "cluster")

	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	purgeEvery := cfg.PurgeEvery
	if purgeEvery == 0 {
		purgeEvery = time.Hour
	}
	maxAge := cfg.MaxAge
	if maxAge == 0 {
		maxAge = 30 * 24 * time.Hour
	}
	bindAddr := cfg.BindAddr
	if bindAddr == "" {
		bindAddr = "0.0.0.0"
	}

	nowUnix := now().Unix()
	doc := newDocument()
	doc.Nodes[cfg.Self] = &NodeRecord{
		ID:            cfg.Self,
		Name:          cfg.Name,
		Volume:        cfg.Volume,
		OutputDelayMs: cfg.OutputDelayMs,
		Addrs:         append([]string(nil), cfg.Addrs...),
		HTTPPort:      cfg.HTTPPort,
		StreamPort:    cfg.StreamPort,
		SourcePort:    cfg.SourcePort,
		GossipPort:    cfg.GossipPort,
		Caps:          cfg.Caps,
		Following:     id.Zero,
		Observed:      map[id.ID]obsEntry{},
		Version:       1,
		UpdatedAt:     nowUnix,
	}

	c := &Cluster{
		self:       cfg.Self,
		log:        log,
		doc:        doc,
		observed:   observedTable{},
		live:       newLiveness(cfg.Self, nowUnix),
		peers:      cfg.Peers,
		clock:      now,
		purgeEvery: purgeEvery,
		maxAge:     maxAge,
		done:       make(chan struct{}),
	}

	c.deleg = &delegate{c: c}
	c.queue = &memberlist.TransmitLimitedQueue{
		NumNodes:       func() int { return 1 },
		RetransmitMult: 4,
	}

	ml := memberlist.DefaultLANConfig()
	ml.Name = cfg.Self.String()
	ml.BindAddr = bindAddr
	ml.BindPort = cfg.GossipPort
	ml.AdvertisePort = cfg.GossipPort
	ml.Delegate = c.deleg
	ml.Events = c.deleg
	// Route memberlist's verbose stdlib logging into our slog at debug, rather
	// than letting it write directly to os.Stderr.
	ml.LogOutput = &slogWriter{log: log}
	ml.Logger = nil
	c.mlCfg = ml

	return c, nil
}

// Start creates the memberlist, begins gossiping, and launches the join loop
// (drains cfg.Peers) and the hourly purge loop. Non-blocking.
func (c *Cluster) Start() error {
	ml, err := memberlist.Create(c.mlCfg)
	if err != nil {
		return fmt.Errorf("cluster: memberlist create: %w", err)
	}
	c.ml = ml
	c.queue.NumNodes = func() int { return ml.NumMembers() }

	if c.peers != nil {
		c.wg.Add(1)
		go c.joinLoop()
	}
	c.wg.Add(1)
	go c.purgeLoop()
	return nil
}

// Self returns this node's immutable id.
func (c *Cluster) Self() id.ID { return c.self }

// Close leaves the cluster gracefully and stops the goroutines. Idempotent.
func (c *Cluster) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	close(c.done)

	if c.ml != nil {
		if err := c.ml.Leave(5 * time.Second); err != nil {
			c.log.Warn("memberlist leave", "err", err)
		}
		if err := c.ml.Shutdown(); err != nil {
			c.log.Warn("memberlist shutdown", "err", err)
		}
	}

	c.wg.Wait()

	c.mu.Lock()
	for _, ch := range c.subs {
		close(ch)
	}
	c.subs = nil
	c.mu.Unlock()
	return nil
}

// Subscribe returns a coalesced change channel (buffer 1, non-blocking sends).
// Signaled on every applied document or liveness change. Closed on Close.
func (c *Cluster) Subscribe() <-chan struct{} {
	ch := make(chan struct{}, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		close(ch)
		return ch
	}
	c.subs = append(c.subs, ch)
	c.mu.Unlock()
	return ch
}

// notify does a non-blocking, coalesced send to every subscriber. Must be
// called WITHOUT holding c.mu (it takes the mutex to read subs).
func (c *Cluster) notify() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	subs := c.subs
	c.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// enqueueBroadcast pushes a single-record delta onto the gossip queue.
func (c *Cluster) enqueueBroadcast(kind byte, key id.ID, d delta) {
	c.queue.QueueBroadcast(&broadcast{
		key: broadcastKey(kind, key),
		msg: encodeDelta(kind, d),
	})
}

// joinLoop drains the discovery Peer channel and joins each new peer's gossip
// endpoint. Best-effort; mDNS re-emits so a transient failure self-heals.
func (c *Cluster) joinLoop() {
	defer c.wg.Done()
	for {
		select {
		case <-c.done:
			return
		case p, ok := <-c.peers:
			if !ok {
				return
			}
			c.tryJoin(p)
		}
	}
}

func (c *Cluster) tryJoin(p discovery.Peer) {
	if p.ID == c.self {
		return
	}
	for _, m := range c.ml.Members() {
		if m.Name == p.ID.String() {
			return // already a member
		}
	}
	addr := p.GossipAddrPort().String()
	if _, err := c.ml.Join([]string{addr}); err != nil {
		c.log.Debug("gossip join failed", "peer", p.ID, "addr", addr, "err", err)
	}
}

// Join seeds gossip with an explicit host:gossipPort list (dev --join, D20).
func (c *Cluster) Join(addrs []string) error {
	if c.ml == nil {
		return errors.New("cluster: Join before Start")
	}
	if len(addrs) == 0 {
		return nil
	}
	_, err := c.ml.Join(addrs)
	return err
}

// purgeLoop deletes records older than maxAge (§4: 30 days), checked hourly.
func (c *Cluster) purgeLoop() {
	defer c.wg.Done()
	t := time.NewTicker(c.purgeEvery)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			c.runPurge()
		}
	}
}

func (c *Cluster) runPurge() {
	alive, _ := c.live.snapshot()
	cutoff := c.clock().Add(-c.maxAge).Unix()
	c.mu.Lock()
	removed := c.doc.purge(c.self, cutoff, alive)
	c.mu.Unlock()
	if removed {
		c.notify()
	}
}

// reconcileOwnVersion implements D7: after the first push/pull, if a peer holds
// our own record at version >= ours, jump our counter above it and re-broadcast.
// Called by the delegate after MergeRemoteState. peerVer is the version a remote
// document carried for our id.
func (c *Cluster) reconcileOwnVersion(peerVer uint64) {
	c.mu.Lock()
	own := c.doc.Nodes[c.self]
	if own == nil || peerVer < own.Version {
		c.mu.Unlock()
		return
	}
	own.Version = peerVer + 1
	own.UpdatedAt = c.clock().Unix()
	d := delta{Node: cloneNode(own)}
	c.mu.Unlock()
	c.enqueueBroadcast(kindNodeDelta, c.self, d)
	c.notify()
}

// slogWriter adapts memberlist's io.Writer-based logging into slog at debug.
type slogWriter struct{ log *slog.Logger }

func (w *slogWriter) Write(p []byte) (int, error) {
	w.log.Debug("memberlist", "msg", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
