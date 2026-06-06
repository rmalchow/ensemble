package adopt

import (
	"errors"
	"testing"
	"time"
)

// clock is an injectable time source for deterministic guard windows.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *clock { return &clock{t: time.Unix(1_700_000_000, 0)} }

func TestSoftBackoffAfter3(t *testing.T) {
	c := newClock()
	g := NewGuard(DefaultGuardParams(), c.now)
	const src = "10.0.0.1"

	if ok, _, _ := g.Allow(src); !ok {
		t.Fatal("first attempt should be allowed")
	}
	for i := 0; i < 3; i++ {
		g.RecordFail(src)
	}
	ok, retry, err := g.Allow(src)
	if ok {
		t.Fatal("expected soft backoff after 3 consecutive fails")
	}
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if retry <= 0 {
		t.Fatalf("retryAfter = %v, want > 0", retry)
	}
}

func TestHardLockout10Per5min(t *testing.T) {
	c := newClock()
	g := NewGuard(DefaultGuardParams(), c.now)
	const src = "10.0.0.2"

	for i := 0; i < 10; i++ {
		g.RecordFail(src)
		c.add(10 * time.Second) // 10 fails over ~100 s, well inside the 5-min window
	}
	ok, retry, err := g.Allow(src)
	if ok {
		t.Fatal("expected hard lockout after 10 fails / 5 min")
	}
	if !errors.Is(err, ErrLockedOut) {
		t.Fatalf("err = %v, want ErrLockedOut", err)
	}
	if retry <= 0 || retry > 15*time.Minute {
		t.Fatalf("retryAfter = %v, want in (0, 15m]", retry)
	}
	// Still locked just before 15 min.
	c.add(15*time.Minute - time.Second)
	if ok, _, _ := g.Allow(src); ok {
		t.Fatal("should still be locked just before 15 min")
	}
	// Unlocked after 15 min + epsilon. RecordSuccess clears the consecutive count
	// the fails also accrued (otherwise soft backoff would still bite).
	c.add(2 * time.Second)
	g.RecordSuccess(src)
	if ok, _, _ := g.Allow(src); !ok {
		t.Fatal("should be unlocked after 15 min")
	}
}

func TestSuccessResetsCounters(t *testing.T) {
	c := newClock()
	g := NewGuard(DefaultGuardParams(), c.now)
	const src = "10.0.0.3"

	g.RecordFail(src)
	g.RecordFail(src)
	g.RecordFail(src)
	if ok, _, _ := g.Allow(src); ok {
		t.Fatal("expected backoff after 3 fails")
	}
	g.RecordSuccess(src)
	if ok, _, _ := g.Allow(src); !ok {
		t.Fatal("RecordSuccess should clear soft backoff")
	}
}

func TestNonceTTL30s(t *testing.T) {
	c := newClock()
	g := NewGuard(DefaultGuardParams(), c.now)

	// Within TTL: consume once OK, twice -> unknown (single-use burn).
	n1 := g.IssueNonce()
	if err := g.ConsumeNonce(n1); err != nil {
		t.Fatalf("consume within TTL: %v", err)
	}
	if err := g.ConsumeNonce(n1); !errors.Is(err, ErrNonceUnknown) {
		t.Fatalf("second consume err = %v, want ErrNonceUnknown", err)
	}

	// Past TTL: expired.
	n2 := g.IssueNonce()
	c.add(NonceSessionTTL + time.Second)
	if err := g.ConsumeNonce(n2); !errors.Is(err, ErrNonceExpired) {
		t.Fatalf("expired consume err = %v, want ErrNonceExpired", err)
	}
}

func TestPerSourceAndGlobal(t *testing.T) {
	c := newClock()
	g := NewGuard(DefaultGuardParams(), c.now)

	// 10 fails spread across distinct sources (1 each) never trip a per-source
	// lockout but DO accumulate to the global cap -> any source is then locked out.
	for i := 0; i < 10; i++ {
		src := "10.1.0." + string(rune('0'+i))
		g.RecordFail(src)
		c.add(5 * time.Second)
	}
	// A brand-new source is refused by the global lockout.
	ok, _, err := g.Allow("10.9.9.9")
	if ok {
		t.Fatal("global cap should lock out even a fresh source")
	}
	if !errors.Is(err, ErrLockedOut) {
		t.Fatalf("err = %v, want ErrLockedOut", err)
	}
}

func TestUnknownNonceRejected(t *testing.T) {
	g := NewGuard(DefaultGuardParams(), nil)
	if err := g.ConsumeNonce([]byte("never-issued")); !errors.Is(err, ErrNonceUnknown) {
		t.Fatalf("err = %v, want ErrNonceUnknown", err)
	}
	if err := g.ConsumeNonce(nil); !errors.Is(err, ErrNonceUnknown) {
		t.Fatalf("nil nonce err = %v, want ErrNonceUnknown", err)
	}
}

func TestDefaultGuardParamsMatchA12(t *testing.T) {
	p := DefaultGuardParams()
	want := GuardParams{
		SoftBackoffAfter: 3,
		HardFailCount:    10,
		HardFailWindow:   5 * time.Minute,
		HardLockout:      15 * time.Minute,
		NonceTTL:         30 * time.Second,
	}
	if p != want {
		t.Fatalf("DefaultGuardParams = %+v, want %+v (A.12)", p, want)
	}
}
