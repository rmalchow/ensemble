package adopt

import (
	"sync"
	"time"
)

// sessionStore is the node half's in-memory map of in-flight NodeSessions, keyed
// by nonceA. It is safe for concurrent use (a node may field overlapping
// handshakes from different controllers) and self-pruning so a stalled handshake
// cannot leak memory on a 512 MB node.
type sessionStore struct {
	mu sync.Mutex
	m  map[string]*NodeSession
}

func newSessionStore() sessionStore {
	return sessionStore{m: make(map[string]*NodeSession)}
}

func (s *sessionStore) put(key string, sess *NodeSession) {
	s.mu.Lock()
	s.m[key] = sess
	s.mu.Unlock()
}

func (s *sessionStore) get(key string) *NodeSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[key]
}

func (s *sessionStore) del(key string) {
	s.mu.Lock()
	delete(s.m, key)
	s.mu.Unlock()
}

// prune drops sessions older than ttl relative to now (bounded memory).
func (s *sessionStore) prune(now time.Time, ttl time.Duration) {
	s.mu.Lock()
	for k, sess := range s.m {
		if now.Sub(sess.born) > ttl {
			delete(s.m, k)
		}
	}
	s.mu.Unlock()
}
