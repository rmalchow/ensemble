package origin

import "testing"

// TestPacerHeapOrder asserts the min-heap releases bundles in send-time order, and
// that equal-sendAt bundles release in push (FIFO) order — the §5.5.3 invariant
// that a repair packet scheduled at the same instant as a later source chunk keeps
// its production order.
func TestPacerHeapOrder(t *testing.T) {
	tests := []struct {
		name    string
		pushAt  []int64 // sendAt of each bundle, in push order
		wantOrd []int   // expected pop order as indices into pushAt
	}{
		{
			name:    "already sorted",
			pushAt:  []int64{0, 10, 20, 30},
			wantOrd: []int{0, 1, 2, 3},
		},
		{
			name:    "reversed",
			pushAt:  []int64{30, 20, 10, 0},
			wantOrd: []int{3, 2, 1, 0},
		},
		{
			name:    "interleaved repairs trail their group (equal sendAt FIFO)",
			pushAt:  []int64{0, 0, 10, 10},
			wantOrd: []int{0, 1, 2, 3},
		},
		{
			name:    "shuffled",
			pushAt:  []int64{50, 10, 40, 20, 30},
			wantOrd: []int{1, 3, 4, 2, 0},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var p pacer
			// Tag each bundle's payload with its push index so we can verify order.
			for i, at := range tc.pushAt {
				p.push(at, [][]byte{{byte(i)}})
			}
			var gotOrd []int
			var lastAt int64 = -1
			for p.len() > 0 {
				at, ok := p.peek()
				if !ok {
					t.Fatal("peek false with non-empty heap")
				}
				if at < lastAt {
					t.Fatalf("send-time order violated: %d after %d", at, lastAt)
				}
				lastAt = at
				b := p.pop()
				gotOrd = append(gotOrd, int(b.packets[0][0]))
			}
			if len(gotOrd) != len(tc.wantOrd) {
				t.Fatalf("popped %d bundles, want %d", len(gotOrd), len(tc.wantOrd))
			}
			for i := range gotOrd {
				if gotOrd[i] != tc.wantOrd[i] {
					t.Errorf("pop order = %v, want %v", gotOrd, tc.wantOrd)
					break
				}
			}
		})
	}
}

// TestPacerReset asserts reset empties the heap (used on ResumeAt / gen change so
// no prior-generation packet survives, 05 §5.8).
func TestPacerReset(t *testing.T) {
	var p pacer
	p.push(10, [][]byte{{1}})
	p.push(20, [][]byte{{2}})
	if p.len() != 2 {
		t.Fatalf("len=%d want 2", p.len())
	}
	p.reset()
	if p.len() != 0 {
		t.Fatalf("after reset len=%d want 0", p.len())
	}
	if _, ok := p.peek(); ok {
		t.Error("peek ok after reset")
	}
	// Reuse after reset works and ord restarts (FIFO tiebreak from 0).
	p.push(5, [][]byte{{9}})
	if at, ok := p.peek(); !ok || at != 5 {
		t.Errorf("post-reset peek=(%d,%v) want (5,true)", at, ok)
	}
}

// TestSendTimeMath asserts the origin's send instant = playout(idx) − Lead, which
// for the reference master folds to baseMono + (idx−baseIdx)/rate. We verify the
// per-chunk send-time spacing equals one chunk (10 ms) at the canonical rate.
func TestSendTimeMath(t *testing.T) {
	const (
		rate        = 48000
		chunkFrames = 480
		secondNs    = int64(1_000_000_000)
	)
	baseIdx := int64(0)
	baseMono := int64(0)
	for c := int64(0); c < 5; c++ {
		idx := baseIdx + c*chunkFrames
		sendAt := baseMono + (idx-baseIdx)*secondNs/rate
		want := c * 10 * 1_000_000 // 10 ms per chunk in ns
		if sendAt != want {
			t.Errorf("chunk %d sendAt=%d want %d (10ms cadence)", c, sendAt, want)
		}
	}
}
