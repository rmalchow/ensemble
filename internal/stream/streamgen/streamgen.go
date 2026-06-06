package streamgen

// Reason classifies why the master bumps streamGen. Per doc 05 §5.8 + D22/R11 the
// bumping events are media change, seek, profile change, and EVERY master change /
// failover; a loop boundary is deliberately NOT a Reason (doc 05 §5.2.1 — the
// timeline is continuous, just more PCM). There is intentionally no "loop" or
// "reconnect" reason: the only way to advance the generation is Bump with one of
// these four reasons, so a loop boundary has no API path to bump.
type Reason int

const (
	// ReasonMediaChange is set when the operator selects new media (doc 05 §5.8;
	// Groups[].media, doc 07). FirstSampleIndex is the continuing timeline position
	// (or 0 on a fresh media start).
	ReasonMediaChange Reason = iota
	// ReasonSeek is an operator seek (doc 05 §5.8): FirstSampleIndex is the seek
	// target sampleIndex.
	ReasonSeek
	// ReasonProfileChange is a codec/FEC/rate renegotiation (doc 04 §4.3.3):
	// FirstSampleIndex is the CONTINUING sampleIndex (the timeline is continuous).
	ReasonProfileChange
	// ReasonMasterChange is a failover / takeover / rejoin-flip (D22/R11/A.5):
	// FirstSampleIndex is the current timeline sampleIndex at promotion, with the
	// replicated Playing preserved (doc 04 §4.4.4).
	ReasonMasterChange
)

// String renders a Reason for logs/tests.
func (r Reason) String() string {
	switch r {
	case ReasonMediaChange:
		return "media-change"
	case ReasonSeek:
		return "seek"
	case ReasonProfileChange:
		return "profile-change"
	case ReasonMasterChange:
		return "master-change"
	default:
		return "unknown"
	}
}

// Generation is the result of a bump: the values the origin stamps on the FIRST
// chunk of the new generation (README §6.4 header fields) plus the reset
// directives. Keyframe, ResetSeq and ResetFEC are ALWAYS true for any bump
// (doc 05 §5.8 steps 2 & 4) — they are returned explicitly so the origin wiring
// reads as a literal transcription of the spec steps rather than implicit
// behavior.
type Generation struct {
	Gen              uint64 // the new streamGen value (header offset 8)
	FirstSampleIndex int64  // sampleIndex of the first chunk of this generation (header offset 24)
	Keyframe         bool   // ALWAYS true: first chunk of any new generation is a keyframe (doc 05 §5.8 step 4)
	Playing          bool   // carried/preserved play-state (failover reads replicated Playing — doc 04 §4.4.4 / R4)
	ResetSeq         bool   // ALWAYS true: new generation restarts seq at 0 (doc 05 §5.8 step 2)
	ResetFEC         bool   // ALWAYS true: parity groups / dedupe offsets restart cleanly (doc 05 §5.8 step 2)
	Reason           Reason // the bump cause (carried for logging/telemetry; not on the wire)
}

// Controller is the master-side streamGen state machine (one per group origin).
// Not safe for concurrent use; the origin serializes Bump/Current with its send
// loop (the bump is latched and applied at a chunk boundary, doc 05 §5.8).
type Controller struct {
	gen uint64
}

// NewController constructs a Controller at initialGen. A fresh group starts at
// streamGen=0 (lifecycle: Create ⇒ streamGen=0, doc 04 §4.6); a node restored
// from replicated state starts at the replicated generation.
func NewController(initialGen uint64) *Controller {
	return &Controller{gen: initialGen}
}

// Current returns the live generation counter (the value the origin stamps on
// chunks of the current generation).
func (c *Controller) Current() uint64 { return c.gen }

// Bump advances the generation for reason and returns the directives for the
// first chunk of the new generation. atSample is the timeline position the new
// generation starts at:
//
//   - ReasonSeek          ⇒ the seek target sampleIndex
//   - ReasonMediaChange   ⇒ the continuing timeline position (or 0 on a fresh start)
//   - ReasonProfileChange ⇒ the CONTINUING sampleIndex (timeline is continuous, §4.3.3)
//   - ReasonMasterChange  ⇒ the current timeline sampleIndex at promotion (§4.4.4)
//
// playing is the play-state to preserve (failover reads the replicated
// GroupRecord.Playing). Bump ALWAYS sets Keyframe = ResetSeq = ResetFEC = true
// (doc 05 §5.8 steps 2 & 4) and advances Gen by exactly 1, so a sequence of bumps
// yields strictly monotonic generations.
func (c *Controller) Bump(reason Reason, atSample int64, playing bool) Generation {
	c.gen++
	return Generation{
		Gen:              c.gen,
		FirstSampleIndex: atSample,
		Keyframe:         true,
		Playing:          playing,
		ResetSeq:         true,
		ResetFEC:         true,
		Reason:           reason,
	}
}
