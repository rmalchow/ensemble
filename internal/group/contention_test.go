package group

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestTakeoverSingleFlight: K concurrent Do(sameID) call fn exactly once; the
// others observe the leader's committed result (03 §4 single-flight).
func TestTakeoverSingleFlight(t *testing.T) {
	g := NewTakeoverGuard()

	var fnCalls atomic.Int32
	release := make(chan struct{})
	var fn = func() error {
		fnCalls.Add(1)
		<-release // hold the leader so the others pile up behind it
		return nil
	}

	const k = 8
	var wg sync.WaitGroup
	errs := make([]error, k)
	started := make(chan struct{}, k)
	for i := 0; i < k; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			started <- struct{}{}
			errs[i] = g.Do("node-x", nil, fn)
		}(i)
	}
	// Wait until all goroutines have entered Do, then release the leader.
	for i := 0; i < k; i++ {
		<-started
	}
	// Give the followers a moment to block on the leader's done channel.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := fnCalls.Load(); got != 1 {
		t.Errorf("fn called %d times, want 1 (single-flight)", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d err=%v, want nil (observed leader result)", i, err)
		}
	}
}

// TestTakeoverDistinctIDsParallel: Do for different targets is NOT serialized —
// two leaders run concurrently (the guard serializes per target only).
func TestTakeoverDistinctIDsParallel(t *testing.T) {
	g := NewTakeoverGuard()
	bothIn := make(chan struct{}, 2)
	proceed := make(chan struct{})
	fn := func() error {
		bothIn <- struct{}{}
		<-proceed
		return nil
	}
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func(id string) { defer wg.Done(); _ = g.Do(id, nil, fn) }(id)
	}
	// Both fns must enter concurrently; if the guard serialized across ids this
	// would deadlock until the timeout.
	timeout := time.After(time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-bothIn:
		case <-timeout:
			t.Fatal("distinct-id takeovers were serialized (only one fn entered)")
		}
	}
	close(proceed)
	wg.Wait()
}

// TestTakeoverPrecheckAborts: a precheck error (revoked or epoch mismatch) aborts
// before fn runs — no signing (03 §4 / §5 / §2.7).
func TestTakeoverPrecheckAborts(t *testing.T) {
	errRevoked := errors.New("target revoked")
	errEpoch := errors.New("epoch mismatch")
	tests := []struct {
		name     string
		precheck func() error
		wantErr  error
	}{
		{"revoked", func() error { return errRevoked }, errRevoked},
		{"epoch mismatch", func() error { return errEpoch }, errEpoch},
		{"allowed", func() error { return nil }, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewTakeoverGuard()
			var signed atomic.Bool
			err := g.Do("n1", tc.precheck, func() error { signed.Store(true); return nil })
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Do err=%v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil && signed.Load() {
				t.Error("fn ran (signed) despite precheck abort")
			}
			if tc.wantErr == nil && !signed.Load() {
				t.Error("fn did not run despite precheck pass")
			}
		})
	}
}

// TestTakeoverSequentialReuse: after a takeover completes, a later takeover of the
// SAME target starts a fresh single-flight (it runs fn again) rather than
// re-observing the stale prior result — a re-adoption under a new generation must
// re-sign (03 §4).
func TestTakeoverSequentialReuse(t *testing.T) {
	g := NewTakeoverGuard()
	var calls atomic.Int32
	fn := func() error { calls.Add(1); return nil }
	_ = g.Do("n1", nil, fn)
	_ = g.Do("n1", nil, fn)
	if got := calls.Load(); got != 2 {
		t.Errorf("sequential takeovers ran fn %d times, want 2 (fresh single-flight each)", got)
	}
}

// TestTakeoverLeaderError: the leader's error is propagated to the waiters (they
// observe the committed failure, not a false success).
func TestTakeoverLeaderError(t *testing.T) {
	g := NewTakeoverGuard()
	wantErr := errors.New("If-Match conflict")
	release := make(chan struct{})
	fn := func() error { <-release; return wantErr }

	const k = 4
	var wg sync.WaitGroup
	errs := make([]error, k)
	for i := 0; i < k; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = g.Do("n", nil, fn) }(i)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, wantErr) {
			t.Errorf("caller %d err=%v, want %v", i, err, wantErr)
		}
	}
}
