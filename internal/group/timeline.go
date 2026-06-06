package group

// MasterTimeline is the sample-domain analogue of media internal/sync.Timeline
// (the pattern source). Where the media timeline tracks a float64 position in
// seconds at rate 1.0, Ensemble's master timeline counts canonical-rate frames
// (int64 samples) advancing at the profile rate (Hz, A.12 default 48000). It is
// the per-group authority (doc 04 §4.4.2): transport commands drive
// Play/Pause/Seek; failover continuity uses Seed (T4/T8, doc 04 §4.4.4); the
// origin stamps every chunk from NowSample.
//
// The struct is mutex-guarded so the origin/render readers can call NowSample
// while a transport command mutates it. clock.NowMono() (process monotonic ns,
// robust to wall-clock steps) is the timebase, injectable via nowMono for tests.

import (
	"math"
	"sync"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/clock"
)

// Timeline is the per-group synchronized stream sample index (README §6.2). On
// the master it is authoritative (MasterTimeline); a follower projects it from a
// ChunkMetaSource + ClockSource (FollowerTimeline). Do not redefine.
type Timeline interface {
	NowSample() (sample int64, playing bool, ok bool)
}

// Sample is a canonical-rate frame index on the group timeline (README §6.2
// "sample index is in canonical-rate frames"). It is a transparent alias of
// int64 (identical for assignment, literals, and the exported signatures); it
// documents the sample-domain at the Seek boundary and keeps `go vet`'s
// stdmethods heuristic from misreading Seek as an io.Seeker.
type Sample = int64

// MasterTimeline is the authoritative timeline (doc 04 §4.4.2). It implements
// Timeline; NowSample's ok is always true because the master IS the reference.
type MasterTimeline struct {
	mu         sync.Mutex
	baseSample int64 // sample index at baseMono
	baseMono   int64 // master monotonic ns when baseSample was set
	rate       int   // canonical Hz (profile rate, A.12 default 48000)
	playing    bool

	// nowMono stamps the current monotonic instant; defaults to clock.NowMono.
	// Injectable so Timeline math is deterministically unit-testable (P3.2 risk 3).
	nowMono func() int64
}

// NewMasterTimeline returns a timeline paused at sample 0 (transition T1). rate
// is the canonical sample rate in Hz; a non-positive rate falls back to the A.12
// default 48000 (a profile is always negotiated before play, but never divide by
// or scale from a zero rate).
func NewMasterTimeline(rate int) *MasterTimeline {
	if rate <= 0 {
		rate = defaultRate
	}
	return &MasterTimeline{rate: rate, nowMono: clock.NowMono}
}

// Play (re)starts the timeline from fromSample: baseSample=fromSample,
// baseMono=now, playing=true. Subsequent NowSample readings advance at rate.
func (t *MasterTimeline) Play(fromSample int64) {
	t.mu.Lock()
	t.baseSample = fromSample
	t.baseMono = t.nowMono()
	t.playing = true
	t.mu.Unlock()
}

// Pause freezes the timeline at its current sample (playing=false). NowSample
// then returns the frozen sample until the next Play.
func (t *MasterTimeline) Pause() {
	t.mu.Lock()
	mono := t.nowMono()
	t.baseSample = t.atLocked(mono)
	t.baseMono = mono
	t.playing = false
	t.mu.Unlock()
}

// Seek repositions the timeline to posSample while PRESERVING the playing state
// (doc 04 §4.4.2 — mirrors media Timeline.Seek). While playing it re-bases so the
// timeline continues advancing from posSample; while paused it freezes at
// posSample. Either way the next NowSample returns posSample.
func (t *MasterTimeline) Seek(posSample Sample) {
	t.mu.Lock()
	t.baseSample = posSample
	t.baseMono = t.nowMono()
	t.mu.Unlock()
}

// Seed re-bases the timeline directly with an explicit playing state — the
// failover-continuity hook used on promotion (T4/T8, doc 04 §4.4.4): the new
// master continues from the last sample it projected as a follower, with the
// authoritative GroupRecord.Playing flag (R4).
func (t *MasterTimeline) Seed(baseSample int64, playing bool) {
	t.mu.Lock()
	t.baseSample = baseSample
	t.baseMono = t.nowMono()
	t.playing = playing
	t.mu.Unlock()
}

// NowSample returns the sample due at "now", the playing flag, and ok (always
// true on the master — it is the reference, doc 04 §4.1.3).
func (t *MasterTimeline) NowSample() (int64, bool, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.atLocked(t.nowMono()), t.playing, true
}

// Rate returns the canonical sample rate in Hz.
func (t *MasterTimeline) Rate() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rate
}

// atLocked computes the sample index at the given monotonic instant under the
// held lock (doc 04 §4.4.2):
//
//	playing ? baseSample + round((mono-baseMono)*rate/1e9) : baseSample
func (t *MasterTimeline) atLocked(mono int64) int64 {
	if !t.playing {
		return t.baseSample
	}
	return t.baseSample + projectSamples(mono-t.baseMono, t.rate)
}

// projectSamples converts an elapsed nanosecond span to canonical-rate frames
// with integer rounding (doc 04 §4.4 / A.2). Shared by master and follower so
// both round identically.
func projectSamples(elapsedNs int64, rate int) int64 {
	return int64(math.Round(float64(elapsedNs) * float64(rate) / 1e9))
}
