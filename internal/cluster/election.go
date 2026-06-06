package cluster

import "sync"

// Election picks the master (clock/stream-origin reference) for one group via
// the A.5 rule: the soft MasterHint if it is an alive candidate, else the lowest
// stable node id among alive members.
//
// This is intentionally STATELESS — the master is a pure function of the current
// member set (and the agreed hint), so every node that sees the same membership
// agrees on the same master. (An earlier design kept the incumbent "sticky" to
// avoid re-elections, but local stickiness is inconsistent across nodes: a node
// that was master while alone would keep itself even after a lower-id peer
// joined, while peers that joined later picked the lower id — a split brain.
// Consistency wins; the cost is a master change, hence a clock re-baseline,
// whenever a lower-id node appears, which only happens on a topology change.)
// The soft MasterHint reintroduces operator-chosen stickiness without breaking
// statelessness: the hint is part of the agreed ConfigDoc, so all nodes apply
// it identically (A.5).
//
// memberlist's own failure detection debounces membership, so a single elected
// master does not flap under normal churn. Each master change bumps Generation
// so downstream consumers (clock re-baseline, stream-origin streamGen) can fence
// stale masters (02 §5.3 / A.5).
type Election struct {
	selfID string

	mu         sync.Mutex
	masterID   string
	generation uint64
}

// NewElection creates an election for the node with the given stable id.
func NewElection(selfID string) *Election {
	return &Election{selfID: selfID}
}

// Update recomputes the master over the given alive candidates honoring the soft
// hint: if hint is a non-empty candidate id it wins, otherwise the lowest stable
// id wins (A.5). Empty candidate ids are skipped. With no candidates the master
// is "" (NO-MASTER / starting). It returns the elected master id and whether it
// changed from the previous master; Generation bumps on (and only on) a change.
func (e *Election) Update(members []Member, hint string) (masterID string, changed bool) {
	lowest := ""
	hintAlive := false
	for _, m := range members {
		id := m.Meta.NodeID
		if id == "" {
			continue
		}
		if id == hint {
			hintAlive = true
		}
		if lowest == "" || id < lowest {
			lowest = id
		}
	}

	elected := lowest
	if hintAlive {
		elected = hint
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if elected != e.masterID {
		e.masterID = elected
		e.generation++
		return e.masterID, true
	}
	return e.masterID, false
}

// Master returns the current elected master id.
func (e *Election) Master() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.masterID
}

// Generation returns a counter that increments on every master change.
func (e *Election) Generation() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.generation
}

// IsMaster reports whether this node is the elected master.
func (e *Election) IsMaster() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.masterID != "" && e.masterID == e.selfID
}
