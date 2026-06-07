package clock

import "testing"

func TestNewSampleMath(t *testing.T) {
	// t1=0, t2=100, t3=110, t4=20 (local). master ahead by ~95.
	// offset = ((100-0)+(110-20))/2 = (100+90)/2 = 95
	// rtt    = (20-0)-(110-100) = 20-10 = 10
	s := newSample(0, 100, 110, 20)
	if s.offset != 95 {
		t.Errorf("offset = %d, want 95", s.offset)
	}
	if s.rtt != 10 {
		t.Errorf("rtt = %d, want 10", s.rtt)
	}
}

func TestEstimatorUnsyncedUntilFirstSample(t *testing.T) {
	var e estimator
	if _, ok := e.offset(); ok {
		t.Fatal("offset ok=true with zero samples, want false")
	}
	e.add(sample{offset: 50, rtt: 1})
	off, ok := e.offset()
	if !ok || off != 50 {
		t.Fatalf("offset = %d ok=%v, want 50 true", off, ok)
	}
}

func TestEstimatorMedianOfBestFive(t *testing.T) {
	var e estimator
	// Five low-RTT samples with offsets 10,20,30,40,50 -> median 30.
	low := []sample{
		{offset: 30, rtt: 1},
		{offset: 10, rtt: 2},
		{offset: 50, rtt: 3},
		{offset: 20, rtt: 4},
		{offset: 40, rtt: 5},
	}
	for _, s := range low {
		e.add(s)
	}
	// 25 high-RTT junk samples with wild offsets that must be ignored.
	for i := 0; i < 25; i++ {
		e.add(sample{offset: int64(1_000_000 + i), rtt: int64(1000 + i)})
	}
	off, ok := e.offset()
	if !ok {
		t.Fatal("not synced")
	}
	if off != 30 {
		t.Fatalf("median = %d, want 30 (median of best-5 offsets)", off)
	}
}

func TestEstimatorFewerThanFiveSamples(t *testing.T) {
	var e estimator
	// 3 samples: offsets 5,15,25 -> median 15.
	e.add(sample{offset: 25, rtt: 3})
	e.add(sample{offset: 5, rtt: 1})
	e.add(sample{offset: 15, rtt: 2})
	off, ok := e.offset()
	if !ok || off != 15 {
		t.Fatalf("median = %d ok=%v, want 15 true", off, ok)
	}
}

func TestEstimatorRingEviction(t *testing.T) {
	var e estimator
	// Add 35 samples. The first 5 (offsets 0..4, rtt 0..4 - the best RTTs!)
	// must be evicted so they cannot influence the estimate.
	for i := 0; i < 35; i++ {
		e.add(sample{offset: int64(i), rtt: int64(i)})
	}
	if e.len() != windowSize {
		t.Fatalf("len = %d, want %d", e.len(), windowSize)
	}
	// Remaining samples are i=5..34. Best-5 RTTs are i=5,6,7,8,9 ->
	// offsets 5,6,7,8,9 -> median 7.
	off, ok := e.offset()
	if !ok || off != 7 {
		t.Fatalf("median = %d ok=%v, want 7 true", off, ok)
	}
}

func TestEstimatorResetClears(t *testing.T) {
	var e estimator
	e.add(sample{offset: 1, rtt: 1})
	e.add(sample{offset: 2, rtt: 2})
	e.reset()
	if e.len() != 0 {
		t.Fatalf("len = %d after reset, want 0", e.len())
	}
	if _, ok := e.offset(); ok {
		t.Fatal("offset ok=true after reset, want false")
	}
}

func TestEstimatorIgnoresHighRTTOutlier(t *testing.T) {
	var e estimator
	for _, s := range []sample{
		{offset: 100, rtt: 1},
		{offset: 100, rtt: 2},
		{offset: 100, rtt: 3},
		{offset: 100, rtt: 4},
		{offset: 100, rtt: 5},
	} {
		e.add(s)
	}
	// A wild-offset, huge-RTT sample must not move the estimate.
	e.add(sample{offset: 999_999, rtt: 1_000_000})
	off, ok := e.offset()
	if !ok || off != 100 {
		t.Fatalf("median = %d, want 100 (outlier excluded)", off)
	}
}
