package pki

import (
	"bytes"
	"testing"
)

func TestNewGossipKey(t *testing.T) {
	k1 := NewGossipKey()
	if len(k1) != gossipKeyLen {
		t.Errorf("len=%d, want %d", len(k1), gossipKeyLen)
	}

	k2 := NewGossipKey()
	if bytes.Equal(k1, k2) {
		t.Error("two NewGossipKey calls returned identical keys")
	}

	// Entropy sanity: a 32-byte random key must not be all-zero.
	if bytes.Equal(k1, make([]byte, gossipKeyLen)) {
		t.Error("gossip key is all-zero")
	}
}
