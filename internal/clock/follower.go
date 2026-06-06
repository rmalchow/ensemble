package clock

// Copied near-verbatim from gitlab.rand0m.me/ruben/go/media/internal/clock/
// follower.go. The only ensemble addition is WithEstimator, which lets cmd/group
// pass A.12's (window, alpha) — (8,0.15) wired / (16,0.10) WiFi — without exposing
// Estimator construction. The hard-coded 250ms/(8,0.15)/200ms defaults are only
// fallbacks; the caller drives the A.12 numbers explicitly (P3.1 §3/§5.3).

import (
	"context"
	"net"
	"sync"
	"time"
)

// Follower periodically exchanges timestamps with a master clock Server and
// maintains a smoothed offset estimate. It is safe for concurrent use: the sync
// loop reads Offset/Now from another goroutine while Run drives the exchanges.
type Follower struct {
	interval time.Duration
	timeout  time.Duration

	mu  sync.Mutex
	est *Estimator
	seq uint64

	// onSample, if set, is called for each completed exchange (used by harness).
	onSample func(Sample, time.Duration)
}

// FollowerOption configures a Follower.
type FollowerOption func(*Follower)

// WithInterval sets the ping interval. A.12: 1s steady, 500ms during the initial
// lock — the cadence switch is the engine's (internal/group); this is the knob.
func WithInterval(d time.Duration) FollowerOption {
	return func(f *Follower) { f.interval = d }
}

// WithTimeout sets the per-ping reply timeout. A.12: 200ms RPC timeout.
func WithTimeout(d time.Duration) FollowerOption {
	return func(f *Follower) { f.timeout = d }
}

// WithSampleHook registers a callback invoked after each completed exchange with
// the raw sample and the resulting smoothed offset.
func WithSampleHook(fn func(Sample, time.Duration)) FollowerOption {
	return func(f *Follower) { f.onSample = fn }
}

// WithEstimator sets the min-delay filter window and EWMA alpha from A.12:
// (8, 0.15) wired / (16, 0.10) WiFi. Out-of-range args fall back to NewEstimator's
// defaults. This is the only public symbol ensemble adds to the reused types.
func WithEstimator(window int, alpha float64) FollowerOption {
	return func(f *Follower) { f.est = NewEstimator(window, alpha) }
}

// NewFollower creates a Follower. Defaults (interval 250ms, timeout 200ms,
// estimator window 8 / alpha 0.15) are mpvsync fallbacks; ensemble overrides
// interval/timeout/window/alpha from A.12 via the options.
func NewFollower(opts ...FollowerOption) *Follower {
	f := &Follower{
		interval: 250 * time.Millisecond,
		timeout:  200 * time.Millisecond,
		est:      NewEstimator(8, 0.15),
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

// Offset returns the current smoothed clock offset (master - follower) and
// whether an estimate is available yet.
func (f *Follower) Offset() (time.Duration, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.est.Offset()
}

// MasterNow maps the current local monotonic instant to the master's clock:
// follower_mono + Offset (doc 04 §4.4.3). ok is false until an estimate exists.
func (f *Follower) MasterNow() (int64, bool) {
	f.mu.Lock()
	off, ok := f.est.Offset()
	f.mu.Unlock()
	return nowMono() + off.Nanoseconds(), ok
}

// SamplesSeen returns the total number of completed exchanges.
func (f *Follower) SamplesSeen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.est.Samples()
}

// MinDelay returns the current best round-trip delay (sync-quality proxy).
func (f *Follower) MinDelay() (time.Duration, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.est.MinDelay()
}

// Run drives the exchange loop against masterAddr until ctx is cancelled.
func (f *Follower) Run(ctx context.Context, masterAddr string) error {
	raddr, err := net.ResolveUDPAddr("udp", masterAddr)
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Cancel the loop promptly when ctx is done by unblocking the socket read.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()

	buf := make([]byte, PacketSize)
	for {
		f.exchange(conn, buf)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// exchange performs one ping/reply round and updates the estimator. A lost or
// timed-out reply is simply skipped; a foreign/stale seq is rejected (the
// follower only ever accepts replies from the one master it dialed, doc 04 §4.1.2).
func (f *Follower) exchange(conn *net.UDPConn, buf []byte) {
	f.mu.Lock()
	f.seq++
	seq := f.seq
	f.mu.Unlock()

	req := packet{kind: kindRequest, seq: seq, t1: nowMono()}
	if _, err := conn.Write(req.marshal()); err != nil {
		return
	}

	_ = conn.SetReadDeadline(time.Now().Add(f.timeout))
	for {
		n, err := conn.Read(buf)
		t4 := nowMono()
		if err != nil {
			return // timeout or closed
		}
		rep, err := unmarshal(buf[:n])
		if err != nil || rep.kind != kindReply || rep.seq != seq {
			continue // stale/foreign reply; keep waiting within the deadline
		}
		s := computeSample(rep.t1, rep.t2, rep.t3, t4)

		f.mu.Lock()
		smoothed := f.est.Add(s)
		hook := f.onSample
		f.mu.Unlock()
		if hook != nil {
			hook(s, smoothed)
		}
		return
	}
}
