package cluster

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

func TestMetaJSONRoundTrip(t *testing.T) {
	in := Meta{
		NodeID:      "n1",
		Name:        "Kitchen",
		GroupID:     "g1",
		ClusterFP:   "abcdef",
		ControlPort: 8443,
		ClockPort:   9000,
		AudioPort:   9100,
		WebPort:     8080,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	// Assert canonical short keys are present (02 §3.1).
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "name", "gid", "cf", "ctrl", "clk", "aud", "wp"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing canonical key %q in %s", k, b)
		}
	}

	out, err := decodeMeta(b)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("round-trip Meta = %+v, want %+v", out, in)
	}
}

func TestDecodeMetaEmpty(t *testing.T) {
	if _, err := decodeMeta(nil); err == nil {
		t.Error("decodeMeta(nil) should error (skipped in Members)")
	}
	if _, err := decodeMeta([]byte{}); err == nil {
		t.Error("decodeMeta([]byte{}) should error")
	}
}

func TestMemberAddrHelpers(t *testing.T) {
	m := Member{
		Addr: net.ParseIP("192.168.1.5"),
		Port: 7946,
		Meta: Meta{ControlPort: 8443, ClockPort: 9000, AudioPort: 9100, WebPort: 8080},
	}
	cases := map[string]string{
		m.GossipAddr():  "192.168.1.5:7946",
		m.ControlAddr(): "192.168.1.5:8443",
		m.ClockAddr():   "192.168.1.5:9000",
		m.AudioAddr():   "192.168.1.5:9100",
		m.WebAddr():     "192.168.1.5:8080",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("addr = %q, want %q", got, want)
		}
	}
}

func TestDelegateBridgesState(t *testing.T) {
	st := state.New("n1")
	d := &delegate{state: st}

	ls := d.LocalState(false)
	if len(ls) == 0 {
		t.Fatal("LocalState returned empty; want MarshalGossip bytes")
	}
	if string(ls) != string(st.MarshalGossip()) {
		t.Error("LocalState does not match store.MarshalGossip()")
	}

	// A higher-Version remote wins through the pass-through delegate. Build a
	// remote envelope by marshaling a higher-version store as node n2.
	remoteStore := state.New("n2")
	if _, err := remoteStore.Apply(state.ConfigDoc{Version: 0, Cluster: state.ClusterInfo{Name: "X"}}); err != nil {
		t.Fatal(err)
	}
	d.MergeRemoteState(remoteStore.MarshalGossip(), false)

	if got := st.Get(); got.Cluster.Name != "X" || got.Version != 1 {
		t.Errorf("after MergeRemoteState: doc = %+v, want Cluster.Name=X Version=1", got)
	}

	// Nil-state delegate is inert.
	nd := &delegate{}
	if nd.LocalState(false) != nil {
		t.Error("nil-state LocalState should be nil")
	}
	nd.MergeRemoteState([]byte("x"), false) // must not panic
}

func TestDelegateNodeMetaTruncates(t *testing.T) {
	d := &delegate{}
	d.setMeta([]byte("0123456789"))
	if got := d.NodeMeta(4); string(got) != "0123" {
		t.Errorf("NodeMeta(4) = %q, want 0123", got)
	}
	if got := d.NodeMeta(100); string(got) != "0123456789" {
		t.Errorf("NodeMeta(100) = %q, want full", got)
	}
}

// ── Integration: two in-proc memberlists on loopback ──

func newTestNode(t *testing.T, id string, port int, st *state.Store) *Membership {
	t.Helper()
	m, err := New(Config{
		NodeID:      id,
		Name:        id,
		GroupID:     "g1",
		ClusterFP:   "fp-" + id,
		BindAddr:    "127.0.0.1",
		BindPort:    port,
		ControlPort: 8443,
		ClockPort:   9000,
		AudioPort:   9100,
		WebPort:     8080,
		State:       st,
	})
	if err != nil {
		t.Fatalf("New(%s): %v", id, err)
	}
	return m
}

// waitMembers polls until Members() reaches want or the deadline elapses.
func waitMembers(m *Membership, want int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if len(m.Members()) == want {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return len(m.Members()) == want
}

// skipLiveNet skips a live two-node memberlist test under -short or -race (see
// race_off_test.go for why -race is excluded for these).
func skipLiveNet(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping network integration test under -short")
	}
	if raceEnabled {
		t.Skip("skipping live memberlist test under -race (upstream Node-pointer race)")
	}
}

func TestMembershipTwoNodeJoinLeave(t *testing.T) {
	skipLiveNet(t)
	a := newTestNode(t, "na", 17946, nil)
	defer a.Leave()
	b := newTestNode(t, "nb", 17947, nil)

	if _, err := b.Join([]string{net.JoinHostPort("127.0.0.1", "17946")}); err != nil {
		t.Fatalf("B join A: %v", err)
	}

	if !waitMembers(a, 2, 5*time.Second) {
		t.Fatalf("A never saw 2 members, got %d", a.NumMembers())
	}
	if !waitMembers(b, 2, 5*time.Second) {
		t.Fatalf("B never saw 2 members, got %d", b.NumMembers())
	}

	// Both decode each other's Meta (ClusterFP + ports).
	foundB := false
	for _, mem := range a.Members() {
		if mem.Meta.NodeID == "nb" {
			foundB = true
			if mem.Meta.ClusterFP != "fp-nb" {
				t.Errorf("A sees B ClusterFP = %q, want fp-nb", mem.Meta.ClusterFP)
			}
			if mem.Meta.ClockPort != 9000 || mem.Meta.AudioPort != 9100 {
				t.Errorf("A sees B ports = %+v", mem.Meta)
			}
		}
	}
	if !foundB {
		t.Fatal("A did not decode B's Meta")
	}

	// Leave => A drops to 1 within a gossip round, and Changed fires.
	if err := b.Leave(); err != nil {
		t.Fatalf("B leave: %v", err)
	}
	if !waitMembers(a, 1, 5*time.Second) {
		t.Errorf("A never dropped to 1 after B left, got %d", a.NumMembers())
	}
	select {
	case <-a.Changed():
	case <-time.After(2 * time.Second):
		t.Error("A.Changed() did not fire after membership change")
	}
}

func TestMembershipUpdateName(t *testing.T) {
	skipLiveNet(t)
	a := newTestNode(t, "na", 17948, nil)
	defer a.Leave()
	b := newTestNode(t, "nb", 17949, nil)
	defer b.Leave()
	if _, err := b.Join([]string{net.JoinHostPort("127.0.0.1", "17948")}); err != nil {
		t.Fatalf("B join A: %v", err)
	}
	if !waitMembers(a, 2, 5*time.Second) {
		t.Fatalf("A never saw 2 members")
	}

	if err := b.UpdateName("Renamed"); err != nil {
		t.Fatalf("UpdateName: %v", err)
	}
	if b.Self().Name != "Renamed" {
		t.Errorf("B Self().Name = %q, want Renamed", b.Self().Name)
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		ok := false
		for _, mem := range a.Members() {
			if mem.Meta.NodeID == "nb" && mem.Meta.Name == "Renamed" {
				ok = true
			}
		}
		if ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("A never observed B's rename within timeout")
}

func TestMembershipConfigDocConverges(t *testing.T) {
	skipLiveNet(t)
	stA := state.New("na")
	stB := state.New("nb")
	a := newTestNode(t, "na", 17950, stA)
	defer a.Leave()
	b := newTestNode(t, "nb", 17951, stB)
	defer b.Leave()
	if _, err := b.Join([]string{net.JoinHostPort("127.0.0.1", "17950")}); err != nil {
		t.Fatalf("B join A: %v", err)
	}
	if !waitMembers(a, 2, 5*time.Second) {
		t.Fatalf("A never saw 2 members")
	}

	// Apply a higher-Version doc on A; B converges via push/pull anti-entropy.
	if _, err := stA.Apply(state.ConfigDoc{Version: 0, Cluster: state.ClusterInfo{Name: "Converged"}}); err != nil {
		t.Fatalf("Apply on A: %v", err)
	}

	// PushPullInterval is ~30s; allow two intervals so a missed first round
	// (the doc was applied just after a push) still converges deterministically.
	deadline := time.Now().Add(70 * time.Second)
	for time.Now().Before(deadline) {
		if stB.Get().Cluster.Name == "Converged" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("B did not converge: doc = %+v", stB.Get())
}
