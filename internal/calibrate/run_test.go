package calibrate

import (
	"context"
	"math"
	"testing"
	"time"

	"ensemble/internal/id"
)

// fakeRig is a shared Controller+Recorder: the recorder synthesizes a capture
// for whichever node the controller currently has un-muted, placing the sweep at
// that node's true acoustic delay on a continuous master-clock loop grid whose
// origin advances between recordings (so the cross-time mod-period reduction is
// genuinely exercised).
type fakeRig struct {
	ref       *Reference
	trueDelay map[id.ID]float64 // node → true acoustic delay, ms
	vol       map[id.ID]float64
	delaySet  map[id.ID]int
	played    bool

	phi0  int64 // master-clock ns of loop 0's emit boundary
	nowNs int64 // virtual master clock, advanced each Record
}

func (f *fakeRig) periodNs() int64 {
	return int64(float64(f.ref.Period) / float64(f.ref.Cfg.SampleRate) * 1e9)
}

func (f *fakeRig) isolated() (id.ID, bool) {
	var best id.ID
	bestV := 0.0
	for n, v := range f.vol {
		if v > bestV {
			bestV, best = v, n
		}
	}
	return best, bestV > 0
}

func (f *fakeRig) PlayReference(context.Context) error { f.played = true; return nil }
func (f *fakeRig) StopReference(context.Context) error { f.played = false; return nil }
func (f *fakeRig) SetVolume(_ context.Context, n id.ID, v float64) error {
	f.vol[n] = v
	return nil
}
func (f *fakeRig) SetOutputDelay(_ context.Context, n id.ID, ms int) error {
	f.delaySet[n] = ms
	return nil
}

func (f *fakeRig) Record(_ context.Context, d time.Duration) (Recording, error) {
	node, ok := f.isolated()
	sr := f.ref.Cfg.SampleRate
	nsPer := 1e9 / float64(sr)
	n := int(float64(d) / nsPer)
	mono := make([]float32, n)
	T0 := f.nowNs
	if ok {
		P := f.periodNs()
		delayNs := int64(f.trueDelay[node] * 1e6)
		k := (T0-f.phi0-delayNs)/P - 1
		for safety := 0; safety < 1000; safety++ {
			arr := f.phi0 + k*P + delayNs
			k++
			if arr < T0 {
				continue
			}
			if arr >= T0+int64(d) {
				break
			}
			idx := int(float64(arr-T0) / nsPer)
			for i, s := range f.ref.Sweep {
				if idx+i < n {
					mono[idx+i] += s
				}
			}
		}
	}
	f.nowNs += int64(d) // advance the virtual clock between recordings
	return Recording{Mono: mono, T0Master: T0, SampleRate: sr}, nil
}

func TestRunRecoversAndAppliesAlignment(t *testing.T) {
	ref := NewReference(Config{SampleRate: 48000, SweepMs: 50, PeriodMs: 200})
	a, b, c := idN(1), idN(2), idN(3)
	rig := &fakeRig{
		ref:       ref,
		trueDelay: map[id.ID]float64{a: 8, b: 23, c: 15}, // ms
		vol:       map[id.ID]float64{a: 0.5, b: 0.5, c: 0.5},
		delaySet:  map[id.ID]int{},
		phi0:      1_000_000_000,
		nowNs:     5_000_000_000,
	}
	plan := Plan{
		Master:     a,
		Members:    []id.ID{a, b, c},
		BufferMs:   150,
		OrigVolume: map[id.ID]float64{a: 0.5, b: 0.5, c: 0.5},
	}
	opt := Options{Ref: ref, Loops: 3, SettleMs: 1, Volume: 0.8}

	rep, err := Run(context.Background(), rig, rig, plan, opt, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.Applied {
		t.Fatal("expected delays applied")
	}

	// Every node should have had an output delay written.
	for _, n := range plan.Members {
		if _, ok := rig.delaySet[n]; !ok {
			t.Errorf("no output delay written for %v", n)
		}
	}
	// The applied delays must preserve true pairwise acoustic-delay differences:
	// the slowest chain (b, 23 ms) is advanced most relative to the fastest (a).
	toMs := func(v int) float64 { return float64(v) }
	dab := toMs(rig.delaySet[b]) - toMs(rig.delaySet[a])
	if math.Abs(dab-(23-8)) > 1 {
		t.Errorf("b-a output delay diff = %.1f, want ~15", dab)
	}
	dcb := toMs(rig.delaySet[c]) - toMs(rig.delaySet[b])
	if math.Abs(dcb-(15-23)) > 1 {
		t.Errorf("c-b output delay diff = %.1f, want ~-8", dcb)
	}

	// Volumes restored and the reference stopped on exit.
	for _, n := range plan.Members {
		if rig.vol[n] != 0.5 {
			t.Errorf("volume of %v = %.2f, want restored 0.5", n, rig.vol[n])
		}
	}
	if rig.played {
		t.Error("reference still playing after run")
	}
}

func TestRunNeedsTwoMembers(t *testing.T) {
	ref := NewReference(Config{})
	rig := &fakeRig{ref: ref, vol: map[id.ID]float64{}, delaySet: map[id.ID]int{}}
	_, err := Run(context.Background(), rig, rig, Plan{Members: []id.ID{idN(1)}}, Options{Ref: ref}, nil)
	if err == nil {
		t.Fatal("expected an error with a single member")
	}
}
