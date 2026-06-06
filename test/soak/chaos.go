//go:build soak

package soak

import (
	"context"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
)

// lossyRelay is a loopback UDP relay between an origin and a receiver that injects
// packet loss (uniform and bursty) under test control (doc P7.1 §5.7 injectors).
// The origin sends to the relay's ingress address; the relay forwards each
// datagram to the receiver's address unless the loss policy drops it. It models
// the realtime data plane faithfully (real sockets, real marshaled wire packets)
// rather than a fake in-memory queue (A.4: the test plant must model reality).
type lossyRelay struct {
	ingress *net.UDPConn // origin sends here
	dst     *net.UDPAddr // receiver listens here

	// loss policy (atomic so a chaos goroutine can flip it mid-run).
	lossPct   atomic.Int64 // 0..100 uniform drop percentage
	burstLeft atomic.Int64 // remaining packets to drop in the current burst

	rng   *rand.Rand
	rngMu sync.Mutex

	forwarded atomic.Int64
	dropped   atomic.Int64
}

// newLossyRelay binds the ingress socket and targets dst. seed makes the uniform
// loss deterministic per run.
func newLossyRelay(dst *net.UDPAddr, seed int64) (*lossyRelay, error) {
	in, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return nil, err
	}
	return &lossyRelay{ingress: in, dst: dst, rng: rand.New(rand.NewSource(seed))}, nil
}

// ingressAddr is the address the origin should send to (the relay's ingress).
func (r *lossyRelay) ingressAddr() *net.UDPAddr { return r.ingress.LocalAddr().(*net.UDPAddr) }

// setUniformLoss sets the uniform drop percentage (0..100).
func (r *lossyRelay) setUniformLoss(pct int) { r.lossPct.Store(int64(pct)) }

// injectBurst schedules the next n forwarded packets to be dropped (a contiguous
// burst, to exercise FEC interleave / concealment, A.12 D=4).
func (r *lossyRelay) injectBurst(n int) { r.burstLeft.Store(int64(n)) }

// run forwards datagrams until ctx is cancelled, applying the loss policy.
func (r *lossyRelay) run(ctx context.Context) {
	stop := context.AfterFunc(ctx, func() { _ = r.ingress.Close() })
	defer stop()
	out, err := net.DialUDP("udp", nil, r.dst)
	if err != nil {
		return
	}
	defer out.Close()

	buf := make([]byte, 2048)
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := r.ingress.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		if r.shouldDrop() {
			r.dropped.Add(1)
			continue
		}
		_, _ = out.Write(buf[:n])
		r.forwarded.Add(1)
	}
}

// shouldDrop applies the burst-then-uniform loss policy to one packet.
func (r *lossyRelay) shouldDrop() bool {
	if b := r.burstLeft.Load(); b > 0 {
		r.burstLeft.Store(b - 1)
		return true
	}
	pct := r.lossPct.Load()
	if pct <= 0 {
		return false
	}
	r.rngMu.Lock()
	v := r.rng.Int63n(100)
	r.rngMu.Unlock()
	return v < pct
}

// partition is a controllable connectivity gate between two sides of a cluster
// (doc P7.1 §5.7 partition/heal). A node consults isPartitioned(self, peer) before
// accepting a peer's realtime packets / counting it as an alive member; flipping
// the flag splits or heals the cluster deterministically.
type partition struct {
	mu     sync.Mutex
	split  bool
	sideOf map[string]int // nodeID -> side (0 or 1)
}

func newPartition() *partition { return &partition{sideOf: make(map[string]int)} }

// assign places a node on a side of the (potential) partition.
func (p *partition) assign(nodeID string, side int) {
	p.mu.Lock()
	p.sideOf[nodeID] = side
	p.mu.Unlock()
}

// setSplit splits (true) or heals (false) the partition.
func (p *partition) setSplit(v bool) {
	p.mu.Lock()
	p.split = v
	p.mu.Unlock()
}

// connected reports whether a and b can currently see each other (same side while
// split, or always when healed).
func (p *partition) connected(a, b string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.split {
		return true
	}
	return p.sideOf[a] == p.sideOf[b]
}
