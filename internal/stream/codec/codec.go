// Package codec is the wire-codec layer (README §6.3, doc 05 §5.4). A Codec turns
// one chunk of canonical-rate interleaved float32 PCM into a wire payload and back.
// The internal pipeline is float32; PCM narrows to S16LE only at this boundary
// (A.10 m5). It is a strict leaf: it imports no sibling internal packages (the
// integer CodecID lives here, parallel to the wire-layer header id; see doc 01
// §2.1). P4.3 ships only the PCM baseline; OPUS is reserved for P4.4.
package codec

import "errors"

// CodecID is the integer codec identifier carried at the §6.4 wire layer (header
// offset 6). JSON/ConfigDoc use the string names (README §6.5); these integers
// exist ONLY on the wire.
type CodecID uint8

const (
	PCM  CodecID = 0 // S16LE, mandatory baseline (doc 05 §5.4.1)
	OPUS CodecID = 1 // lossy, optional/capability-gated (doc 05 §5.4.2) — P4.4
)

// Codec is the spine interface (README §6.3), implemented verbatim. A Codec is
// stateless and immutable after construction, so a single instance is safe for
// concurrent Encode/Decode (the origin encodes once, doc 05 §5.2.3).
type Codec interface {
	ID() CodecID                              // PCM=0, OPUS=1
	Encode(pcm []float32) ([]byte, error)     // master side; one chunk in, one frame out
	Decode(payload []byte) ([]float32, error) // follower side; one frame in, one chunk out
}

// Sentinel errors (wrapped with %w by callers).
var (
	// ErrUnsupportedCodec is returned by New for an id not implemented in this
	// build (e.g. OPUS in P4.3) or an unknown id.
	ErrUnsupportedCodec = errors.New("codec: unsupported codec id")
	// ErrShortPayload is returned by Decode for a payload that is not a whole
	// number of S16LE frames.
	ErrShortPayload = errors.New("codec: payload not a whole number of S16LE frames")
	// ErrChunkAlloc is returned by Encode for a pcm slice that is not a whole
	// number of frames (len % channels != 0).
	ErrChunkAlloc = errors.New("codec: pcm length not a multiple of channel count")
)

// opusFactory is the in-package registration hook the optional Opus codec fills
// (P5.2 §4.3, §9 Q1). It is nil on the default build (no `opus` build tag), in
// which case New(OPUS) returns ErrUnsupportedCodec exactly as P4.3 specifies.
// The `opus`-tagged opus_register.go sets it from an init() so the leaf `codec`
// package stays the single construction authority for every CodecID — callers
// never reach the Opus constructor except through New(OPUS). It is assigned once
// at init (before any New call) and only read thereafter, so it needs no lock.
var opusFactory func(rate, channels, frameSamples, bitrate int) (Codec, error)

// New returns the codec for id, or ErrUnsupportedCodec for an unknown or
// not-yet-implemented id. PCM is always constructible (always stereo, the
// canonical channel count, doc 05 §5.1). OPUS is constructible ONLY when the
// optional `opus`-tagged build registered opusFactory (P5.2): the default
// CGO_ENABLED=0 build leaves opusFactory nil so New(OPUS) keeps returning
// ErrUnsupportedCodec and negotiation falls to the PCM floor (doc 04 §4.3.2).
// The Opus codec is constructed at the canonical rate/channels/frame size
// (A.12) and a default starting bitrate (doc 05 §5.11, §9 Q2); profile-driven
// bitrate selection lives upstream and uses NewOpus directly.
func New(id CodecID) (Codec, error) {
	switch id {
	case PCM:
		return NewPCM(canonicalChannels), nil
	case OPUS:
		if opusFactory == nil {
			return nil, ErrUnsupportedCodec
		}
		return opusFactory(canonicalRate, canonicalChannels, canonicalFrameSamples, defaultOpusBitrate)
	default:
		return nil, ErrUnsupportedCodec
	}
}

// Canonical audio constants (A.12: "48000 / 2 / 480 (10 ms)"). PCM stays generic
// (NewPCM takes channels) so the dumb-node/mono future (doc 10) needs no rewrite;
// New uses these defaults, and the Opus constructor is configured for the same
// canonical frame so one Opus frame == one 10 ms chunk (doc 05 §5.4.2).
const (
	canonicalRate         = 48000 // canonical sample rate, Hz
	canonicalChannels     = 2     // canonical interleaved channel count
	canonicalFrameSamples = 480   // samples/channel per 10 ms chunk (FramesPerChunk)
)

// defaultOpusBitrate is the starting Opus bitrate used by New(OPUS) (doc 05
// §5.11 lists @128k as a typical music estimate; A.12 pins no number — §9 Q2).
// Profile-driven bitrate policy lives upstream and calls NewOpus directly with
// the negotiated value; this default only serves the generic New(OPUS) path.
const defaultOpusBitrate = 128000

// --- name↔id registry (README §6.5: string enums in JSON, integer IDs on the wire) ---
//
// The table is a small fixed slice indexed by id: immutable, allocation-free, no
// map and no sync (cheapest on the Pi). It lists EVERY enum name so profile
// negotiation (P4.2) and state (P2.1) can round-trip string↔id for any advertised
// capability; the constructor New gates what is actually buildable in this build.

// codecNames is the canonical id->string table (README §6.5 / doc 05 §6.3),
// indexed by CodecID. The reverse direction is derived from it so the two can
// never disagree.
var codecNames = [...]string{
	PCM:  "pcm",
	OPUS: "opus",
}

// NameOf maps a CodecID to its canonical JSON/ConfigDoc string ("pcm"|"opus").
// Returns ("", false) for an unknown id.
func NameOf(id CodecID) (string, bool) {
	if int(id) >= len(codecNames) {
		return "", false
	}
	return codecNames[id], true
}

// FromName maps a canonical string ("pcm"|"opus") to its CodecID. Returns
// (0, false) for an unknown name.
func FromName(name string) (CodecID, bool) {
	for id, n := range codecNames {
		if n == name {
			return CodecID(id), true
		}
	}
	return 0, false
}
