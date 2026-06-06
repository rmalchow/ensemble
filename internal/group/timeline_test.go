package group

import (
	"testing"
	"time"
)

// fakeMono is an injectable monotonic clock for deterministic Timeline tests
// (P3.2 risk 3: clock.NowMono is otherwise unmockable).
type fakeMono struct{ ns int64 }

func (f *fakeMono) now() int64   { return f.ns }
func (f *fakeMono) set(ns int64) { f.ns = ns }

func TestMasterTimeline_PlayPauseSeekSeed(t *testing.T) {
	const rate = 48000
	tests := []struct {
		name       string
		drive      func(tl *MasterTimeline, m *fakeMono)
		atNs       int64
		wantSample int64
		wantPlay   bool
	}{
		{
			name:       "paused at zero (T1)",
			drive:      func(tl *MasterTimeline, m *fakeMono) {},
			atNs:       1_000_000_000,
			wantSample: 0,
			wantPlay:   false,
		},
		{
			name: "play advances at rate",
			drive: func(tl *MasterTimeline, m *fakeMono) {
				m.set(0)
				tl.Play(0)
			},
			atNs:       1_000_000_000, // 1s @ 48k = 48000 frames
			wantSample: 48000,
			wantPlay:   true,
		},
		{
			name: "play from non-zero base",
			drive: func(tl *MasterTimeline, m *fakeMono) {
				m.set(500_000_000)
				tl.Play(96000)
			},
			atNs:       1_000_000_000, // +0.5s = +24000 frames
			wantSample: 120000,
			wantPlay:   true,
		},
		{
			name: "pause freezes at current sample",
			drive: func(tl *MasterTimeline, m *fakeMono) {
				m.set(0)
				tl.Play(0)
				m.set(1_000_000_000)
				tl.Pause() // freeze at 48000
			},
			atNs:       5_000_000_000, // time moves on; frozen
			wantSample: 48000,
			wantPlay:   false,
		},
		{
			name: "seek preserves playing and re-bases",
			drive: func(tl *MasterTimeline, m *fakeMono) {
				m.set(0)
				tl.Play(0)
				m.set(2_000_000_000)
				tl.Seek(96000)
			},
			atNs:       3_000_000_000, // +1s after seek = +48000
			wantSample: 144000,
			wantPlay:   true,
		},
		{
			name: "seek while paused freezes at pos",
			drive: func(tl *MasterTimeline, m *fakeMono) {
				m.set(0)
				tl.Seek(12345) // paused
			},
			atNs:       9_000_000_000,
			wantSample: 12345,
			wantPlay:   false,
		},
		{
			name: "seed paused for failover continuity (T4 not playing)",
			drive: func(tl *MasterTimeline, m *fakeMono) {
				m.set(0)
				tl.Seed(200000, false)
			},
			atNs:       1_000_000_000,
			wantSample: 200000,
			wantPlay:   false,
		},
		{
			name: "seed playing resumes from seeded sample (T4 playing)",
			drive: func(tl *MasterTimeline, m *fakeMono) {
				m.set(1_000_000_000)
				tl.Seed(200000, true)
			},
			atNs:       2_000_000_000, // +1s = +48000
			wantSample: 248000,
			wantPlay:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := &fakeMono{}
			tl := NewMasterTimeline(rate)
			tl.nowMono = m.now
			tc.drive(tl, m)
			m.set(tc.atNs)
			got, playing, ok := tl.NowSample()
			if !ok {
				t.Fatalf("master NowSample ok=false; master is always ok")
			}
			if got != tc.wantSample {
				t.Errorf("sample = %d, want %d", got, tc.wantSample)
			}
			if playing != tc.wantPlay {
				t.Errorf("playing = %v, want %v", playing, tc.wantPlay)
			}
		})
	}
}

func TestMasterTimeline_RateDefault(t *testing.T) {
	if got := NewMasterTimeline(0).Rate(); got != defaultRate {
		t.Errorf("rate fallback = %d, want %d", got, defaultRate)
	}
	if got := NewMasterTimeline(44100).Rate(); got != 44100 {
		t.Errorf("rate = %d, want 44100", got)
	}
}

// fakeChunks is a settable ChunkMetaSource.
type fakeChunks struct {
	meta ChunkMeta
	have bool
}

func (f *fakeChunks) LatestChunkMeta() (ChunkMeta, bool) { return f.meta, f.have }

// fakeClock is a settable ClockSource.
type fakeClock struct {
	off      time.Duration
	offOK    bool
	minDelay time.Duration
	minOK    bool
}

func (f *fakeClock) Offset() (time.Duration, bool)   { return f.off, f.offOK }
func (f *fakeClock) MinDelay() (time.Duration, bool) { return f.minDelay, f.minOK }

func TestFollowerTimeline_WorkedVector(t *testing.T) {
	// doc 04 §4.5 worked example: anchor (96000, masterMono=3_000_000ns),
	// Offset=+812_000ns, clock.NowMono()=2_400_000ns, rate=48000 ⇒ 96010.
	m := &fakeMono{ns: 2_400_000}
	chunks := &fakeChunks{
		meta: ChunkMeta{SampleIndex: 96000, MasterMono: 3_000_000, StreamGen: 7, Playing: true},
		have: true,
	}
	clk := &fakeClock{off: 812_000 * time.Nanosecond, offOK: true, minDelay: 1_400_000, minOK: true}

	f := NewFollowerTimeline(chunks, clk, 48000)
	f.nowMono = m.now
	f.SetStreamGen(7)

	got, playing, ok := f.NowSample()
	if !ok {
		t.Fatalf("ok=false; want true (offset ok + current-gen chunk)")
	}
	if !playing {
		t.Errorf("playing=false; want true")
	}
	if got != 96010 {
		t.Errorf("NowSample = %d, want 96010 (doc 04 §4.5)", got)
	}
}

func TestFollowerTimeline_Gating(t *testing.T) {
	m := &fakeMono{ns: 2_400_000}
	base := ChunkMeta{SampleIndex: 96000, MasterMono: 3_000_000, StreamGen: 7, Playing: true}

	tests := []struct {
		name    string
		chunks  *fakeChunks
		clk     *fakeClock
		gen     uint64
		wantOK  bool
		wantSmp int64
		wantPly bool
	}{
		{
			name:   "offset not ok ⇒ not synced",
			chunks: &fakeChunks{meta: base, have: true},
			clk:    &fakeClock{offOK: false},
			gen:    7,
			wantOK: false,
		},
		{
			name:   "no chunk yet ⇒ not synced",
			chunks: &fakeChunks{have: false},
			clk:    &fakeClock{off: 812_000, offOK: true},
			gen:    7,
			wantOK: false,
		},
		{
			name:   "streamGen mismatch ⇒ not synced (A.2)",
			chunks: &fakeChunks{meta: base, have: true}, // chunk gen 7
			clk:    &fakeClock{off: 812_000, offOK: true},
			gen:    8, // engine moved to gen 8
			wantOK: false,
		},
		{
			name:    "current-gen chunk ⇒ synced",
			chunks:  &fakeChunks{meta: base, have: true},
			clk:     &fakeClock{off: 812_000 * time.Nanosecond, offOK: true},
			gen:     7,
			wantOK:  true,
			wantSmp: 96010,
			wantPly: true,
		},
		{
			name: "paused master freezes at sample",
			chunks: &fakeChunks{meta: ChunkMeta{
				SampleIndex: 96000, MasterMono: 3_000_000, StreamGen: 7, Playing: false,
			}, have: true},
			clk:     &fakeClock{off: 812_000 * time.Nanosecond, offOK: true},
			gen:     7,
			wantOK:  true,
			wantSmp: 96000, // frozen, no projection
			wantPly: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFollowerTimeline(tc.chunks, tc.clk, 48000)
			f.nowMono = m.now
			f.SetStreamGen(tc.gen)
			got, playing, ok := f.NowSample()
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got != tc.wantSmp {
				t.Errorf("sample = %d, want %d", got, tc.wantSmp)
			}
			if playing != tc.wantPly {
				t.Errorf("playing = %v, want %v", playing, tc.wantPly)
			}
		})
	}
}

func TestFollowerTimeline_ImplementsTimeline(t *testing.T) {
	var _ Timeline = (*FollowerTimeline)(nil)
	var _ Timeline = (*MasterTimeline)(nil)
}
