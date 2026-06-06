//go:build soak

// Package soak is the in-process chaos + soak harness for the Ensemble realtime
// plane (A.13 P7). It is opt-in behind the `soak` build tag, so it is excluded
// from the default `go test ./...` and the cgo-free arm64 cross-build; run it with
//
//	go test -tags soak ./test/soak/...
//
// The harness composes the REAL ensemble packages in-process on loopback (no
// external process, no hardware): a synthetic deterministic source.Reader, a real
// origin.Origin per master, a real sink_net.Receiver per follower over real
// loopback UDP/TCP, real cluster.GroupElections, real allowlist.Set, real
// streamgen gates. It samples each follower's projected timeline against the
// master timeline and asserts sync holds (sub-ms) at steady state and after each
// induced fault, the failover gap is bounded, and there is no goroutine/FD leak
// across churn cycles. The full 24 h soak (A.13 P7) is the manual Pi+DAC
// acceptance gate; this is the compressed deterministic CI proxy (doc P7.1 §5.7).
package soak

import (
	"runtime"
	"time"
)

// syncError is the absolute frame-domain difference between a follower's
// projected NowSample and the master's NowSample at the same instant, converted
// to a wall-clock duration at the canonical rate. It is the inter-node sync error
// the A.13 P5/P7 acceptance bounds to sub-ms.
func syncError(masterSample, followerSample int64, rate int) time.Duration {
	d := masterSample - followerSample
	if d < 0 {
		d = -d
	}
	return time.Duration(d) * time.Second / time.Duration(rate)
}

// sampler tracks the worst observed sync error across a run for one follower.
type sampler struct {
	rate  int
	worst time.Duration
	count int
}

func newSampler(rate int) *sampler { return &sampler{rate: rate} }

// observe records one (master, follower) sample pair, updating the worst-case
// sync error. Pairs where the follower is not yet locked (ok=false) are skipped
// (they are the bounded re-prime window, not a steady-state desync).
func (s *sampler) observe(masterSample, followerSample int64, followerOK bool) {
	if !followerOK {
		return
	}
	e := syncError(masterSample, followerSample, s.rate)
	if e > s.worst {
		s.worst = e
	}
	s.count++
}

// goroutineCount returns the current goroutine count, used by the leak check
// across thousands of failover/flap cycles (doc P7.1 §5.7 / §7.7). Callers settle
// (GC + a brief pause) before sampling so transient teardown goroutines drain.
func goroutineCount() int {
	settle()
	return runtime.NumGoroutine()
}

// settle lets in-flight teardown goroutines exit and runs a GC so the leak check
// compares quiescent counts.
func settle() {
	for i := 0; i < 3; i++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
}
