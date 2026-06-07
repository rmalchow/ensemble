package stream

import (
	"reflect"
	"testing"
)

func seqsOf(recs []frameRec) []uint64 {
	out := make([]uint64, len(recs))
	for i, r := range recs {
		out[i] = r.seq
	}
	return out
}

func TestWindowInOrderDelivery(t *testing.T) {
	b := newReorderBuffer(1)
	var got []uint64
	for i := uint64(0); i < 5; i++ {
		d, lost, dup, stale := b.admit(1, i, int64(i), payload(1, 4))
		if lost != 0 || dup || stale {
			t.Fatalf("unexpected lost/dup/stale at %d", i)
		}
		got = append(got, seqsOf(d)...)
	}
	if !reflect.DeepEqual(got, []uint64{0, 1, 2, 3, 4}) {
		t.Fatalf("got %v", got)
	}
}

func TestWindowReordersWithinWindow(t *testing.T) {
	b := newReorderBuffer(1)
	b.admit(1, 0, 0, payload(1, 4))
	// deliver 2 before 1 -> 2 held
	d, _, _, _ := b.admit(1, 2, 0, payload(1, 4))
	if len(d) != 0 {
		t.Fatalf("seq 2 should be held, got %v", seqsOf(d))
	}
	d, _, _, _ = b.admit(1, 1, 0, payload(1, 4))
	if !reflect.DeepEqual(seqsOf(d), []uint64{1, 2}) {
		t.Fatalf("got %v want [1 2]", seqsOf(d))
	}
}

func TestWindowGapBecomesLost(t *testing.T) {
	b := newReorderBuffer(1)
	b.admit(1, 0, 0, payload(1, 4))
	// Jump far ahead beyond maxAhead -> the gap [1..] is forced lost.
	target := uint64(1 + defaultMaxAhead + 5)
	d, lost, _, _ := b.admit(1, target, 0, payload(1, 4))
	if lost == 0 {
		t.Fatal("expected lost frames from forced progress")
	}
	// the target should now be deliverable (it's at the new head)
	_ = d
	if b.next <= 1 {
		t.Fatalf("window did not advance: next=%d", b.next)
	}
}

func TestWindowDuplicateDropped(t *testing.T) {
	b := newReorderBuffer(1)
	b.admit(1, 0, 0, payload(1, 4))
	_, _, dup, _ := b.admit(1, 0, 0, payload(1, 4))
	if !dup {
		t.Fatal("re-admitting delivered seq should be dup")
	}
}

func TestWindowOverflowEvicts(t *testing.T) {
	b := newReorderBuffer(1)
	b.admit(1, 0, 0, payload(1, 4)) // anchor at 0, delivered
	// Hold many out-of-order frames beyond the horizon to force eviction.
	d, lost, _, _ := b.admit(1, 1+defaultMaxAhead, 0, payload(1, 4))
	if lost == 0 {
		t.Fatalf("expected eviction lost; got d=%v", seqsOf(d))
	}
}

func TestWindowResetReanchors(t *testing.T) {
	b := newReorderBuffer(1)
	b.admit(1, 5, 0, payload(1, 4)) // anchors at 5
	b.reset(2)
	if b.started || b.next != 0 {
		t.Fatal("reset should clear anchor")
	}
	d, _, _, _ := b.admit(2, 100, 0, payload(1, 4))
	if len(d) != 1 || d[0].seq != 100 {
		t.Fatalf("new gen should re-anchor at first seq, got %v", seqsOf(d))
	}
}

func TestWindowFirstFrameAnchors(t *testing.T) {
	b := newReorderBuffer(3)
	// Join mid-stream: first admitted seq 50 anchors and is delivered.
	d, lost, _, _ := b.admit(3, 50, 0, payload(1, 4))
	if len(d) != 1 || d[0].seq != 50 || lost != 0 {
		t.Fatalf("first frame should anchor & deliver, got %v lost=%d", seqsOf(d), lost)
	}
}

func TestWindowStaleGen(t *testing.T) {
	b := newReorderBuffer(5)
	_, _, _, stale := b.admit(4, 0, 0, payload(1, 4))
	if !stale {
		t.Fatal("gen below window should be stale")
	}
}
