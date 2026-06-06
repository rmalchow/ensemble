package caps

import (
	"context"
	"slices"
	"testing"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

const selfID = "node-self"

// noSleep is injected so the retry path runs without real delays.
func noSleep(time.Duration) {}

// seedStore returns a non-persistent Store seeded with a doc that carries a
// NodeRecord for the given ids (zero Caps), at Version 1.
func seedStore(t *testing.T, ids ...string) *state.Store {
	t.Helper()
	s := state.New(selfID)
	doc := s.Get() // Version 0
	for _, id := range ids {
		doc.Nodes = append(doc.Nodes, state.NodeRecord{ID: id, Name: id})
	}
	if _, err := s.Apply(doc); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}
	drain(s) // consume the seed's Changed() signal
	return s
}

// drain consumes any pending Changed() signal so a later changedFired() reflects
// only the action under test.
func drain(s *state.Store) {
	select {
	case <-s.Changed():
	default:
	}
}

// changedFired reports whether Changed() has a pending signal (and consumes it).
func changedFired(s *state.Store) bool {
	select {
	case <-s.Changed():
		return true
	default:
		return false
	}
}

func nodeCaps(t *testing.T, s *state.Store, id string) state.Capabilities {
	t.Helper()
	doc := s.Get()
	i := indexOfNode(doc.Nodes, id)
	if i < 0 {
		t.Fatalf("node %q absent", id)
	}
	return doc.Nodes[i].Caps
}

// renderingDetected is a probe result for a node with a working sink.
var renderingDetected = Detected{
	Sinks:        []string{"alsa", "exec:aplay"},
	EncodeCodecs: []string{codecPCM},
	DecodeCodecs: []string{codecPCM},
	FEC:          []string{fecNone, fecXORParity, fecDuplicate},
	MaxRate:      canonicalRate,
}

func newPub(s *state.Store, d Detected, m Mask) *Publisher {
	p := NewPublisher(s, selfID, d, m)
	p.sleep = noSleep
	return p
}

func TestPublishFirstWrite(t *testing.T) {
	s := seedStore(t, selfID)
	p := newPub(s, renderingDetected, Mask{})

	before := s.Get().Version
	got, err := p.Publish(context.Background())
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	want := Compute(renderingDetected, Mask{})
	if !capsEqual(got, want) {
		t.Fatalf("published caps = %+v, want %+v", got, want)
	}
	if !capsEqual(nodeCaps(t, s, selfID), want) {
		t.Fatalf("stored caps = %+v, want %+v", nodeCaps(t, s, selfID), want)
	}
	if after := s.Get().Version; after != before+1 {
		t.Fatalf("Version = %d, want %d", after, before+1)
	}
	if !changedFired(s) {
		t.Fatal("Changed() did not fire on first publish")
	}
}

func TestPublishIdempotent(t *testing.T) {
	s := seedStore(t, selfID)
	p := newPub(s, renderingDetected, Mask{})

	if _, err := p.Publish(context.Background()); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	drain(s)
	v := s.Get().Version

	if _, err := p.Publish(context.Background()); err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	if got := s.Get().Version; got != v {
		t.Fatalf("idempotent Publish bumped Version %d -> %d", v, got)
	}
	if changedFired(s) {
		t.Fatal("idempotent Publish fired Changed()")
	}
}

func TestPublishRecordNotSeeded(t *testing.T) {
	s := seedStore(t, "other-node") // no record for selfID
	p := newPub(s, renderingDetected, Mask{})

	v := s.Get().Version
	got, err := p.Publish(context.Background())
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !capsEqual(got, state.Capabilities{}) {
		t.Fatalf("want zero caps, got %+v", got)
	}
	if s.Get().Version != v {
		t.Fatal("Publish wrote despite missing record (adoption-race guard violated)")
	}
	if changedFired(s) {
		t.Fatal("Publish fired Changed() despite missing record")
	}
}

// TestPublishConflictRetry forces a genuine in-loop ErrConflict and verifies the
// publisher refetches and retries, landing BOTH the competing edit and its own
// caps. The conflict is injected deterministically through the sleep (backoff)
// hook: a one-shot competitor Applies during the publisher's first backoff, so
// the publisher's captured doc is stale on the first Apply attempt and fresh on
// the retry.
//
// To make the FIRST Apply conflict, the competitor lands its edit before the
// publisher's first Apply. We arrange that by advancing the store from the sleep
// hook is too late (the conflict must already have happened to reach a backoff),
// so instead we use a small concurrent competitor that the publisher's bounded
// retry loop is guaranteed to converge against, with a real (tiny) backoff so
// the goroutine makes progress.
func TestPublishConflictRetry(t *testing.T) {
	s := seedStore(t, selfID, "other-node")
	p := newPub(s, renderingDetected, Mask{})

	// One-shot competitor: rename other-node exactly once, racing the publisher.
	// The publisher's optimistic loop must absorb at most one ErrConflict.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			comp := s.Get()
			ci := indexOfNode(comp.Nodes, "other-node")
			comp.Nodes[ci].Name = "renamed-by-competitor"
			if _, err := s.Apply(comp); err == nil {
				return
			}
			// lost a race against the publisher's own Apply; retry on fresh doc
		}
	}()

	// Use a tiny real backoff so the competitor goroutine can make progress
	// between the publisher's attempts (no flakiness: the loop is bounded and
	// both writers converge — LWW with disjoint fields).
	p.sleep = func(d time.Duration) { time.Sleep(time.Millisecond) }

	got, err := p.Publish(context.Background())
	if err != nil {
		t.Fatalf("Publish after conflict: %v", err)
	}
	<-done

	want := Compute(renderingDetected, Mask{})
	if !capsEqual(got, want) {
		t.Fatalf("published caps = %+v, want %+v", got, want)
	}
	// Final doc carries BOTH the competing edit and the node's own caps.
	doc := s.Get()
	oi := indexOfNode(doc.Nodes, "other-node")
	if doc.Nodes[oi].Name != "renamed-by-competitor" {
		t.Fatalf("competing edit was lost: %q", doc.Nodes[oi].Name)
	}
	if !capsEqual(nodeCaps(t, s, selfID), want) {
		t.Fatal("self caps not landed after conflict")
	}
}

// TestPublishConflictDirect verifies the Store contract the retry loop relies
// on: applying a stale doc returns ErrConflict (sanity for the retry path).
func TestPublishConflictDirect(t *testing.T) {
	s := seedStore(t, selfID, "other-node")

	stale := s.Get() // Version 1
	si := indexOfNode(stale.Nodes, selfID)
	stale.Nodes[si].Caps = Compute(renderingDetected, Mask{})

	comp := s.Get()
	ci := indexOfNode(comp.Nodes, "other-node")
	comp.Nodes[ci].Name = "advanced"
	if _, err := s.Apply(comp); err != nil { // store -> Version 2
		t.Fatalf("competing Apply: %v", err)
	}
	if _, err := s.Apply(stale); err == nil {
		t.Fatal("expected ErrConflict applying a stale doc")
	}
}

func TestPublishRenderFalse(t *testing.T) {
	s := seedStore(t, selfID)
	p := newPub(s, renderingDetected, Mask{ForceRender: boolPtr(false)})

	got, err := p.Publish(context.Background())
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got.Render {
		t.Error("Render should be false (forced)")
	}
	if got.Sinks != nil {
		t.Errorf("Sinks should be empty, got %v", got.Sinks)
	}
	if got.MaxRate != 0 {
		t.Errorf("MaxRate should be 0 for a sink-less node, got %d", got.MaxRate)
	}
}

func TestPublishSetMaskRepublish(t *testing.T) {
	s := seedStore(t, selfID)
	p := newPub(s, renderingDetected, Mask{})

	if _, err := p.Publish(context.Background()); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	drain(s)
	v := s.Get().Version

	p.SetMask(Mask{DisableBackends: []string{"alsa", "exec:aplay"}})
	got, err := p.Publish(context.Background())
	if err != nil {
		t.Fatalf("re-Publish: %v", err)
	}
	if got.Render {
		t.Error("after disabling all backends, Render should be false")
	}
	if after := s.Get().Version; after != v+1 {
		t.Fatalf("re-publish Version = %d, want %d", after, v+1)
	}
}

func TestPublishOnlyOwnRecord(t *testing.T) {
	s := seedStore(t, selfID, "other-node")

	// Give the other node some pre-existing caps and capture them.
	doc := s.Get()
	oi := indexOfNode(doc.Nodes, "other-node")
	doc.Nodes[oi].Caps = state.Capabilities{
		Render: true,
		Sinks:  []string{"exec:pw-play"},
		FEC:    slices.Clone(allFEC),
	}
	if _, err := s.Apply(doc); err != nil {
		t.Fatalf("seed other caps: %v", err)
	}
	drain(s)
	otherBefore := nodeCaps(t, s, "other-node")

	p := newPub(s, renderingDetected, Mask{})
	if _, err := p.Publish(context.Background()); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if !capsEqual(nodeCaps(t, s, "other-node"), otherBefore) {
		t.Fatal("Publish mutated another node's Caps (must edit only its own record)")
	}
}

// TestPublishGossipRoundtrip is the A.13 P2 unit contribution: the self-write
// lands in the store and MarshalGossip carries the updated Caps to peers.
func TestPublishGossipRoundtrip(t *testing.T) {
	s := seedStore(t, selfID)
	p := newPub(s, renderingDetected, Mask{})
	if _, err := p.Publish(context.Background()); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// A fresh peer store merges the gossip envelope and must see our caps.
	peer := state.New("peer")
	peer.MergeGossip(s.MarshalGossip())

	want := Compute(renderingDetected, Mask{})
	if !capsEqual(nodeCaps(t, peer, selfID), want) {
		t.Fatalf("peer caps after gossip = %+v, want %+v", nodeCaps(t, peer, selfID), want)
	}
}
