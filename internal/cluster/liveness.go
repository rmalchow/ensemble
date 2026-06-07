package cluster

import (
	"sync"

	"ensemble/internal/id"
)

// liveness tracks alive/dead + lastSeen, fed by EventDelegate. Separate from the
// replicated Document (§4: liveness is memberlist's, not gossiped). Its own tiny
// mutex (§2.9): written from the memberlist event goroutine and read in
// Snapshot. Lock-order rule: never hold the Cluster doc mutex and this one
// simultaneously; liveness never calls back into the cluster mutex.
type liveness struct {
	mu    sync.Mutex
	alive map[id.ID]bool
	seen  map[id.ID]int64 // unix seconds of last event
}

func newLiveness(self id.ID, now int64) *liveness {
	l := &liveness{
		alive: map[id.ID]bool{},
		seen:  map[id.ID]int64{},
	}
	l.alive[self] = true
	l.seen[self] = now
	return l
}

func (l *liveness) join(peer id.ID, now int64) {
	l.mu.Lock()
	l.alive[peer] = true
	l.seen[peer] = now
	l.mu.Unlock()
}

func (l *liveness) leave(peer id.ID) {
	l.mu.Lock()
	l.alive[peer] = false
	l.mu.Unlock()
}

func (l *liveness) update(peer id.ID, now int64) {
	l.mu.Lock()
	l.alive[peer] = true
	l.seen[peer] = now
	l.mu.Unlock()
}

// snapshot returns copies of the alive + seen maps (safe to read after release).
func (l *liveness) snapshot() (alive map[id.ID]bool, seen map[id.ID]int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	alive = make(map[id.ID]bool, len(l.alive))
	for k, v := range l.alive {
		alive[k] = v
	}
	seen = make(map[id.ID]int64, len(l.seen))
	for k, v := range l.seen {
		seen[k] = v
	}
	return alive, seen
}
