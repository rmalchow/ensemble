package render

// channel.go is the canonical channel-role + gain mapping (doc 06 §5.1/§5.2),
// lifted out of P4.9's renderer producer (the unexported pickChannel/gainLinear,
// re-homed from media internal/audio/renderer.go) into a pure, exported, testable
// form. The canonical group stream is stereo; this node's NodeRecord.Channel
// selects its role out of that stereo pair (D13, README §6.5):
//
//	stereo : passthrough  — src ch0->out ch0, src ch1->out ch1
//	left   : take src ch0, fan it out to ALL output channels
//	right  : take src ch1, fan it out to ALL output channels
//
// A stereo PAIR is two physical nodes — one left, one right — each fanning its
// selected source channel to both of its own output pins, so a stereo DAC plays
// the same mono on both pins and the pair forms a stable stereo image (06 §5.1).
//
// This file is a leaf of the audio pipeline: stdlib only (math/fmt/errors). It
// MUST NOT import group/clock/stream/state/web so the §5.1 mapping stays unit-
// testable with zero deps (doc 01 §2.2 / P6.1 §6).

import (
	"fmt"
	"math"
)

// Channel is this node's role within the canonical stereo pair (README §6.5,
// D13, doc 06 §5.1).
type Channel int

const (
	// ChannelStereo is passthrough: src ch0->out ch0, src ch1->out ch1 (clamped
	// to the available source channels). The zero value, and the default for an
	// un-set NodeRecord.Channel.
	ChannelStereo Channel = iota
	// ChannelLeft takes source ch0 and fans it out to ALL output channels.
	ChannelLeft
	// ChannelRight takes source ch1 and fans it out to ALL output channels.
	ChannelRight
)

// ParseChannel maps the §6.5 JSON string enum to a Channel. "" => ChannelStereo
// (the sensible default = full passthrough; the ConfigDoc default for an un-set
// field is the empty string, P6.1 §9 R5). An unknown value returns an error so
// the caller can default to stereo and log.
func ParseChannel(s string) (Channel, error) {
	switch s {
	case "", "stereo":
		return ChannelStereo, nil
	case "left":
		return ChannelLeft, nil
	case "right":
		return ChannelRight, nil
	default:
		return ChannelStereo, fmt.Errorf("render: unknown channel role %q", s)
	}
}

// String renders a Channel back to the §6.5 string enum.
func (c Channel) String() string {
	switch c {
	case ChannelLeft:
		return "left"
	case ChannelRight:
		return "right"
	default:
		return "stereo"
	}
}

// SelectChannel returns the source sample feeding output channel oc, per doc 06
// §5.1. srcFrame is one interleaved canonical-stereo source frame (len == source
// channels, normally 2). outCh is the sink's output channel count; oc in
// [0,outCh). It is the per-frame pick the producer applies AFTER resampling and
// BEFORE gain (doc 06 §5, §2.2). Bounds-guarded: if the wanted source channel
// index is >= len(srcFrame) it returns 0 (right on a mono source) or, for
// stereo passthrough, clamps to the last available source channel.
func SelectChannel(srcFrame []float32, role Channel, outCh, oc int) float32 {
	srcCh := len(srcFrame)
	if srcCh == 0 {
		return 0
	}
	switch role {
	case ChannelLeft:
		// src ch0 fanned to every output channel.
		return srcFrame[0]
	case ChannelRight:
		// src ch1 fanned to every output channel; 0 if the source has no ch1.
		if srcCh < 2 {
			return 0
		}
		return srcFrame[1]
	default: // ChannelStereo: passthrough, clamp to the last available channel.
		if oc < srcCh {
			return srcFrame[oc]
		}
		return srcFrame[srcCh-1]
	}
}

// GainLinear converts NodeRecord.GainDB to a linear amplitude scale (doc 06 §5.2):
// g = 10^(db/20), with 0 dB short-circuiting to 1.0 (no math.Pow). Clamping to
// [-1,1] is the sink's job, not the gain stage's. Copied verbatim from media's
// gainLinear (rename only).
func GainLinear(db float64) float32 {
	if db == 0 {
		return 1.0
	}
	return float32(math.Pow(10, db/20))
}
