package calibrate

import (
	"context"
	"fmt"
	"time"

	"ensemble/internal/id"
)

// Controller drives the group during a calibration run (docs/calibrate.md §6).
// It is implemented in the API/daemon layer over the peer HTTP client and faked
// in tests. Every method addresses a node by id; the implementation routes to
// the local node or a peer transparently.
type Controller interface {
	// PlayReference starts the looped reference signal on the group master and
	// returns once it is playing.
	PlayReference(ctx context.Context) error
	// StopReference stops playback on the master.
	StopReference(ctx context.Context) error
	// SetVolume sets a node's playout volume in [0,1].
	SetVolume(ctx context.Context, node id.ID, v float64) error
	// SetOutputDelay sets a node's output-delay calibration in ms (D36).
	SetOutputDelay(ctx context.Context, node id.ID, ms int) error
}

// Recording is a mono microphone capture mapped to the master clock: Mono[i] was
// captured at master-clock time T0Master + i/SampleRate seconds.
type Recording struct {
	Mono       []float32
	T0Master   int64 // master-clock ns of Mono[0]
	SampleRate int
}

// Recorder captures the local microphone, stamped against the master clock. The
// real implementation drives the input source + clock follower; tests inject a
// synthetic recorder with exact timing.
type Recorder interface {
	Record(ctx context.Context, d time.Duration) (Recording, error)
}

// Plan is the static input to a run: the group geometry and the pre-run volumes
// (restored on exit) — captured by the caller from the live snapshot.
type Plan struct {
	Master     id.ID
	Members    []id.ID           // measured in this order
	BufferMs   int               // group buffer (advance feasibility, §5)
	OrigVolume map[id.ID]float64 // pre-run volume per member, restored after
}

// Options tunes a run; zero fields fall back to sensible defaults.
type Options struct {
	Ref           *Reference // reference signal (default DefaultConfig)
	Volume        float64    // isolation level for the node under test (default 0.8)
	Loops         int        // loop periods recorded per node (default 6)
	SettleMs      int        // wait after a volume change before recording (default 400)
	MinConfidence float64    // nodes below this keep their prior delay (default 0.3)
	MarginMs      float64    // buffer headroom reserved for advance mode (default 20)
}

func (o Options) withDefaults() Options {
	if o.Ref == nil {
		o.Ref = NewReference(Config{})
	}
	if o.Volume <= 0 {
		o.Volume = 0.8
	}
	if o.Loops <= 0 {
		o.Loops = 6
	}
	if o.SettleMs <= 0 {
		o.SettleMs = 400
	}
	if o.MinConfidence <= 0 {
		o.MinConfidence = 0.3
	}
	if o.MarginMs <= 0 {
		o.MarginMs = 20
	}
	return o
}

// Measurement is one node's per-node result.
type Measurement struct {
	Node            id.ID
	ArrivalMasterNs int64   // master-clock time the sweep reached the mic
	LagSamples      float64 // sweep position within its recording
	Confidence      float64
	DelayMs         float64 // loop-phase delay, relative to the run (used by Solve)
	Used            bool    // included in the solve (confidence ≥ threshold)
}

// Report is the outcome of a run.
type Report struct {
	Master       id.ID
	PeriodMs     float64
	Measurements []Measurement
	Solution     Solution
	Applied      bool // output delays were written
}

// Progress is streamed to the caller (→ WebSocket) as the run advances.
type Progress struct {
	Phase      string // start|measuring|measured|solving|applying|done|error
	Node       id.ID
	Index      int
	Total      int
	Confidence float64
	Message    string
}

// Run executes a calibration pass over the group (docs/calibrate.md §4–§6): play
// the reference loop, isolate and measure each member, solve the alignment, and
// PATCH each node's outputDelayMs. It always restores pre-run volumes and stops
// the reference on exit — including on error or context cancellation — and never
// half-writes the delay set (all measurement happens before any delay is
// applied). progress may be nil.
func Run(ctx context.Context, ctrl Controller, rec Recorder, plan Plan, opt Options, progress func(Progress)) (*Report, error) {
	opt = opt.withDefaults()
	ref := opt.Ref
	report := func(p Progress) {
		if progress != nil {
			progress(p)
		}
	}

	if len(plan.Members) < 2 {
		return nil, fmt.Errorf("calibrate: need at least 2 members, have %d", len(plan.Members))
	}

	// Restore volumes and stop the reference no matter how we leave. Uses a fresh
	// context so cleanup still runs when ctx is already cancelled.
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, m := range plan.Members {
			if v, ok := plan.OrigVolume[m]; ok {
				_ = ctrl.SetVolume(rctx, m, v)
			}
		}
		_ = ctrl.StopReference(rctx)
	}()

	report(Progress{Phase: "start", Total: len(plan.Members), Message: "starting reference"})
	if err := ctrl.PlayReference(ctx); err != nil {
		report(Progress{Phase: "error", Message: "play reference: " + err.Error()})
		return nil, fmt.Errorf("calibrate: play reference: %w", err)
	}

	settle := time.Duration(opt.SettleMs) * time.Millisecond
	recDur := time.Duration(opt.Loops)*time.Duration(ref.Cfg.PeriodMs)*time.Millisecond + settle

	measurements := make([]Measurement, 0, len(plan.Members))
	for i, n := range plan.Members {
		report(Progress{Phase: "measuring", Node: n, Index: i, Total: len(plan.Members),
			Message: "isolating node"})

		// isolate: node under test at the known level, everyone else muted.
		for _, m := range plan.Members {
			v := 0.0
			if m == n {
				v = opt.Volume
			}
			if err := ctrl.SetVolume(ctx, m, v); err != nil {
				return nil, fmt.Errorf("calibrate: set volume %s: %w", m, err)
			}
		}
		if err := sleep(ctx, settle); err != nil {
			return nil, err
		}

		r, err := rec.Record(ctx, recDur)
		if err != nil {
			return nil, fmt.Errorf("calibrate: record %s: %w", n, err)
		}
		est, ok := ref.EstimateDelay(r.Mono)
		mm := Measurement{Node: n, Confidence: est.Confidence, LagSamples: est.LagSamples}
		if ok {
			sr := r.SampleRate
			if sr <= 0 {
				sr = ref.Cfg.SampleRate
			}
			mm.ArrivalMasterNs = r.T0Master + int64(est.LagSamples/float64(sr)*1e9)
			mm.Used = est.Confidence >= opt.MinConfidence
		}
		measurements = append(measurements, mm)
		report(Progress{Phase: "measured", Node: n, Index: i, Total: len(plan.Members),
			Confidence: est.Confidence})
	}

	// Reduce the absolute master-clock arrivals to loop-phase delays. Only
	// pairwise DIFFERENCES matter (the common mic-capture latency cancels), so we
	// phase-reference everyone to the first used node and unwrap into one period.
	report(Progress{Phase: "solving", Message: "computing alignment"})
	periodNs := int64(float64(ref.Period) / float64(ref.Cfg.SampleRate) * 1e9)
	pivot, havePivot := int64(0), false
	for i := range measurements {
		if measurements[i].Used {
			pivot, havePivot = measurements[i].ArrivalMasterNs, true
			break
		}
	}
	if !havePivot {
		report(Progress{Phase: "error", Message: "no node measured with enough confidence"})
		return &Report{Master: plan.Master, PeriodMs: float64(periodNs) / 1e6,
			Measurements: measurements}, fmt.Errorf("calibrate: no confident measurements")
	}

	var delays []NodeDelay
	for i := range measurements {
		if !measurements[i].Used {
			continue
		}
		rel := wrapNs(measurements[i].ArrivalMasterNs-pivot, periodNs)
		measurements[i].DelayMs = float64(rel) / 1e6
		delays = append(delays, NodeDelay{
			Node:       measurements[i].Node,
			DelayMs:    measurements[i].DelayMs,
			Confidence: measurements[i].Confidence,
		})
	}

	sol := Solve(delays, plan.BufferMs, opt.MarginMs)

	report(Progress{Phase: "applying", Message: "writing output delays"})
	for _, a := range sol.Applied {
		if err := ctrl.SetOutputDelay(ctx, a.Node, a.OutputDelayMs); err != nil {
			return nil, fmt.Errorf("calibrate: set output delay %s: %w", a.Node, err)
		}
	}

	report(Progress{Phase: "done", Total: len(plan.Members), Message: "calibration complete"})
	return &Report{
		Master:       plan.Master,
		PeriodMs:     float64(periodNs) / 1e6,
		Measurements: measurements,
		Solution:     sol,
		Applied:      true,
	}, nil
}

// wrapNs reduces x into (−P/2, P/2], so loop-phase delays that differ by less
// than half a period are compared on the same branch (true acoustic delays of
// tens of ms sit well inside a 500 ms period).
func wrapNs(x, period int64) int64 {
	if period <= 0 {
		return x
	}
	x %= period
	if x > period/2 {
		x -= period
	} else if x <= -period/2 {
		x += period
	}
	return x
}

// sleep waits for d or until ctx is cancelled.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
