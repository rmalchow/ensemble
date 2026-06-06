// Package caps owns the per-node capability pipeline: detect → mask → intersect
// → self-write (07 §2.4.1, 06 §1.5, D16). At startup (and on a reload of the
// audio.* masking keys) it computes the EFFECTIVE capability set
//
//	Caps = detected(runtime probe) ∩ enabled(per-node config)
//
// and publishes it by writing this node's OWN NodeRecord.Caps into the
// replicated ConfigDoc via the optimistic If-Match self-write (07 §3.2/§4.5);
// gossip then distributes it and the profile negotiator (04) consumes it.
//
// The sink half (Render/Sinks/MaxRate) is produced by the P4.6 sink registry
// (internal/audio/sink, 06 §1.5). caps consumes it ONLY through the SinkProber/
// MaxRateProber function-value seam (probe.go) so it builds and unit-tests
// standalone before P4.6 lands and never imports audio/sink — preserving the
// layering (doc 01 §2: caps imports only state + config, both leaves).
package caps

import (
	"slices"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/config"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// Detected is the raw runtime probe result for this machine, BEFORE config
// masking (07 §2.4.1 "detected"). It is the union of the sink registry probe
// (P4.6) and the local codec/FEC/rate detectors. All slices are sorted,
// deduplicated, canonical-string.
type Detected struct {
	Sinks        []string // usable output backend Names from the sink Probe() (06 §1.5): "alsa","exec:aplay","exec:pw-play"
	EncodeCodecs []string // wire codecs this node can ORIGINATE: "pcm" always; "opus" iff the encoder is available
	DecodeCodecs []string // wire codecs this node can PLAY: "pcm" always; "opus" iff the decoder is available
	FEC          []string // FEC schemes: "none","xorParity","duplicate" (all pure-Go, always present)
	MaxRate      int      // highest sample rate the (best precise) device accepts, Hz; 0 if no sink
}

// Mask is the per-node "enabled" intent parsed from config.AudioConfig
// (07 §2.4.2, P0.1). A nil ForceRender means "probe-driven"; non-nil false
// forces a sink-less node.
type Mask struct {
	ForceRender     *bool    // audio.render: nil => probe-driven; &false => force Render=false
	DisableBackends []string // audio.backends.disable: backends removed from detected before Sinks
	DisableCodecs   []string // audio.codecs.disable: codecs removed from Encode/DecodeCodecs
	PreferBackends  []string // audio.backends.prefer: ORDER only — does NOT add/remove caps (07 §2.4.2)
}

// MaskFromConfig projects the parsed per-node audio config (P0.1
// config.AudioConfig) into a Mask. Pure; no I/O. PreferBackends is carried
// through but never affects the effective set (07 §2.4.2).
func MaskFromConfig(a config.AudioConfig) Mask {
	return Mask{
		ForceRender:     a.Render,
		DisableBackends: a.Backends.Disable,
		DisableCodecs:   a.Codecs.Disable,
		PreferBackends:  a.Backends.Prefer,
	}
}

// Compute applies the masking config to the detected set to yield the EFFECTIVE
// Capabilities written into NodeRecord.Caps (07 §2.4.1: effective = detected ∩
// enabled). Pure; deterministic — output slices are sorted and deduplicated so
// the gossiped Caps is byte-stable and never triggers a spurious version bump
// from slice-order churn (§5.1). This is THE intersection function (D16).
func Compute(d Detected, m Mask) state.Capabilities {
	sinks := subtract(d.Sinks, m.DisableBackends)

	// Render = there is a usable+enabled sink (06 §1.5), unless ForceRender
	// forces it off. ForceRender==&true is NOT a force-on: a node with no
	// usable sink cannot render by fiat (§5.1 step 2), so we treat &true as
	// probe-driven (== nil).
	render := len(sinks) > 0
	if m.ForceRender != nil && !*m.ForceRender {
		// Force Render=false regardless of probe; Sinks emptied (07 §2.4.2; D17).
		render = false
		sinks = nil
	}

	caps := state.Capabilities{
		Render:       render,
		Sinks:        sinks,
		EncodeCodecs: subtract(d.EncodeCodecs, m.DisableCodecs),
		DecodeCodecs: subtract(d.DecodeCodecs, m.DisableCodecs),
		FEC:          canonical(d.FEC), // no FEC masking key exists (07 §2.4.2)
	}
	// MaxRate is meaningful only when there is a device to clamp; a sink-less
	// node has no output rate (§5.1 step 5; §9 open question 1).
	if render {
		caps.MaxRate = d.MaxRate
	}
	return caps
}

// subtract returns the sorted, deduplicated elements of in that are not present
// in remove. nil-safe; always returns a freshly sorted slice (or nil when
// empty) so the result is byte-stable for gossip.
func subtract(in, remove []string) []string {
	if len(in) == 0 {
		return nil
	}
	drop := make(map[string]struct{}, len(remove))
	for _, r := range remove {
		drop[r] = struct{}{}
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if _, skip := drop[v]; !skip {
			out = append(out, v)
		}
	}
	return canonical(out)
}

// canonical returns a sorted, deduplicated copy of in, or nil when empty, so
// every Caps slice is order-stable and free of duplicates.
func canonical(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.Sort(out)
	out = slices.Compact(out)
	return out
}
