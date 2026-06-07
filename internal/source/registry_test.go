package source

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"ensemble/internal/stream"
)

func ap(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

func TestRegistryUpsertNewVsKeepalive(t *testing.T) {
	r := newRegistry()
	now := time.Now()
	_, isNew := r.upsert(ap("127.0.0.1:5000"), stream.TransportUDP, nil, now)
	if !isNew {
		t.Fatal("first upsert should be new")
	}
	later := now.Add(time.Second)
	s, isNew := r.upsert(ap("127.0.0.1:5000"), stream.TransportUDP, nil, later)
	if isNew {
		t.Fatal("repeat upsert should not be new")
	}
	if !s.lastSeen.Equal(later) {
		t.Fatal("lastSeen not refreshed")
	}
}

func TestRegistryExpire(t *testing.T) {
	r := newRegistry()
	now := time.Now()
	r.upsert(ap("127.0.0.1:5000"), stream.TransportUDP, nil, now)
	r.upsert(ap("127.0.0.1:5001"), stream.TransportUDP, nil, now.Add(20*time.Second))
	conns := r.expire(now.Add(20*time.Second), 15*time.Second)
	if len(conns) != 0 {
		t.Fatal("UDP sub has no conn to return")
	}
	if r.count() != 1 {
		t.Fatalf("count=%d want 1 (one expired)", r.count())
	}
	if r.get(ap("127.0.0.1:5000")) != nil {
		t.Fatal("stale sub not removed")
	}
	if r.get(ap("127.0.0.1:5001")) == nil {
		t.Fatal("fresh sub wrongly removed")
	}
}

func TestRegistryExpireReturnsTCPConn(t *testing.T) {
	r := newRegistry()
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	now := time.Now()
	r.upsert(ap("127.0.0.1:6000"), stream.TransportTCP, c1, now)
	conns := r.expire(now.Add(20*time.Second), 15*time.Second)
	if len(conns) != 1 {
		t.Fatalf("expected 1 returned conn, got %d", len(conns))
	}
}

func TestRegistryRemoveBye(t *testing.T) {
	r := newRegistry()
	r.upsert(ap("127.0.0.1:5000"), stream.TransportUDP, nil, time.Now())
	r.remove(ap("127.0.0.1:5000"))
	if r.count() != 0 {
		t.Fatal("remove failed")
	}
}

func TestRegistryTransportRouting(t *testing.T) {
	r := newRegistry()
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	now := time.Now()
	u, _ := r.upsert(ap("127.0.0.1:5000"), stream.TransportUDP, nil, now)
	tc, _ := r.upsert(ap("127.0.0.1:6000"), stream.TransportTCP, c1, now)
	if u.conn != nil {
		t.Fatal("UDP sub should have nil conn")
	}
	if tc.conn == nil || tc.tr != stream.TransportTCP {
		t.Fatal("TCP sub should carry conn")
	}
}
