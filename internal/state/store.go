package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrConflict is returned by Apply when the submitted version is not the current
// version (a concurrent edit landed first); the caller should refetch and retry
// (07 §4.5, served as HTTP 409 behind If-Match by web, §6.6).
var ErrConflict = errors.New("config version conflict")

// Store is the concurrency-safe holder of the replicated ConfigDoc. The zero
// value is not usable; construct with New or Load.
//
// When constructed with Load, the doc is persisted to disk (path) on every
// change so config survives a full cluster restart; gossip last-writer-wins then
// reconciles the highest version across the cluster on rejoin. A Store built
// with New has an empty path and never touches disk (used by tests and
// non-persistent callers). Persistence is always best-effort: a write error is
// logged to stderr and never fails Apply/Merge.
type Store struct {
	mu      sync.Mutex
	selfID  string
	doc     ConfigDoc
	changed chan struct{}

	// path is the on-disk config.json location ("" => no persistence). saveMu
	// serializes the file writes (which happen OUTSIDE mu, on a doc snapshot
	// taken under mu) so saving never deadlocks against Get/Apply/Merge.
	path   string
	saveMu sync.Mutex
}

// New returns an empty, non-persistent Store. selfID is this node's id, used as
// the Apply provenance (UpdatedBy) and the Merge tiebreaker when two replicas
// carry the same non-zero Version.
func New(selfID string) *Store {
	return &Store{
		selfID:  selfID,
		changed: make(chan struct{}, 1),
	}
}

// Load returns a Store that persists its doc to path (config.json) on every
// change. If path holds a readable, well-formed config.json it seeds the
// in-memory doc with it; a missing or corrupt file is non-fatal and yields an
// empty doc (gossip will reconcile the cluster's highest version on rejoin). An
// empty path falls back to New (no persistence). The caller (cmd) supplies
// <configdir>/ensemble/config.json (07 §5.4).
func Load(selfID, path string) *Store {
	s := New(selfID)
	if path == "" {
		return s
	}
	s.path = path
	if b, err := os.ReadFile(path); err == nil {
		var d ConfigDoc
		if json.Unmarshal(b, &d) == nil {
			s.doc = cloneConfigDoc(d)
		}
	}
	return s
}

// Get returns a deep copy of the current document (safe to mutate by the caller).
func (s *Store) Get() ConfigDoc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneConfigDoc(s.doc)
}

// Apply accepts an optimistic update. It requires update.Version to equal the
// current version; otherwise it returns ErrConflict and leaves the store
// unchanged. On success it stores the update, bumps the version, stamps
// UpdatedBy=selfID and UpdatedAt=now (A.6), signals Changed, and returns the
// stored (post-bump) document. UpdatedAt is human/debug only and never feeds a
// merge (07 §2.1).
func (s *Store) Apply(update ConfigDoc) (ConfigDoc, error) {
	s.mu.Lock()
	if update.Version != s.doc.Version {
		cur := cloneConfigDoc(s.doc)
		s.mu.Unlock()
		return cur, ErrConflict
	}
	next := cloneConfigDoc(update)
	next.Version = s.doc.Version + 1
	next.UpdatedBy = s.selfID
	next.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.doc = next
	out := cloneConfigDoc(next)
	s.mu.Unlock()

	s.save(out)
	s.signal()
	return out, nil
}

// Merge reconciles a remote replica into the local store. The doc body is
// last-writer-wins by Version, with the gossip sender id (remoteID) as the
// deterministic tiebreaker on an equal non-zero version (07 §2.1: the envelope
// id wins the tiebreak; UpdatedBy is advisory only). Independently, the
// grow-only RevokedSet is ALWAYS unioned regardless of who wins the doc (A.6,
// 07 §4.3) so a forgotten cert can never resurrect via a stale replica. It
// signals Changed only when the doc advanced OR the Revoked set grew (07 §5.2).
func (s *Store) Merge(remote ConfigDoc, remoteID string) {
	s.mu.Lock()
	take := remote.Version > s.doc.Version ||
		(remote.Version == s.doc.Version && remote.Version != 0 && remoteID > s.selfID)

	var merged ConfigDoc
	if take {
		merged = cloneConfigDoc(remote)
	} else {
		merged = cloneConfigDoc(s.doc)
	}
	// Grow-only union, ALWAYS — independent of the LWW doc winner.
	merged.Revoked = unionRevoked(s.doc.Revoked, remote.Revoked)

	changed := take || len(merged.Revoked.Entries) != len(s.doc.Revoked.Entries)
	if !changed {
		s.mu.Unlock()
		return
	}
	s.doc = merged
	out := cloneConfigDoc(merged)
	s.mu.Unlock()

	s.save(out)
	s.signal()
}

// Changed returns a coalesced signal channel (at most one pending). Receivers
// should re-read Get() on each receipt.
func (s *Store) Changed() <-chan struct{} { return s.changed }

// gossipState is the wire envelope carrying the doc plus the sender's id (so the
// receiver can apply the Merge tiebreak). It is internal to the gossip seam.
type gossipState struct {
	NodeID string    `json:"nodeId"`
	Doc    ConfigDoc `json:"doc"`
}

// MarshalGossip serializes the doc plus this node's id, for the cluster
// delegate's LocalState exchange (which carries no sender identity of its own).
func (s *Store) MarshalGossip() []byte {
	s.mu.Lock()
	env := gossipState{NodeID: s.selfID, Doc: cloneConfigDoc(s.doc)}
	s.mu.Unlock()
	b, err := json.Marshal(env)
	if err != nil {
		return nil
	}
	return b
}

// MergeGossip parses a gossip envelope produced by MarshalGossip and merges it.
// Malformed or empty payloads are ignored (anti-entropy will retry).
func (s *Store) MergeGossip(b []byte) {
	if len(b) == 0 {
		return
	}
	var env gossipState
	if err := json.Unmarshal(b, &env); err != nil {
		return
	}
	s.Merge(env.Doc, env.NodeID)
}

// Reset wipes the store back to the zero ConfigDoc (Version 0) and REMOVES the
// persisted file — the node-release path (forget/leave/takeover, 03 §4): the
// departing node must not retain the old cluster's secrets, and a version-0 doc
// loses every gossip merge so a stale replica cannot resurrect it. Signals
// Changed like any other write.
func (s *Store) Reset() {
	s.mu.Lock()
	s.doc = ConfigDoc{}
	s.mu.Unlock()

	if s.path != "" {
		s.saveMu.Lock()
		_ = os.Remove(s.path)
		s.saveMu.Unlock()
	}
	s.signal()
}

// save persists doc to config.json (best-effort, atomic temp+rename, mode 0600 —
// the doc carries the plaintext CA private key and credential verifiers, D18 /
// 07 §5.3). It runs OUTSIDE s.mu on the caller-supplied snapshot, serialized by
// saveMu, so it never deadlocks against the store mutex and concurrent saves
// can't interleave a half-written file. A no-op when the store has no path
// (built with New). Write errors are logged and swallowed so Apply/Merge never
// fail on a disk problem.
func (s *Store) save(doc ConfigDoc) {
	if s.path == "" {
		return
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "state: marshal config: %v\n", err)
		return
	}

	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	_ = os.MkdirAll(filepath.Dir(s.path), 0o700)
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "state: write config: %v\n", err)
		return
	}
	_ = os.Chmod(tmp, 0o600) // defeat umask so the mode is exactly 0600
	if err := os.Rename(tmp, s.path); err != nil {
		fmt.Fprintf(os.Stderr, "state: rename config: %v\n", err)
		return
	}
	_ = os.Chmod(s.path, 0o600)
}

func (s *Store) signal() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}
