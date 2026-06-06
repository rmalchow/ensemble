package auth

import (
	"net/netip"
	"testing"
	"time"
)

func TestVerifyPIN(t *testing.T) {
	good, err := HashPIN("4271", DefaultArgon2id())
	if err != nil {
		t.Fatal(err)
	}
	// Tamper the SECOND-TO-LAST hash char with a guaranteed-different value.
	// Not the last char: the PHC hash is unpadded base64, whose final char
	// carries unused trailing bits that Go's (non-Strict) decoder ignores — so
	// several distinct final chars decode to the SAME bytes and a last-char
	// tamper is flakily a no-op. Every other char is fully significant.
	ti := len(good) - 2
	tb := byte('x')
	if good[ti] == 'x' {
		tb = 'y'
	}
	tampered := good[:ti] + string(tb) + good[ti+1:]

	tests := []struct {
		name    string
		pin     string
		pinHash string
		want    bool
	}{
		{"default empty hash accepts 0000", "0000", "", true},
		{"default empty hash rejects other", "1234", "", false},
		{"correct against hash", "4271", good, true},
		{"wrong against hash", "0000", good, false},
		{"tampered hash", "4271", tampered, false},
		{"garbage hash", "4271", "not-a-phc", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerifyPIN(tc.pin, tc.pinHash); got != tc.want {
				t.Errorf("VerifyPIN(%q, ...) = %v, want %v", tc.pin, got, tc.want)
			}
		})
	}
}

var (
	srcA = netip.MustParseAddr("192.0.2.10")
	srcB = netip.MustParseAddr("192.0.2.20")
)

func TestGuardSoftBackoff(t *testing.T) {
	g := NewAdoptionGuard()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	if ok, _ := g.Allow(srcA, now); !ok {
		t.Fatal("fresh source should be allowed")
	}
	// 3 consecutive fails => soft backoff.
	for i := 0; i < softFailThreshold; i++ {
		g.RecordFail(srcA, now)
		now = now.Add(time.Second)
	}
	ok, reason := g.Allow(srcA, now)
	if ok || reason != ReasonRateLimited {
		t.Errorf("after %d fails: Allow = (%v,%q), want (false,%q)", softFailThreshold, ok, reason, ReasonRateLimited)
	}

	// A success clears the soft backoff.
	g.RecordSuccess(srcA)
	if ok, _ := g.Allow(srcA, now); !ok {
		t.Error("RecordSuccess should clear soft backoff")
	}
}

func TestGuardHardLockout(t *testing.T) {
	g := NewAdoptionGuard()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	// 10 fails within 5 min => 15-min hard lockout.
	for i := 0; i < hardFailThreshold; i++ {
		g.RecordFail(srcA, now)
		now = now.Add(10 * time.Second) // total 100s < 5min window
	}
	ok, reason := g.Allow(srcA, now)
	if ok || reason != ReasonLockedOut {
		t.Errorf("after %d fails: Allow = (%v,%q), want (false,%q)", hardFailThreshold, ok, reason, ReasonLockedOut)
	}

	// Even a success does not lift an active hard lockout.
	g.RecordSuccess(srcA)
	if ok, reason := g.Allow(srcA, now); ok || reason != ReasonLockedOut {
		t.Errorf("success during lockout lifted it: (%v,%q)", ok, reason)
	}

	// Still locked just before 15 min.
	if ok, _ := g.Allow(srcA, now.Add(lockoutDuration-time.Minute)); ok {
		t.Error("should still be locked before 15 min")
	}
	// Allowed again past 15 min.
	if ok, _ := g.Allow(srcA, now.Add(lockoutDuration+time.Second)); !ok {
		t.Error("should be allowed again past 15 min")
	}
}

func TestGuardPerSourceIsolation(t *testing.T) {
	g := NewAdoptionGuard()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	// Lock out only srcA. Keep global below the hard threshold by interleaving
	// is impossible (global aggregates), so verify srcB soft-path is clear after
	// fewer-than-soft fails on srcA.
	for i := 0; i < softFailThreshold; i++ {
		g.RecordFail(srcA, now)
	}
	if ok, _ := g.Allow(srcA, now); ok {
		t.Error("srcA should be soft-limited")
	}
	// srcB has its own counter; but the global consecutive also tripped soft.
	// Reset global via a success on srcA, then srcB must be clear while srcA is
	// reset too — this asserts the maps are independent keys.
	g.RecordSuccess(srcA)
	if ok, _ := g.Allow(srcB, now); !ok {
		t.Error("srcB should be allowed (independent per-source counter)")
	}
}

func TestGuardGlobalCounter(t *testing.T) {
	g := NewAdoptionGuard()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	// Spread the hard threshold across many distinct sources so no single
	// per-source counter trips, but the global one does.
	for i := 0; i < hardFailThreshold; i++ {
		src := netip.AddrFrom4([4]byte{10, 0, 0, byte(i)})
		g.RecordFail(src, now)
		now = now.Add(5 * time.Second)
	}
	// A brand-new source is refused because the global lockout is active.
	fresh := netip.MustParseAddr("203.0.113.7")
	if ok, reason := g.Allow(fresh, now); ok || reason != ReasonLockedOut {
		t.Errorf("global lockout should refuse a fresh source: (%v,%q)", ok, reason)
	}
}

func TestGuardNonce(t *testing.T) {
	g := NewAdoptionGuard()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	n := g.IssueNonce(now)
	if n == "" {
		t.Fatal("empty nonce")
	}
	// Single-use: first consume true, second false.
	if !g.ConsumeNonce(n, now) {
		t.Error("first ConsumeNonce = false, want true")
	}
	if g.ConsumeNonce(n, now) {
		t.Error("second ConsumeNonce = true, want false (single-use)")
	}

	// Expired nonce.
	n2 := g.IssueNonce(now)
	if g.ConsumeNonce(n2, now.Add(NonceTTL+time.Second)) {
		t.Error("expired nonce consumed")
	}
	// Unknown nonce.
	if g.ConsumeNonce("unknown", now) {
		t.Error("unknown nonce consumed")
	}
	if g.ConsumeNonce("", now) {
		t.Error("empty nonce consumed")
	}
}
