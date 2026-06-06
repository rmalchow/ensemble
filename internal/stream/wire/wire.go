// Package wire defines the ESND audio packet: a fixed 44-byte big-endian header
// (README §6.4) followed by an opaque payload, with allocation-conscious
// Marshal/Unmarshal and the name<->id registry that maps the ConfigDoc/API
// string codec/FEC enums ("pcm"|"opus", "none"|"xorParity"|"duplicate") to/from
// the integer CodecID/FECID that appear only on the wire (README §6.5, 07 §6).
//
// It is the lowest layer of the audio plane: pure bytes<->structs with no audio
// logic. It never encodes, decodes, FECs, paces, or touches a socket; both the
// origin (master, 05 §5.2) and the receiver (follower, 05 §5.6), and the
// codec/fec packages (P3.2), build on it. The 44-byte layout lives here exactly
// once so the rest of stream/* never hand-rolls offsets.
//
// Allocation discipline: MarshalInto and Unmarshal are allocation-free on the
// hot path (caller-owned buffers; payload returned as a subslice). Unmarshal
// strictly validates untrusted input and never panics on hostile bytes
// (03 §6.3).
//
// Leaf: imports Go stdlib only (01 §2). It must not import any sibling internal
// package — codec/fec import wire, not the other way around.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Layout constants (README §6.4 — the only normative source; not restated elsewhere).
const (
	Magic      uint32 = 0x45534E44 // 'E''S''N''D', big-endian on the wire at offset 0
	Version    uint8  = 1          // header version, fixed =1 this build (README §6.4)
	HeaderSize        = 44         // fixed header size in bytes (README §6.4)
)

// maxPayload is the largest payload PayloadLen (uint16) can describe.
const maxPayload = 1<<16 - 1 // 65535

// Field byte offsets within the header (README §6.4 / 05 §5.10).
const (
	offMagic       = 0  // 4  uint32  'ESND'
	offVersion     = 4  // 1  uint8   =1
	offFlags       = 5  // 1  uint8   bit0 repair, bit1 keyframe
	offCodecID     = 6  // 1  uint8   CodecID
	offFECID       = 7  // 1  uint8   FECID
	offStreamGen   = 8  // 8  uint64  group stream generation (§5.8)
	offSeq         = 16 // 8  uint64  monotonic source packet sequence
	offSampleIndex = 24 // 8  int64   first sample's canonical-rate frame index
	offMasterMono  = 32 // 8  int64   master monotonic ns when sourced
	offPayloadLen  = 40 // 2  uint16  payload length in bytes
	offRate100     = 42 // 2  uint16  rate/100 (480 => 48000 Hz)
)

// Errors returned by Unmarshal/Marshal. Sentinels so callers can errors.Is them.
var (
	ErrShort      = errors.New("wire: buffer shorter than header")
	ErrMagic      = errors.New("wire: bad magic (not 'ESND')")
	ErrVersion    = errors.New("wire: unsupported header version")
	ErrPayloadLen = errors.New("wire: payloadLen exceeds buffer")
	ErrOverflow   = errors.New("wire: payload length overflows uint16")
)

// Flags is the header flags bitset (README §6.4, offset 5).
type Flags uint8

const (
	FlagRepair   Flags = 1 << 0 // bit0: this is an FEC repair packet (§5.5.1)
	FlagKeyframe Flags = 1 << 1 // bit1: decodable cold (always set for PCM; Opus keyframes) (§5.4)
)

// Repair reports whether bit0 (FEC repair packet) is set.
func (f Flags) Repair() bool { return f&FlagRepair != 0 }

// Keyframe reports whether bit1 (cold-decodable keyframe) is set.
func (f Flags) Keyframe() bool { return f&FlagKeyframe != 0 }

// Header is the parsed ESND header (README §6.4). Field order/types mirror the
// wire layout. SampleIndex/MasterMono are signed (timeline frame index /
// monotonic ns, both diffable); StreamGen/Seq are unsigned monotonic counters.
//
// Magic and Version are NOT stored: they are constants this build always writes
// and always validates; carrying them as fields would invite drift.
type Header struct {
	Flags       Flags   // offset 5
	CodecID     CodecID // offset 6
	FECID       FECID   // offset 7
	StreamGen   uint64  // offset 8  (§5.8)
	Seq         uint64  // offset 16
	SampleIndex int64   // offset 24 (canonical-rate frame index on the group timeline)
	MasterMono  int64   // offset 32 (master monotonic ns at source, §5.2.2)
	PayloadLen  uint16  // offset 40 (length of the payload that follows)
	Rate100     uint16  // offset 42 (rate/100; 480 => 48000 Hz; sanity/redundancy)
}

// MarshalInto writes the 44-byte big-endian header into dst[:HeaderSize] and is
// the allocation-free hot path for the origin (05 §5.2.3: encode once, fan out).
// It writes ONLY the header bytes and does not touch dst[HeaderSize:]. h.PayloadLen
// is taken as authoritative (the caller sets it = len(payload)). Returns ErrShort
// if len(dst) < HeaderSize.
func MarshalInto(dst []byte, h Header) error {
	if len(dst) < HeaderSize {
		return ErrShort
	}
	binary.BigEndian.PutUint32(dst[offMagic:], Magic)
	dst[offVersion] = Version
	dst[offFlags] = byte(h.Flags)
	dst[offCodecID] = byte(h.CodecID)
	dst[offFECID] = byte(h.FECID)
	binary.BigEndian.PutUint64(dst[offStreamGen:], h.StreamGen)
	binary.BigEndian.PutUint64(dst[offSeq:], h.Seq)
	binary.BigEndian.PutUint64(dst[offSampleIndex:], uint64(h.SampleIndex))
	binary.BigEndian.PutUint64(dst[offMasterMono:], uint64(h.MasterMono))
	binary.BigEndian.PutUint16(dst[offPayloadLen:], h.PayloadLen)
	binary.BigEndian.PutUint16(dst[offRate100:], h.Rate100)
	return nil
}

// Marshal returns a freshly-allocated []byte of HeaderSize+len(payload) bytes:
// the big-endian header followed by payload verbatim. It sets PayloadLen from
// len(payload) defensively and returns ErrOverflow if that exceeds uint16. It is
// the convenience wrapper over MarshalInto for cold paths and tests.
func Marshal(h Header, payload []byte) ([]byte, error) {
	if len(payload) > maxPayload {
		return nil, fmt.Errorf("%w: %d bytes", ErrOverflow, len(payload))
	}
	h.PayloadLen = uint16(len(payload))
	buf := make([]byte, HeaderSize+len(payload))
	// MarshalInto cannot fail here: buf is sized >= HeaderSize.
	_ = MarshalInto(buf, h)
	copy(buf[HeaderSize:], payload)
	return buf, nil
}

// Unmarshal parses one received datagram (05 §5.6.2 step 3). It validates magic,
// version, len(buf) >= HeaderSize, and HeaderSize+PayloadLen <= len(buf); on any
// failure it returns a non-nil error with the zero Header and nil payload. The
// returned payload is a SUBSLICE of buf (no copy), valid only while buf is
// unmodified — the receiver's reorder window must Clone before retaining it.
//
// CodecID/FECID are NOT range-validated here: an unknown id is the codec/FEC
// layer's concern (it maps via the registry and drops on false). wire guarantees
// only structural integrity, keeping it codec-agnostic (§5.3).
func Unmarshal(buf []byte) (Header, []byte, error) {
	if len(buf) < HeaderSize {
		return Header{}, nil, ErrShort
	}
	if binary.BigEndian.Uint32(buf[offMagic:]) != Magic {
		return Header{}, nil, ErrMagic
	}
	if buf[offVersion] != Version {
		return Header{}, nil, ErrVersion
	}
	payloadLen := binary.BigEndian.Uint16(buf[offPayloadLen:])
	if HeaderSize+int(payloadLen) > len(buf) {
		return Header{}, nil, ErrPayloadLen
	}
	h := Header{
		Flags:       Flags(buf[offFlags]),
		CodecID:     CodecID(buf[offCodecID]),
		FECID:       FECID(buf[offFECID]),
		StreamGen:   binary.BigEndian.Uint64(buf[offStreamGen:]),
		Seq:         binary.BigEndian.Uint64(buf[offSeq:]),
		SampleIndex: int64(binary.BigEndian.Uint64(buf[offSampleIndex:])),
		MasterMono:  int64(binary.BigEndian.Uint64(buf[offMasterMono:])),
		PayloadLen:  payloadLen,
		Rate100:     binary.BigEndian.Uint16(buf[offRate100:]),
	}
	// Trailing bytes (len(buf) > HeaderSize+PayloadLen, e.g. UDP padding) are
	// tolerated: payload is exactly the declared region.
	payload := buf[HeaderSize : HeaderSize+int(payloadLen)]
	return h, payload, nil
}
