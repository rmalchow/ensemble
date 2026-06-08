package calibrate

import (
	"math"

	"ensemble/internal/id"
)

// clampMs bounds an output delay to the backend's ±500 ms range (D36).
const clampMs = 500

// NodeDelay is one node's measured scheduled-emit→ear delay (docs/calibrate.md
// §4), in milliseconds on the common master-clock timeline. Confidence in [0,1]
// grades the measurement; the orchestrator filters low-confidence nodes out
// before solving (they keep their prior delay, §7).
type NodeDelay struct {
	Node       id.ID
	DelayMs    float64
	Confidence float64
}

// Applied is the output-delay value to PATCH onto a node (D36).
type Applied struct {
	Node          id.ID
	OutputDelayMs int
	DelayMs       float64 // the measured delay it was derived from
}

// Solution is the result of the delay solve: the per-node output delays plus
// the chosen alignment strategy.
type Solution struct {
	// Mode is "advance" (pull slow chains toward the fastest; lowest added
	// latency) or "delay" (push fast nodes toward the slowest; always feasible).
	Mode     string
	SpreadMs float64 // max(D) − min(D)
	Applied  []Applied
	Clamped  bool // true if any node hit the ±500 ms clamp (alignment imperfect)
}

// Solve computes per-node outputDelayMs so every node's ear-arrival lines up
// (docs/calibrate.md §5). Arrival[n] = C − outputDelayMs[n] + D[n] is equal for
// all when outputDelayMs[n] = D[n] + K; K is chosen to keep every value feasible:
//
//   - spread ≤ bufferMs − margin → advance: K = −min(D), all delays ≥ 0, the
//     slow chains are pulled toward the fastest (you can only advance a node by
//     up to one buffer's worth, so this needs the spread to fit the buffer).
//   - otherwise → delay: K = −max(D), all delays ≤ 0, the fast nodes are pushed
//     toward the slowest (unbounded — just more buffering — so always feasible).
//
// Results are rounded to ms and clamped to ±500 ms (D36). delays should already
// be confidence-filtered. With fewer than two nodes there is nothing to align.
func Solve(delays []NodeDelay, bufferMs int, marginMs float64) Solution {
	if len(delays) == 0 {
		return Solution{Mode: "advance"}
	}

	minD, maxD := delays[0].DelayMs, delays[0].DelayMs
	for _, d := range delays {
		minD = math.Min(minD, d.DelayMs)
		maxD = math.Max(maxD, d.DelayMs)
	}
	spread := maxD - minD

	// advance when the spread fits inside the buffer (with margin headroom);
	// else delay everyone toward the slowest node.
	mode := "advance"
	k := -minD
	if spread > float64(bufferMs)-marginMs {
		mode = "delay"
		k = -maxD
	}

	sol := Solution{Mode: mode, SpreadMs: spread}
	for _, d := range delays {
		ms := int(math.Round(d.DelayMs + k))
		if ms > clampMs {
			ms = clampMs
			sol.Clamped = true
		} else if ms < -clampMs {
			ms = -clampMs
			sol.Clamped = true
		}
		sol.Applied = append(sol.Applied, Applied{
			Node:          d.Node,
			OutputDelayMs: ms,
			DelayMs:       d.DelayMs,
		})
	}
	return sol
}
