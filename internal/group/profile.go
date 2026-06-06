package group

// Profile negotiation (doc 04 §4.3). The master picks the richest codec/FEC/rate
// that EVERY Render=true listener can decode, constrained to what the master can
// encode. Render=false members (incl. a sink-less master) never constrain the
// playback profile (doc 04 §4.3.1). Pure function over the listeners' effective
// Capabilities — running it only on the master gives a single writer for
// GroupRecord.profile (doc 04 §4.3.1).

import "gitlab.rand0m.me/ruben/go/ensemble/internal/state"

// Pinned defaults (A.12 / doc 04 §4.3.1).
const (
	defaultRate    = 48000 // canonical Hz
	framesPerChunk = 480   // 10 ms @ 48k (§6.4)
)

// Codec preference, richest → mandatory floor. PCM is the universal baseline
// (always decodable, dumb-node safe, D15); Opus is capability-gated (doc 04
// §4.3.2). FLAC is source-only, never a wire codec (R2), so it is absent here.
var codecRank = []string{"opus", "pcm"}

// FEC preference, richest → none (doc 04 §4.3.2). XOR parity is the default
// ceiling (D4) and the only capability-GATED mode (it needs the XOR recover
// implementation). "duplicate" (resend each packet) and "none" are universal
// floors every node can handle — exactly the dumb-node fallbacks, so they are
// not listed in a node's Caps.FEC to be available. This is what makes the doc
// 04 §4.3.2 worked example resolve to "duplicate" when the dumb N4 (Caps.FEC=
// {duplicate}, lacking xorParity) is present: the listener intersection drops
// xorParity but every listener still supports the duplicate floor.
var fecRank = []string{"xorParity", "duplicate", "none"}

// fecUniversal reports whether a FEC mode is a universal floor (no capability
// needed). Only "xorParity" must appear in a listener's Caps.FEC.
func fecUniversal(fec string) bool { return fec == "duplicate" || fec == "none" }

// Profile is the negotiated audio transport for a group (doc 04 §4.3.2). It is
// the concrete realization of state.TransportProfile's codec/FEC/rate fields;
// negotiation works purely in string enums (the name↔wire-id registry is owned
// by stream/codec, R4).
type Profile struct {
	Codec          string // "pcm"|"opus"
	FEC            string // "none"|"xorParity"|"duplicate"
	Rate           int    // canonical Hz, default 48000 (A.12)
	FramesPerChunk int    // §6.4, default 480 (A.12)
}

// NegotiateProfile picks the richest codec/FEC/rate every Render=true LISTENER
// can decode, constrained to what the MASTER can encode (doc 04 §4.3.1/§4.3.2).
// Render=false members are ignored. A zero-listener group yields the default
// profile bounded by the master's EncodeCodecs (doc 04 §4.3.4 — valid, not an
// error). FramesPerChunk is fixed at 480 (A.12).
func NegotiateProfile(members []state.NodeRecord, master state.NodeRecord) Profile {
	p := Profile{
		Codec:          "pcm", // mandatory universal floor (D15)
		FEC:            "none",
		Rate:           defaultRate,
		FramesPerChunk: framesPerChunk,
	}

	var listeners []state.NodeRecord
	for _, m := range members {
		if m.Caps.Render {
			listeners = append(listeners, m)
		}
	}

	// Codec: richest in codecRank that every listener can decode AND the master
	// can encode. With no listeners, only the master's encode ceiling applies;
	// PCM remains the floor (it is always in a full node's EncodeCodecs).
	for _, codec := range codecRank {
		if !contains(master.Caps.EncodeCodecs, codec) {
			continue
		}
		if allDecode(listeners, codec) {
			p.Codec = codec
			break
		}
	}

	// FEC: richest in fecRank every listener supports. No listeners ⇒ "none".
	if len(listeners) > 0 {
		for _, fec := range fecRank {
			if allFEC(listeners, fec) {
				p.FEC = fec
				break
			}
		}
	}

	// Rate: min over listeners' MaxRate, defaulting to 48000 (A.12). A listener
	// that reports MaxRate<=0 (unprobed) does not lower the floor.
	rate := defaultRate
	for _, l := range listeners {
		if l.Caps.MaxRate > 0 && l.Caps.MaxRate < rate {
			rate = l.Caps.MaxRate
		}
	}
	p.Rate = rate

	return p
}

// allDecode reports whether every listener's DecodeCodecs contains codec. An
// empty listener set vacuously satisfies this (the master encode ceiling decides).
func allDecode(listeners []state.NodeRecord, codec string) bool {
	for _, l := range listeners {
		if !contains(l.Caps.DecodeCodecs, codec) {
			return false
		}
	}
	return true
}

// allFEC reports whether every listener can use fec. Universal floors
// ("duplicate"/"none") are always available; "xorParity" requires the mode to be
// in each listener's Caps.FEC.
func allFEC(listeners []state.NodeRecord, fec string) bool {
	if fecUniversal(fec) {
		return true
	}
	for _, l := range listeners {
		if !contains(l.Caps.FEC, fec) {
			return false
		}
	}
	return true
}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
