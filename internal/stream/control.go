package stream

import (
	"encoding/binary"
	"errors"
	"net/netip"
)

// control.go is the v2 control-plane payload codec (PLAYER.md §6, D49–D58):
// the master→playback commands carried on the playback node's CONTROL_PORT and the
// STATUS telemetry the playback node sends back. Each payload follows the common
// 24-byte Header (wire.go); on control packets the Header's Gen/Seq/PTS are unused
// (set to 0) — only Type and PayloadLen are load-bearing. All multi-byte fields are
// big-endian, matching the header. IPv4-only in v2 (LAN multiroom); IPv6 would be a
// new ATTACH type, not a layout change.

// Codec is the compact on-wire codec selector for ATTACH (PLAYER §6.1),
// mirroring contracts.GroupSettings.Codec ("pcm" | "opus").
type Codec uint8

const (
	CodecPCM  Codec = 0
	CodecOpus Codec = 1
)

func (c Codec) String() string {
	if c == CodecOpus {
		return "opus"
	}
	return "pcm"
}

// ParseCodec maps the group-setting string to a Codec. Unknown values map to PCM
// (the universally-decodable floor) so a malformed record never wedges playout.
func ParseCodec(s string) Codec {
	if s == "opus" || s == "OPUS" {
		return CodecOpus
	}
	return CodecPCM
}

// Control-payload sizes (bytes), exact on-wire lengths.
const (
	AttachLen   = 16  // §6.1
	SetVolLen   = 2   // §6.2
	SetDelayLen = 2   // §6.2
	SetCapLen   = 2   // §6.2
	SetEqLen    = 2   // D65: master-driven cross-room equalization delay
	StatusLen   = 103 // §6.3 (87 + 8 SamplesInjected + 8 SamplesDropped)
)

// errBadControl is returned when a control payload is too short / malformed.
var errBadControl = errors.New("stream: malformed control payload")

// AttachPayload tells a playback node which stream to join (PLAYER §6.1).
// Source/Clock are the master's SOURCE_PORT/STREAM_PORT endpoints (usually the same
// IP). Both MUST be IPv4 (v2).
type AttachPayload struct {
	Source    netip.AddrPort // HELLO/BYE/RESTART + TCP dial target
	Clock     netip.AddrPort // CLOCK_REQ target / CLOCK_RSP source
	Codec     Codec
	Transport Transport
	BufferMs  uint16
}

// AppendTo appends the 16-byte ATTACH payload to dst and returns the grown slice.
// Non-IPv4 endpoints are encoded as 0.0.0.0 (the receiver then treats them as
// unset); callers should pass Is4 endpoints.
func (a AttachPayload) AppendTo(dst []byte) []byte {
	var b [AttachLen]byte
	put4(b[0:4], a.Source.Addr())
	binary.BigEndian.PutUint16(b[4:6], a.Source.Port())
	put4(b[6:10], a.Clock.Addr())
	binary.BigEndian.PutUint16(b[10:12], a.Clock.Port())
	b[12] = byte(a.Codec)
	b[13] = byte(a.Transport)
	binary.BigEndian.PutUint16(b[14:16], a.BufferMs)
	return append(dst, b[:]...)
}

// DecodeAttach parses a 16-byte ATTACH payload.
func DecodeAttach(p []byte) (AttachPayload, error) {
	if len(p) < AttachLen {
		return AttachPayload{}, errBadControl
	}
	return AttachPayload{
		Source:    netip.AddrPortFrom(addr4(p[0:4]), binary.BigEndian.Uint16(p[4:6])),
		Clock:     netip.AddrPortFrom(addr4(p[6:10]), binary.BigEndian.Uint16(p[10:12])),
		Codec:     Codec(p[12]),
		Transport: Transport(p[13]),
		BufferMs:  binary.BigEndian.Uint16(p[14:16]),
	}, nil
}

// SetVolPayload sets software/hardware volume + mute (PLAYER §6.2).
type SetVolPayload struct {
	VolumePct uint8 // 0..100
	Mute      bool
}

func (v SetVolPayload) AppendTo(dst []byte) []byte {
	var flags byte
	if v.Mute {
		flags = 0x01
	}
	return append(dst, v.VolumePct, flags)
}

// DecodeSetVol parses a 2-byte SETVOL payload. VolumePct is clamped to 0..100.
func DecodeSetVol(p []byte) (SetVolPayload, error) {
	if len(p) < SetVolLen {
		return SetVolPayload{}, errBadControl
	}
	pct := p[0]
	if pct > 100 {
		pct = 100
	}
	return SetVolPayload{VolumePct: pct, Mute: p[1]&0x01 != 0}, nil
}

// SetDelayPayload sets the output-delay calibration in milliseconds, signed
// (positive = device chain plays late → emit earlier; PLAYER §6.2).
type SetDelayPayload struct {
	DelayMs int16
}

func (d SetDelayPayload) AppendTo(dst []byte) []byte {
	var b [SetDelayLen]byte
	binary.BigEndian.PutUint16(b[:], uint16(d.DelayMs))
	return append(dst, b[:]...)
}

func DecodeSetDelay(p []byte) (SetDelayPayload, error) {
	if len(p) < SetDelayLen {
		return SetDelayPayload{}, errBadControl
	}
	return SetDelayPayload{DelayMs: int16(binary.BigEndian.Uint16(p[0:2]))}, nil
}

// SetCapPayload toggles a runtime capability (PLAYER §6.2). CapID enumerates
// the toggleable capabilities; unknown ids MUST be ignored by the receiver.
type SetCapPayload struct {
	CapID uint8
	On    bool
}

func (c SetCapPayload) AppendTo(dst []byte) []byte {
	var on byte
	if c.On {
		on = 0x01
	}
	return append(dst, c.CapID, on)
}

func DecodeSetCap(p []byte) (SetCapPayload, error) {
	if len(p) < SetCapLen {
		return SetCapPayload{}, errBadControl
	}
	return SetCapPayload{CapID: p[0], On: p[1]&0x01 != 0}, nil
}

// SetEqualizePayload sets the master-computed cross-room equalization delay in
// milliseconds, UNSIGNED (always ≥0: it only ever DELAYS a faster room to match the
// slowest, never advances). It is a SEPARATE knob from SetDelayPayload (D36 acoustic
// offset, node-owned): the sink sums both, so the master can equalize device
// buffering without clobbering the node's own room calibration (D65).
type SetEqualizePayload struct {
	DelayMs uint16
}

func (e SetEqualizePayload) AppendTo(dst []byte) []byte {
	var b [SetEqLen]byte
	binary.BigEndian.PutUint16(b[:], e.DelayMs)
	return append(dst, b[:]...)
}

func DecodeSetEqualize(p []byte) (SetEqualizePayload, error) {
	if len(p) < SetEqLen {
		return SetEqualizePayload{}, errBadControl
	}
	return SetEqualizePayload{DelayMs: binary.BigEndian.Uint16(p[0:2])}, nil
}

// Status flag bits (StatusPayload.Flags).
const (
	StatusFlagSynced     = 0x01
	StatusFlagPlaying    = 0x02
	StatusFlagCalibrated = 0x04 // servo setpoint captured: DeviceDelayNs−PhaseErrNs is a stable constant (D65)
)

// StatusPayload is the playback node's telemetry to its master (PLAYER §6.3),
// modeled on SlimProto STAT: enough for the master to spot a starving/drifting room
// and feed the per-room health UI. NodeID is the 16-byte node id so the master can
// correlate to the mDNS advert.
type StatusPayload struct {
	NodeID        [16]byte
	Synced        bool
	Playing       bool
	Buffered      uint16 // jitter-buffer depth, frames
	LastSeq       uint64 // last seq written to the output
	OffsetNs      int64  // clock offset estimate (master_ns − local_ns)
	RTTNs         int64  // smallest RTT in the clock window
	RatePPMx1000  int32  // servo rate correction, ppm×1000
	Played        uint64
	Silence       uint64
	Late          uint64
	DeviceDelayNs int64 // measured output (device) latency, ns; 0 if the backend can't report it. The master diffs this across rooms to see inter-node skew (D63 telemetry).
	PhaseErrNs    int64 // playout phase error vs the smoothed model, ns (D64 telemetry)
	Calibrated    bool  // servo setpoint captured → DeviceDelayNs−PhaseErrNs is the stable per-room device-queue depth (D65; flag, not a payload field)
	// Grounded resample accounting: cumulative samples the rate-servo actually
	// duplicated into / dropped from the output (per-channel sample units) — the
	// realized correction at the DAC, not the commanded RatePPM.
	SamplesInjected uint64
	SamplesDropped  uint64
}

// AppendTo appends the 103-byte STATUS payload to dst (offsets per §6.3).
func (s StatusPayload) AppendTo(dst []byte) []byte {
	var b [StatusLen]byte
	copy(b[0:16], s.NodeID[:])
	var flags byte
	if s.Synced {
		flags |= StatusFlagSynced
	}
	if s.Playing {
		flags |= StatusFlagPlaying
	}
	if s.Calibrated {
		flags |= StatusFlagCalibrated
	}
	b[16] = flags
	binary.BigEndian.PutUint16(b[17:19], s.Buffered)
	binary.BigEndian.PutUint64(b[19:27], s.LastSeq)
	binary.BigEndian.PutUint64(b[27:35], uint64(s.OffsetNs))
	binary.BigEndian.PutUint64(b[35:43], uint64(s.RTTNs))
	binary.BigEndian.PutUint32(b[43:47], uint32(s.RatePPMx1000))
	binary.BigEndian.PutUint64(b[47:55], s.Played)
	binary.BigEndian.PutUint64(b[55:63], s.Silence)
	binary.BigEndian.PutUint64(b[63:71], s.Late)
	binary.BigEndian.PutUint64(b[71:79], uint64(s.DeviceDelayNs))
	binary.BigEndian.PutUint64(b[79:87], uint64(s.PhaseErrNs))
	binary.BigEndian.PutUint64(b[87:95], s.SamplesInjected)
	binary.BigEndian.PutUint64(b[95:103], s.SamplesDropped)
	return append(dst, b[:]...)
}

// DecodeStatus parses a 103-byte STATUS payload.
func DecodeStatus(p []byte) (StatusPayload, error) {
	if len(p) < StatusLen {
		return StatusPayload{}, errBadControl
	}
	var s StatusPayload
	copy(s.NodeID[:], p[0:16])
	s.Synced = p[16]&StatusFlagSynced != 0
	s.Playing = p[16]&StatusFlagPlaying != 0
	s.Calibrated = p[16]&StatusFlagCalibrated != 0
	s.Buffered = binary.BigEndian.Uint16(p[17:19])
	s.LastSeq = binary.BigEndian.Uint64(p[19:27])
	s.OffsetNs = int64(binary.BigEndian.Uint64(p[27:35]))
	s.RTTNs = int64(binary.BigEndian.Uint64(p[35:43]))
	s.RatePPMx1000 = int32(binary.BigEndian.Uint32(p[43:47]))
	s.Played = binary.BigEndian.Uint64(p[47:55])
	s.Silence = binary.BigEndian.Uint64(p[55:63])
	s.Late = binary.BigEndian.Uint64(p[63:71])
	s.DeviceDelayNs = int64(binary.BigEndian.Uint64(p[71:79]))
	s.PhaseErrNs = int64(binary.BigEndian.Uint64(p[79:87]))
	s.SamplesInjected = binary.BigEndian.Uint64(p[87:95])
	s.SamplesDropped = binary.BigEndian.Uint64(p[95:103])
	return s, nil
}

// put4 writes a's IPv4 bytes into dst[:4]; a non-IPv4 (or zero) address writes
// 0.0.0.0. dst must be >= 4 bytes.
func put4(dst []byte, a netip.Addr) {
	if a.Is4() {
		v := a.As4()
		copy(dst[:4], v[:])
		return
	}
	if a.Is4In6() {
		v := a.Unmap().As4()
		copy(dst[:4], v[:])
		return
	}
	dst[0], dst[1], dst[2], dst[3] = 0, 0, 0, 0
}

// addr4 reads a 4-byte IPv4 address from p[:4].
func addr4(p []byte) netip.Addr {
	return netip.AddrFrom4([4]byte{p[0], p[1], p[2], p[3]})
}
