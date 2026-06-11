package group

import (
	"io"
	"testing"

	"ensemble/internal/contracts"
	"ensemble/internal/stream"
)

// qFrameSrc is a pull source yielding n frames then io.EOF, tracking close.
type qFrameSrc struct {
	n      int
	read   int
	closed bool
}

func (s *qFrameSrc) ReadFrame([]byte) error {
	if s.read >= s.n {
		return io.EOF
	}
	s.read++
	return nil
}
func (s *qFrameSrc) Live() bool   { return false }
func (s *qFrameSrc) Close() error { s.closed = true; return nil }

// qOpener builds an open seam over a uri→frame-count map, recording open order.
func qOpener(frames map[string]int) (func(string) (MediaSource, error), *[]string) {
	var opened []string
	open := func(uri string) (MediaSource, error) {
		opened = append(opened, uri)
		n := frames[uri]
		if n == 0 {
			n = 1
		}
		return &qFrameSrc{n: n}, nil
	}
	return open, &opened
}

func item(uri string) contracts.QueueItem { return contracts.QueueItem{URI: uri} }

// drain reads frames until io.EOF, returning the total frame count.
func drain(t *testing.T, q *queueSource) int {
	t.Helper()
	buf := make([]byte, stream.FrameBytes)
	total := 0
	for {
		err := q.ReadFrame(buf)
		if err == io.EOF {
			return total
		}
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		total++
		if total > 10000 {
			t.Fatal("runaway: no EOF")
		}
	}
}

func TestQueueChainsGaplessToEOF(t *testing.T) {
	open, opened := qOpener(map[string]int{"a": 3, "b": 2, "c": 4})
	q := newQueueSource([]contracts.QueueItem{item("a"), item("b"), item("c")}, open, nil)
	if err := q.prime(); err != nil {
		t.Fatal(err)
	}
	if total := drain(t, q); total != 9 {
		t.Fatalf("frames = %d, want 9 (3+2+4)", total)
	}
	if got := *opened; len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("open order = %v, want [a b c]", got)
	}
}

func TestQueueNowReportsCurrentAndUpcoming(t *testing.T) {
	open, _ := qOpener(map[string]int{"a": 5, "b": 1, "c": 1})
	q := newQueueSource([]contracts.QueueItem{item("a"), item("b"), item("c")}, open, nil)
	if err := q.prime(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, stream.FrameBytes)
	_ = q.ReadFrame(buf)
	_ = q.ReadFrame(buf) // 2 frames into "a"

	uri, _, pos, up := q.Now()
	if uri != "a" {
		t.Errorf("now uri = %q, want a", uri)
	}
	wantPos := float64(2*stream.FrameNanos) / 1e9
	if pos != wantPos {
		t.Errorf("pos = %v, want %v", pos, wantPos)
	}
	if len(up) != 2 || up[0].URI != "b" || up[1].URI != "c" {
		t.Errorf("upcoming = %v, want [b c]", up)
	}
}

func TestQueueNextSkips(t *testing.T) {
	open, _ := qOpener(map[string]int{"a": 100, "b": 2, "c": 1})
	q := newQueueSource([]contracts.QueueItem{item("a"), item("b"), item("c")}, open, nil)
	if err := q.prime(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, stream.FrameBytes)
	_ = q.ReadFrame(buf) // 1 frame of "a"
	q.Next()             // skip the rest of "a"
	_ = q.ReadFrame(buf) // should now be reading "b"
	if uri, _, _, _ := q.Now(); uri != "b" {
		t.Fatalf("after Next, now = %q, want b", uri)
	}
}

func TestQueuePlayNowReplacesCurrent(t *testing.T) {
	open, opened := qOpener(map[string]int{"a": 100, "b": 1, "x": 2})
	q := newQueueSource([]contracts.QueueItem{item("a"), item("b")}, open, nil)
	if err := q.prime(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, stream.FrameBytes)
	_ = q.ReadFrame(buf) // playing "a"
	q.PlayNow(item("x")) // drop "a", play "x" now, keep "b"
	_ = q.ReadFrame(buf) // now "x"
	uri, _, _, up := q.Now()
	if uri != "x" {
		t.Fatalf("now = %q, want x", uri)
	}
	if len(up) != 1 || up[0].URI != "b" {
		t.Fatalf("upcoming = %v, want [b]", up)
	}
	// "a" must not be replayed: total open sequence is a, x, b.
	if got := *opened; got[len(got)-1] != "x" && got[len(got)-1] != "b" {
		t.Fatalf("unexpected open order %v", got)
	}
}

func TestQueuePlayUpcomingPromotes(t *testing.T) {
	open, _ := qOpener(map[string]int{"a": 100, "b": 1, "c": 2})
	q := newQueueSource([]contracts.QueueItem{item("a"), item("b"), item("c")}, open, nil)
	if err := q.prime(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, stream.FrameBytes)
	_ = q.ReadFrame(buf) // playing "a"; upcoming [b c]
	// promote upcoming index 1 ("c"): drop "a", play "c" now, "c" leaves its slot.
	q.PlayUpcoming(1, "c")
	_ = q.ReadFrame(buf) // now "c"
	uri, _, _, up := q.Now()
	if uri != "c" {
		t.Fatalf("now = %q, want c", uri)
	}
	if len(up) != 1 || up[0].URI != "b" {
		t.Fatalf("upcoming = %v, want [b]", up)
	}
}

func TestQueuePlayUpcomingStaleGuardNoop(t *testing.T) {
	open, _ := qOpener(map[string]int{"a": 100, "b": 1, "c": 1})
	q := newQueueSource([]contracts.QueueItem{item("a"), item("b"), item("c")}, open, nil)
	if err := q.prime(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, stream.FrameBytes)
	_ = q.ReadFrame(buf) // playing "a"
	q.PlayUpcoming(0, "wrong")
	if uri, _, _, up := q.Now(); uri != "a" || len(up) != 2 {
		t.Fatalf("guard mismatch should be a no-op, now = %q upcoming = %v", uri, up)
	}
}

func TestQueueRevBumpsOnChange(t *testing.T) {
	open, _ := qOpener(map[string]int{"a": 100, "b": 1, "c": 1, "x": 1})
	q := newQueueSource([]contracts.QueueItem{item("a"), item("b"), item("c")}, open, nil)
	if err := q.prime(); err != nil {
		t.Fatal(err)
	}
	if q.QueueRev() != 0 {
		t.Fatalf("initial rev = %d, want 0", q.QueueRev())
	}
	steps := []struct {
		name string
		do   func()
	}{
		{"append", func() { q.Append([]contracts.QueueItem{item("d")}) }},
		{"remove", func() { q.RemoveUpcoming(0, "b") }},
		{"playUpcoming", func() { q.PlayUpcoming(0, "c") }},
		{"playNow", func() { q.PlayNow(item("x")) }},
		{"next", func() { q.Next() }},
	}
	prev := q.QueueRev()
	for _, s := range steps {
		s.do()
		if got := q.QueueRev(); got <= prev {
			t.Fatalf("after %s, rev = %d, want > %d", s.name, got, prev)
		}
		prev = q.QueueRev()
	}
	// a no-op (stale guard) must NOT bump the rev.
	q.RemoveUpcoming(99, "nope")
	if got := q.QueueRev(); got != prev {
		t.Fatalf("stale remove bumped rev to %d, want %d", got, prev)
	}
}

func TestQueueRemoveUpcoming(t *testing.T) {
	open, _ := qOpener(map[string]int{"a": 1, "b": 1, "c": 1})
	q := newQueueSource([]contracts.QueueItem{item("a"), item("b"), item("c")}, open, nil)
	if err := q.prime(); err != nil {
		t.Fatal(err)
	}
	// upcoming is [b c]; remove index 0 ("b") with a matching guard.
	q.RemoveUpcoming(0, "b")
	if _, _, _, up := q.Now(); len(up) != 1 || up[0].URI != "c" {
		t.Fatalf("after remove, upcoming = %v, want [c]", up)
	}
	// stale guard is a no-op.
	q.RemoveUpcoming(0, "wrong")
	if _, _, _, up := q.Now(); len(up) != 1 {
		t.Fatalf("guard mismatch should be a no-op, upcoming = %v", up)
	}
}

func TestQueueAppend(t *testing.T) {
	open, _ := qOpener(map[string]int{"a": 1, "b": 1})
	q := newQueueSource([]contracts.QueueItem{item("a")}, open, nil)
	if err := q.prime(); err != nil {
		t.Fatal(err)
	}
	q.Append([]contracts.QueueItem{item("b")})
	if !q.hasUpcoming() {
		t.Fatal("hasUpcoming should be true after append")
	}
	if total := drain(t, q); total != 2 {
		t.Fatalf("frames = %d, want 2", total)
	}
}

func TestQueueOnChangeFiresPerTrack(t *testing.T) {
	open, _ := qOpener(map[string]int{"a": 1, "b": 1})
	changes := 0
	q := newQueueSource([]contracts.QueueItem{item("a"), item("b")}, open, func() { changes++ })
	if err := q.prime(); err != nil {
		t.Fatal(err)
	}
	drain(t, q)
	// "a" is primed (no onChange), the roll into "b" fires once.
	if changes != 1 {
		t.Fatalf("onChange fired %d times, want 1 (the a→b boundary)", changes)
	}
}
