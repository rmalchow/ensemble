package allowlist

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// mustAddr parses s into a netip.Addr or fails the test.
func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return a
}

// member builds a MemberAddr from an IP string.
func member(t *testing.T, s string) MemberAddr {
	t.Helper()
	return MemberAddr{Addr: mustAddr(t, s)}
}

// docWithNodes builds a ConfigDoc whose Nodes carry the given Addrs slices.
func docWithNodes(addrSets ...[]string) state.ConfigDoc {
	var d state.ConfigDoc
	for i, set := range addrSets {
		d.Nodes = append(d.Nodes, state.NodeRecord{ID: itoaTest(i), Addrs: set})
	}
	return d
}

func itoaTest(i int) string { return string(rune('a' + i)) }

func TestAllowedSources(t *testing.T) {
	tests := []struct {
		name      string
		doc       state.ConfigDoc
		live      []MemberAddr
		wantIn    []string // must be Allowed
		wantOut   []string // must NOT be Allowed
		wantCount int      // exact unique-IP count, -1 to skip
	}{
		{
			name:      "empty doc, no live",
			doc:       state.ConfigDoc{},
			live:      nil,
			wantOut:   []string{"192.168.1.1", "::1"},
			wantCount: 0,
		},
		{
			name:      "doc-only with ipv6 and dup",
			doc:       docWithNodes([]string{"192.168.1.21", "fe80::21"}, []string{"192.168.1.21", "192.168.1.22"}),
			wantIn:    []string{"192.168.1.21", "fe80::21", "192.168.1.22"},
			wantOut:   []string{"192.168.1.23"},
			wantCount: 3, // .21 deduped across the two nodes
		},
		{
			name:      "live-only",
			live:      []MemberAddr{{Addr: netip.MustParseAddr("192.168.1.50")}, {Addr: netip.MustParseAddr("192.168.1.51")}},
			wantIn:    []string{"192.168.1.50", "192.168.1.51"},
			wantOut:   []string{"192.168.1.52"},
			wantCount: 2,
		},
		{
			name:      "union doc + live DHCP-fresh",
			doc:       docWithNodes([]string{"192.168.1.21"}),
			live:      []MemberAddr{{Addr: netip.MustParseAddr("192.168.1.99")}},
			wantIn:    []string{"192.168.1.21", "192.168.1.99"},
			wantCount: 2,
		},
		{
			name:      "multi-homed node",
			doc:       docWithNodes([]string{"192.168.1.23", "192.168.1.24"}),
			wantIn:    []string{"192.168.1.23", "192.168.1.24"},
			wantCount: 2,
		},
		{
			name:      "bad addr skipped",
			doc:       docWithNodes([]string{"not-an-ip", "192.168.1.5"}),
			wantIn:    []string{"192.168.1.5"},
			wantOut:   []string{"not-an-ip"}, // not parseable => never allowed
			wantCount: 1,
		},
		{
			name: "forgotten node not allowed",
			// doc no longer carries the node's record, and it is gone from live.
			doc:       docWithNodes([]string{"192.168.1.21"}),
			live:      []MemberAddr{{Addr: netip.MustParseAddr("192.168.1.21")}},
			wantIn:    []string{"192.168.1.21"},
			wantOut:   []string{"192.168.1.42"}, // the forgotten node's old IP
			wantCount: 1,
		},
		{
			name:      "ipv4 mapped normalized to native v4",
			doc:       docWithNodes([]string{"192.168.1.21"}),
			wantIn:    []string{"192.168.1.21", "::ffff:192.168.1.21"}, // mapped form matches
			wantCount: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			s.Update(tc.doc, tc.live)

			if tc.wantCount >= 0 {
				got := len(*s.cur.Load())
				if got != tc.wantCount {
					t.Errorf("unique IP count = %d, want %d", got, tc.wantCount)
				}
			}
			for _, ip := range tc.wantIn {
				if !s.AllowedAddr(mustAddr(t, ip)) {
					t.Errorf("AllowedAddr(%s) = false, want true", ip)
				}
			}
			for _, ip := range tc.wantOut {
				// wantOut may include non-parseable strings; only check parseable ones via AllowedAddr.
				if a, err := netip.ParseAddr(ip); err == nil {
					if s.AllowedAddr(a) {
						t.Errorf("AllowedAddr(%s) = true, want false", ip)
					}
				}
			}
		})
	}
}

func TestAllowedVsAllowedAddr(t *testing.T) {
	s := New()
	s.Update(docWithNodes([]string{"192.168.1.21", "fe80::21"}), nil)

	cases := []struct {
		ip   net.IP
		want bool
	}{
		{net.ParseIP("192.168.1.21"), true},
		{net.ParseIP("fe80::21"), true},
		{net.ParseIP("192.168.1.99"), false},
		{net.IP{}, false},  // invalid
		{nil, false},        // nil slice
		{net.IP{1, 2, 3}, false}, // bogus length
	}
	for _, c := range cases {
		if got := s.Allowed(c.ip); got != c.want {
			t.Errorf("Allowed(%v) = %v, want %v", c.ip, got, c.want)
		}
	}

	// Allowed and AllowedAddr must agree for the same IP.
	ip := net.ParseIP("192.168.1.21")
	addr := mustAddr(t, "192.168.1.21")
	if s.Allowed(ip) != s.AllowedAddr(addr) {
		t.Errorf("Allowed/AllowedAddr disagree for %s", ip)
	}

	// Invalid netip.Addr is denied.
	if s.AllowedAddr(netip.Addr{}) {
		t.Error("AllowedAddr(zero) = true, want false")
	}
}

func TestZeroSetDeniesAll(t *testing.T) {
	// A Set with no Update (nil snapshot) denies everything (07 §3.1 zero value).
	var s Set
	if s.AllowedAddr(mustAddr(t, "192.168.1.1")) {
		t.Error("zero Set allowed an IP")
	}
	if New().AllowedAddr(mustAddr(t, "192.168.1.1")) {
		t.Error("New() Set allowed an IP")
	}
}

func TestUpdateAtomicSwap(t *testing.T) {
	s := New()
	s.Update(docWithNodes([]string{"192.168.1.21"}), nil)
	if !s.AllowedAddr(mustAddr(t, "192.168.1.21")) {
		t.Fatal("initial IP not allowed")
	}

	// Concurrent readers + a swapping writer; -race must see no torn map.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := mustAddr(t, "192.168.1.21")
			b := mustAddr(t, "192.168.1.99")
			for {
				select {
				case <-stop:
					return
				default:
					_ = s.AllowedAddr(a)
					_ = s.AllowedAddr(b)
				}
			}
		}()
	}
	for i := 0; i < 1000; i++ {
		s.Update(docWithNodes([]string{"192.168.1.99"}), nil)
		s.Update(docWithNodes([]string{"192.168.1.21"}), nil)
	}
	close(stop)
	wg.Wait()

	// Final state reflects the last Update.
	if !s.AllowedAddr(mustAddr(t, "192.168.1.21")) {
		t.Error("final snapshot missing .21")
	}
	if s.AllowedAddr(mustAddr(t, "192.168.1.99")) {
		t.Error("final snapshot still has .99")
	}
}

// waitAllowed polls until AllowedAddr(ip)==want or the deadline passes.
func waitAllowed(t *testing.T, s *Set, ip netip.Addr, want bool) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.AllowedAddr(ip) == want {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func TestRunStartupPriming(t *testing.T) {
	store := state.New("self")
	if _, err := store.Apply(docWithNodes([]string{"192.168.1.21"})); err != nil {
		t.Fatalf("apply: %v", err)
	}

	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	memberCh := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { done <- Run(ctx, s, store, memberCh, func() []MemberAddr { return nil }) }()

	// Run must prime the set before any signal: the doc IP becomes allowed
	// without us touching either channel.
	if !waitAllowed(t, s, mustAddr(t, "192.168.1.21"), true) {
		t.Fatal("Run did not prime the set at startup")
	}

	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel (goroutine leak)")
	}
}

func TestRunRebuildOnStoreChanged(t *testing.T) {
	store := state.New("self")
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	memberCh := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { done <- Run(ctx, s, store, memberCh, func() []MemberAddr { return nil }) }()

	// Apply a doc adding an IP; Run must rebuild and allow it without restart.
	if _, err := store.Apply(docWithNodes([]string{"192.168.1.77"})); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !waitAllowed(t, s, mustAddr(t, "192.168.1.77"), true) {
		t.Fatal("Run did not rebuild on store.Changed()")
	}

	cancel()
	<-done
}

func TestRunRebuildOnMembershipSignal(t *testing.T) {
	store := state.New("self")
	s := New()

	var mu sync.Mutex
	liveIPs := []MemberAddr{}
	liveFn := func() []MemberAddr {
		mu.Lock()
		defer mu.Unlock()
		out := make([]MemberAddr, len(liveIPs))
		copy(out, liveIPs)
		return out
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	memberCh := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { done <- Run(ctx, s, store, memberCh, liveFn) }()

	// Add a live member and pulse the membership signal.
	mu.Lock()
	liveIPs = []MemberAddr{{Addr: netip.MustParseAddr("192.168.1.88")}}
	mu.Unlock()
	memberCh <- struct{}{}

	if !waitAllowed(t, s, mustAddr(t, "192.168.1.88"), true) {
		t.Fatal("Run did not rebuild on membership signal")
	}

	cancel()
	<-done
}

func TestRunCoalescing(t *testing.T) {
	store := state.New("self")
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	memberCh := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { done <- Run(ctx, s, store, memberCh, func() []MemberAddr { return nil }) }()

	// Rapid double-signal must not deadlock; at least one rebuild lands.
	if _, err := store.Apply(docWithNodes([]string{"192.168.1.10"})); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := store.Apply(state.ConfigDoc{Version: 1, Nodes: []state.NodeRecord{{ID: "a", Addrs: []string{"192.168.1.10", "192.168.1.11"}}}}); err != nil {
		t.Fatalf("apply2: %v", err)
	}
	if !waitAllowed(t, s, mustAddr(t, "192.168.1.11"), true) {
		t.Fatal("Run did not converge after coalesced signals")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run deadlocked")
	}
}
