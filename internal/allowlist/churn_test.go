package allowlist

import (
	"sync"
	"testing"
)

// TestChurnUnionDerivation: Allowed is true iff the IP is in the union of config
// NodeRecord.Addrs and live-member addrs (A.8 / 03 §6.2), false otherwise.
func TestChurnUnionDerivation(t *testing.T) {
	s := New()
	doc := docWithNodes([]string{"10.0.0.1"}) // durable half
	live := []MemberAddr{member(t, "10.0.0.2")}
	s.Update(doc, live)

	tests := []struct {
		ip   string
		want bool
	}{
		{"10.0.0.1", true},  // config half
		{"10.0.0.2", true},  // live half
		{"10.0.0.3", false}, // neither
	}
	for _, tc := range tests {
		if got := s.AllowedAddr(mustAddr(t, tc.ip)); got != tc.want {
			t.Errorf("AllowedAddr(%s)=%v, want %v", tc.ip, got, tc.want)
		}
	}
}

// TestChurnForgetCloses: a forgotten node (dropped from Nodes[] AND no live
// member) is denied, and stays denied across a re-Update — the forget closes the
// IP and keeps it closed (03 §5.3).
func TestChurnForgetCloses(t *testing.T) {
	s := New()
	// Node b is a member with a durable addr and is alive.
	doc := docWithNodes([]string{"10.0.0.1"}, []string{"10.0.0.2"})
	live := []MemberAddr{member(t, "10.0.0.1"), member(t, "10.0.0.2")}
	s.Update(doc, live)
	if !s.AllowedAddr(mustAddr(t, "10.0.0.2")) {
		t.Fatal("pre-forget: 10.0.0.2 should be allowed")
	}

	// Forget node b: it leaves Nodes[] and is no longer a live member (gossip
	// rekey means it cannot re-announce). Only node a remains.
	forgottenDoc := docWithNodes([]string{"10.0.0.1"})
	forgottenLive := []MemberAddr{member(t, "10.0.0.1")}
	s.Update(forgottenDoc, forgottenLive)

	if s.AllowedAddr(mustAddr(t, "10.0.0.2")) {
		t.Error("post-forget: 10.0.0.2 still allowed; want denied")
	}
	// A second Update (e.g. a later membership tick) keeps it denied.
	s.Update(forgottenDoc, forgottenLive)
	if s.AllowedAddr(mustAddr(t, "10.0.0.2")) {
		t.Error("post-forget re-Update: 10.0.0.2 still allowed; want denied")
	}
}

// TestChurnFlapKeepsLiveMember: a member that flaps suspect→alive (transient
// packet loss) keeps its durable NodeRecord.Addrs half, so its IP stays allowed
// across the flap even when the live half momentarily lacks it (03 §6 / 02 §6.1).
func TestChurnFlapKeepsLiveMember(t *testing.T) {
	s := New()
	doc := docWithNodes([]string{"10.0.0.1"}, []string{"10.0.0.2"}) // both durable

	// Both alive.
	s.Update(doc, []MemberAddr{member(t, "10.0.0.1"), member(t, "10.0.0.2")})
	if !s.AllowedAddr(mustAddr(t, "10.0.0.2")) {
		t.Fatal("alive: 10.0.0.2 should be allowed")
	}

	// Node b flaps to suspect: it drops OUT of the live set, but its durable
	// NodeRecord.Addrs entry survives → it stays allowed (no dropped packets).
	s.Update(doc, []MemberAddr{member(t, "10.0.0.1")})
	if !s.AllowedAddr(mustAddr(t, "10.0.0.2")) {
		t.Error("flap suspect: 10.0.0.2 dropped despite durable addr; want allowed")
	}

	// Node b recovers (alive again).
	s.Update(doc, []MemberAddr{member(t, "10.0.0.1"), member(t, "10.0.0.2")})
	if !s.AllowedAddr(mustAddr(t, "10.0.0.2")) {
		t.Error("flap recover: 10.0.0.2 not allowed; want allowed")
	}
}

// TestChurnDHCPChange: a node's new live IP (fresh DHCP lease) is admitted via the
// live half BEFORE its NodeRecord.Addrs is written back (01 §3.4 / 03 §6.2).
func TestChurnDHCPChange(t *testing.T) {
	s := New()
	// Durable record still carries the OLD address; the node is alive on a NEW one.
	doc := docWithNodes([]string{"10.0.0.50"})
	s.Update(doc, []MemberAddr{member(t, "10.0.0.99")}) // new DHCP IP, live only

	if !s.AllowedAddr(mustAddr(t, "10.0.0.99")) {
		t.Error("new DHCP IP not admitted via the live half; want allowed")
	}
	// The old durable IP is also still allowed (union), until the record catches up.
	if !s.AllowedAddr(mustAddr(t, "10.0.0.50")) {
		t.Error("old durable IP denied; want allowed (union, bounded by convergence)")
	}
}

// TestChurnAtomicSwapNoTornRead: concurrent Allowed during Update is safe and
// every single lookup returns a definite result from one complete snapshot — the
// swap is a single atomic pointer store, so a reader sees either the whole old
// map or the whole new map, never a partial union. Run under -race; the assertion
// is "no race, no panic, and a lookup of a member that is in BOTH sets is always
// allowed" (it can never momentarily vanish during the swap).
func TestChurnAtomicSwapNoTornRead(t *testing.T) {
	s := New()
	// "shared" is present in every set the writer installs, so a correct atomic
	// swap means a concurrent lookup of it is ALWAYS true (a torn/partial map could
	// momentarily drop it).
	const shared = "10.0.0.1"
	setA := docWithNodes([]string{shared, "10.1.0.2", "10.1.0.3"})
	setB := docWithNodes([]string{shared, "10.2.0.2", "10.2.0.3"})
	sharedAddr := mustAddr(t, shared)
	s.Update(setA, nil)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				s.Update(setB, nil)
			} else {
				s.Update(setA, nil)
			}
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20000; i++ {
				if !s.AllowedAddr(sharedAddr) {
					t.Errorf("shared IP momentarily denied during swap (torn read)")
					return
				}
			}
		}()
	}

	close(stop)
	wg.Wait()
}
