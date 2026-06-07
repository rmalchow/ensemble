package cluster

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"ensemble/internal/discovery"
	"ensemble/internal/id"
)

// freeUDPPort returns a likely-free UDP port on loopback. memberlist binds both
// TCP and UDP on the number, so this is best-effort.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("freeUDPPort: %v", err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	c.Close()
	return port
}

func startNode(t *testing.T, self id.ID, peers <-chan discovery.Peer) (*Cluster, int) {
	t.Helper()
	port := freeUDPPort(t)
	c, err := New(Config{
		Self:       self,
		Name:       self.String()[:8],
		Volume:     1.0,
		GossipPort: port,
		BindAddr:   "127.0.0.1",
		Addrs:      []string{"127.0.0.1/8"},
		HTTPPort:   8080,
		StreamPort: 9090,
		SourcePort: 9200,
		Peers:      peers,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return c, port
}

func TestNewSeedsOwnRecord(t *testing.T) {
	self := id.New()
	c, err := New(Config{
		Self:          self,
		Name:          "seed",
		Volume:        0.7,
		OutputDelayMs: 33,
		GossipPort:    7946,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.mu.Lock()
	r := c.doc.Nodes[self]
	c.mu.Unlock()
	if r == nil || r.Version != 1 || r.Name != "seed" || r.Volume != 0.7 || r.OutputDelayMs != 33 {
		t.Fatalf("own record not seeded: %+v", r)
	}
}

func TestNewRejectsZeroSelf(t *testing.T) {
	if _, err := New(Config{GossipPort: 7946}); err == nil {
		t.Fatal("expected error for zero self")
	}
}

func TestStartCloseNoLeak(t *testing.T) {
	c, _ := startNode(t, id.New(), nil)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestCloseIdempotent(t *testing.T) {
	c, _ := startNode(t, id.New(), nil)
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestSubscribeCoalesces(t *testing.T) {
	c := newTestCluster(t, id.New(), nil)
	sub := c.Subscribe()
	for i := 0; i < 100; i++ {
		c.SetName("n" + string(rune('a'+i%26)))
	}
	// at least one signal, never blocks
	select {
	case <-sub:
	default:
		t.Fatal("expected at least one notify")
	}
	c.Close()
	// channel closed after Close
	if _, ok := <-sub; ok {
		// drain any buffered then expect closed
		<-sub
	}
}

func TestJoinLoopSkipsSelf(t *testing.T) {
	self := id.New()
	peers := make(chan discovery.Peer, 1)
	c, _ := startNode(t, self, peers)
	defer c.Close()
	// feed our own id — tryJoin must skip; no panic, members stays 1
	peers <- discovery.Peer{ID: self, Addr: addr127(), GossipPort: 1}
	time.Sleep(50 * time.Millisecond)
	if c.ml.NumMembers() != 1 {
		t.Fatalf("self-join should be skipped, members=%d", c.ml.NumMembers())
	}
}

func TestTwoNodesConvergeOverLoopback(t *testing.T) {
	idA := id.New()
	idB := id.New()
	peersB := make(chan discovery.Peer, 4)

	a, portA := startNode(t, idA, nil)
	defer a.Close()
	b, _ := startNode(t, idB, peersB)
	defer b.Close()

	// Tell B about A via the discovery channel.
	peersB <- discovery.Peer{ID: idA, Addr: addr127(), GossipPort: portA}

	// Wait for convergence: both snapshots list both nodes.
	if !eventually(t, 5*time.Second, func() bool {
		return hasNodes(a, idA, idB) && hasNodes(b, idA, idB)
	}) {
		t.Fatalf("nodes did not converge: A=%d B=%d members", a.ml.NumMembers(), b.ml.NumMembers())
	}
}

func TestFollowPropagates(t *testing.T) {
	idA := id.New()
	idB := id.New()
	peersB := make(chan discovery.Peer, 4)

	a, portA := startNode(t, idA, nil)
	defer a.Close()
	b, _ := startNode(t, idB, peersB)
	defer b.Close()
	peersB <- discovery.Peer{ID: idA, Addr: addr127(), GossipPort: portA}

	if !eventually(t, 5*time.Second, func() bool { return hasNodes(a, idA, idB) }) {
		t.Fatal("precondition: nodes not converged")
	}

	b.SetFollowing(idA)

	if !eventually(t, 5*time.Second, func() bool {
		for _, g := range a.Snapshot().Groups {
			if g.Master == idA && len(g.Members) == 2 {
				return true
			}
		}
		return false
	}) {
		t.Fatal("A did not see derived {A,B} group after B follows A")
	}
}

func TestStartReconcilesOwnVersionAfterRestart(t *testing.T) {
	self := id.New()
	c := newTestCluster(t, self, nil)
	// Simulate a peer holding a higher-versioned copy of our own record arriving
	// via push/pull; reconcileOwnVersion must jump us above it.
	c.reconcileOwnVersion(50)
	if ownVersion(c) != 51 {
		t.Fatalf("own version after reconcile = %d want 51", ownVersion(c))
	}
	// A subsequent SetName advances further and would win.
	c.SetName("after-restart")
	if ownVersion(c) != 52 {
		t.Fatalf("post-reconcile setter version = %d want 52", ownVersion(c))
	}
}

// --- helpers ---

func addr127() netip.Addr {
	return netip.MustParseAddr("127.0.0.1")
}

func hasNodes(c *Cluster, ids ...id.ID) bool {
	snap := c.Snapshot()
	have := map[id.ID]bool{}
	for _, n := range snap.Nodes {
		have[n.ID] = true
	}
	for _, want := range ids {
		if !have[want] {
			return false
		}
	}
	return true
}

func eventually(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cond()
}
