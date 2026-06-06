package cluster

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Peer is a previously-seen member of OUR cluster, remembered across restarts.
// Peers are persisted to <data>/peers.json so that after a reboot a
// known-but-currently-absent peer shows immediately as "offline" (rather than
// vanishing until rediscovered, 02 §2.4/§3.3) and so its last-known gossip
// address can seed memberlist.Join for a faster rejoin (A.14.1). GossipAddr is
// the ip:port form memberlist.Join accepts.
type Peer struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	GossipAddr string `json:"gossipAddr"`
	LastSeen   int64  `json:"lastSeen"` // unix seconds
}

// PeerStore is an always-on, best-effort cache of previously-seen peers of our
// own cluster, persisted to <data>/peers.json. It is NOT secret-bearing (it
// holds only ids/names/addresses), so the file is written mode 0644, distinct
// from the secret-bearing config doc (0600, D18). All disk operations are
// best-effort: read/write failures are non-fatal and never crash the node. Safe
// for concurrent use.
//
// NOTE (spec §9 Q1): this is the RICHER mpvsync shape ([]Peer with
// Name/LastSeen), deliberately diverging from A.14.1's flat
// {"peers":["host:7946",...]} string list, which would lose the Name/LastSeen
// the offline-UI state needs. Flagged for orchestrator confirmation.
type PeerStore struct {
	path  string
	mu    sync.Mutex
	peers map[string]Peer // keyed by peer id
}

// LoadPeerStore opens (or initializes) the peer cache at path
// (<data>/peers.json). A missing or corrupt file yields an empty store
// (non-fatal, no panic); the file is created lazily on the first write.
func LoadPeerStore(path string) *PeerStore {
	s := &PeerStore{path: path, peers: make(map[string]Peer)}
	if b, err := os.ReadFile(path); err == nil {
		var loaded []Peer
		if json.Unmarshal(b, &loaded) == nil {
			for _, p := range loaded {
				if p.ID != "" {
					s.peers[p.ID] = p
				}
			}
		}
	}
	return s
}

// Upsert merges the given members (our current cluster peers) into the store —
// refreshing each member's name/gossip-addr/last-seen — then persists the full
// set (best-effort, atomic temp+rename, mode 0644). Previously-seen peers not in
// the current member list are retained (small scale, <=8 nodes; never pruned)
// so they keep showing as "offline". Self may be included; it is harmless to
// remember our own entry, and the caller passes mem.Members() (which includes
// self) as-is.
func (s *PeerStore) Upsert(members []Member) {
	now := time.Now().Unix()
	s.mu.Lock()
	for _, m := range members {
		if m.Meta.NodeID == "" {
			continue
		}
		s.peers[m.Meta.NodeID] = Peer{
			ID:         m.Meta.NodeID,
			Name:       m.Meta.Name,
			GossipAddr: m.GossipAddr(),
			LastSeen:   now,
		}
	}
	data := s.marshalLocked()
	s.mu.Unlock()

	writePublic(s.path, data)
}

// Remove drops the peer with the given id and persists the remaining set
// (best-effort, atomic temp+rename, mode 0644), so a peer forgotten from this
// node's UI no longer lingers as "offline" and is no longer used as a gossip
// join seed. A no-op (no rewrite) if the id is empty or unknown.
func (s *PeerStore) Remove(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	if _, ok := s.peers[id]; !ok {
		s.mu.Unlock()
		return
	}
	delete(s.peers, id)
	data := s.marshalLocked()
	s.mu.Unlock()

	writePublic(s.path, data)
}

// Snapshot returns a copy of all remembered peers for read-only use.
func (s *PeerStore) Snapshot() []Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Peer, 0, len(s.peers))
	for _, p := range s.peers {
		out = append(out, p)
	}
	return out
}

// JoinSeeds returns the stored gossip addresses, for seeding memberlist.Join on
// startup so a configured node re-forms the cluster even if mDNS is slow or
// unavailable (A.14.1). Empty/unset addresses are skipped.
func (s *PeerStore) JoinSeeds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.peers))
	for _, p := range s.peers {
		if p.GossipAddr != "" {
			out = append(out, p.GossipAddr)
		}
	}
	return out
}

// Clear empties the peer cache and rewrites an empty peers.json. Used when the
// node LEAVES its cluster (forget, or moving to a different group): the
// remembered peers belong to the cluster it left and must not seed a rejoin
// (02 §3.3).
func (s *PeerStore) Clear() {
	s.mu.Lock()
	s.peers = make(map[string]Peer)
	data := s.marshalLocked()
	s.mu.Unlock()
	writePublic(s.path, data)
}

// marshalLocked serializes the current peer set; the caller holds s.mu.
func (s *PeerStore) marshalLocked() []byte {
	list := make([]Peer, 0, len(s.peers))
	for _, p := range s.peers {
		list = append(list, p)
	}
	data, _ := json.MarshalIndent(list, "", "  ")
	return data
}

// writePublic atomically writes data to path (temp+rename) with mode 0644. The
// peer cache carries no secret, so the file is world-readable. All failures are
// silently ignored (best-effort, off the hot path).
func writePublic(path string, data []byte) {
	if data == nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Chmod(tmp, 0o644) // defeat umask so the mode is exactly 0644
		_ = os.Rename(tmp, path)
		_ = os.Chmod(path, 0o644)
	}
}
