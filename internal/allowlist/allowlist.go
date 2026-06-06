// Package allowlist is the source-IP gate guarding the unauthenticated realtime
// planes (clock UDP :9000, audio UDP/TCP :9100). It is the ONLY protection on
// those planes (03 §0/§6, D2/D5/D6): nothing here authenticates, encrypts, or
// parses payloads — it is a coarse, fail-safe source-IP filter.
//
// The allowed set is the union of every NodeRecord.Addrs in the replicated
// ConfigDoc (durable, from internal/state, survives restart) and the currently
// alive gossip members' observed addresses (live, injected from internal/cluster
// via a liveFn so this package needs no memberlist dependency) — exactly
// 07 §3.1 / A.8. It is held in an atomically swapped snapshot so the per-packet
// check on the recv hot path is lock-free, and is recomputed whenever
// state.Store.Changed() or the membership-changed signal fires (07 §3.2).
//
// Honest limits (03 §6.4, 07 §3.4): this stops off-path / non-member strangers,
// NOT an on-link source-IP spoofer (the realtime planes are unauthenticated by
// design). Stale-doc over-permission (a not-yet-merged forget still listing an
// IP) is bounded by gossip convergence; immediate control-plane cutoff is the
// RevokedSet's job, not this gate's.
//
// Layering (01 §2): allowlist sits beside state, both below cluster's wiring. It
// imports internal/state only; the live half is injected (liveFn / a channel) so
// the package stays unit-testable without a live gossip stack and never forms a
// cyclic dependency on cluster/memberlist.
package allowlist

import (
	"net"
	"net/netip"
	"sync/atomic"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// MemberAddr is one live gossip member's observed source address (07 §3.1). The
// cluster layer (P2.1) supplies these; only the IP is load-bearing for the gate
// (realtime ports are fixed per plane, A.12), so a port is intentionally absent.
type MemberAddr struct {
	Addr netip.Addr
}

// Set is the source-IP gate. It holds an atomically swapped snapshot of the
// allowed IP set so the UDP read path does a lock-free, per-packet lookup
// (07 §3.2, 03 §6.3). The zero value (and a Set fresh from New) denies
// everything until the first Update.
type Set struct {
	cur atomic.Pointer[map[netip.Addr]struct{}]
}

// New returns an empty Set (denies all until Update). Used by cmd wiring and
// tests. A nil snapshot is treated as the empty (deny-all) set by the lookups.
func New() *Set { return &Set{} }

// Allowed reports whether a source IP may send on the realtime planes (03 §6.3).
// Lock-free: reads the current atomic snapshot. An un-parseable/zero ip is
// denied. This is the net.IP convenience form; the recv hot path should prefer
// AllowedAddr to avoid the net.IP boxing.
func (s *Set) Allowed(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	return s.AllowedAddr(addr)
}

// AllowedAddr is the netip.Addr fast path (no net.IP alloc on the hot recv
// path). IPv4-mapped-IPv6 addresses are unmapped before lookup so a v4 source
// surfaced as ::ffff:a.b.c.d matches a natively-stored a.b.c.d and vice versa
// (Q1) — derivation unmaps identically so both sides canonicalize the same way.
func (s *Set) AllowedAddr(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	m := s.cur.Load()
	if m == nil {
		return false
	}
	_, ok := (*m)[ip.Unmap()]
	return ok
}

// Update atomically swaps in a freshly derived snapshot. Called whenever
// ConfigDoc.Nodes OR the live member set changes (07 §3.2). Idempotent: building
// the same inputs yields an equivalent set with no observable disruption to
// concurrent readers (an in-flight reader sees either the old or the new map,
// never a torn one).
func (s *Set) Update(doc state.ConfigDoc, live []MemberAddr) {
	next := AllowedSources(doc, live)
	s.cur.Store(&next)
}

// AllowedSources is the pure derivation function (07 §3.1, A.8): the union of
// every node's ConfigDoc Addrs and the live members' observed addrs. Exported
// and side-effect free so it is unit-testable in isolation.
//
//	allowed = ∪ { netip.ParseAddr(a) : a ∈ n.Addrs, n ∈ doc.Nodes }  // durable
//	        ∪ { m.Addr               : m ∈ live }                     // live
//
// It is a union, not an intersection (07 §3.1): a node may be a valid source
// before its NodeRecord.Addrs catches up (fresh DHCP lease) or while memberlist
// briefly suspects it. A forgotten node has no NodeRecord and so contributes
// nothing (07 §3.1, §4.2) — the whole point of deriving from the doc. Matching
// is host-only (ports are fixed per plane). Un-parseable Addrs are skipped
// silently, never failing the rebuild. All addrs are stored Unmap'd so the
// per-packet lookup (which also unmaps) compares v4/mapped-v4 consistently (Q1).
func AllowedSources(doc state.ConfigDoc, live []MemberAddr) map[netip.Addr]struct{} {
	allowed := make(map[netip.Addr]struct{}, len(live))
	for i := range doc.Nodes {
		for _, a := range doc.Nodes[i].Addrs {
			if addr, err := netip.ParseAddr(a); err == nil {
				allowed[addr.Unmap()] = struct{}{}
			}
		}
	}
	for _, m := range live {
		if m.Addr.IsValid() {
			allowed[m.Addr.Unmap()] = struct{}{}
		}
	}
	return allowed
}
