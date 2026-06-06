//go:build linux

package audio

// ALSA PCM uapi ABI, hand-transcribed from <sound/asound.h> (SNDRV_PCM_VERSION
// 2.0.x, the version shipped by current Raspberry Pi OS arm64 / mainline). This
// is the same kernel character-device ABI that libasound itself sits on; we talk
// to it directly so the binary stays pure-Go and needs no C toolchain (D24, 06
// §1.3). The struct layouts MUST match the kernel byte-for-byte — the
// sink_alsa_linux_test.go size/offset asserts guard against a layout regression.
//
// ⚠️ Verification spike (06 §1.3): the struct layouts and ioctl numbers are
// pinned to one uapi version and are only PROVEN on real hardware. Start() gates
// on SNDRV_PCM_IOCTL_PVERSION (major must match) and fails closed otherwise so a
// mismatched kernel degrades to the coarse exec tier rather than corrupting
// memory.

import "unsafe"

// SNDRV_PCM_VERSION components we target. The kernel returns its protocol
// version from SNDRV_PCM_IOCTL_PVERSION; we require the MAJOR to match (the
// struct layout is stable within a major version).
const (
	sndrvPCMVersionMajor = 2
	sndrvPCMVersion      = (sndrvPCMVersionMajor << 16) | (0 << 8) | 16 // 2.0.16
)

// ioctl direction bits and field widths (asm-generic/ioctl.h).
const (
	iocNrbits   = 8
	iocTypebits = 8
	iocSizebits = 14
	iocDirbits  = 2

	iocNrshift   = 0
	iocTypeshift = iocNrshift + iocNrbits
	iocSizeshift = iocTypeshift + iocTypebits
	iocDirshift  = iocSizeshift + iocSizebits

	iocNone  = 0
	iocWrite = 1
	iocRead  = 2
)

// ioc encodes an ioctl request number (the _IOC macro).
func ioc(dir, typ, nr, size uintptr) uintptr {
	return (dir << iocDirshift) | (typ << iocTypeshift) | (nr << iocNrshift) | (size << iocSizeshift)
}

func iow(typ, nr, size uintptr) uintptr  { return ioc(iocWrite, typ, nr, size) }
func ior(typ, nr, size uintptr) uintptr  { return ioc(iocRead, typ, nr, size) }
func iowr(typ, nr, size uintptr) uintptr { return ioc(iocWrite|iocRead, typ, nr, size) }

// 'A' is the ALSA PCM ioctl type byte.
const sndrvPCMIoctlType = 'A'

// PCM ioctl request numbers (sound/asound.h). Sizes are taken from the structs
// below so the encoding matches the kernel's _IOWR/_IOR/_IOW expansions exactly.
var (
	SNDRV_PCM_IOCTL_PVERSION   = ior(sndrvPCMIoctlType, 0x00, unsafe.Sizeof(int32(0)))
	SNDRV_PCM_IOCTL_HW_REFINE  = iowr(sndrvPCMIoctlType, 0x10, unsafe.Sizeof(sndPCMHwParams{}))
	SNDRV_PCM_IOCTL_HW_PARAMS  = iowr(sndrvPCMIoctlType, 0x11, unsafe.Sizeof(sndPCMHwParams{}))
	SNDRV_PCM_IOCTL_SW_PARAMS  = iowr(sndrvPCMIoctlType, 0x13, unsafe.Sizeof(sndPCMSwParams{}))
	SNDRV_PCM_IOCTL_PREPARE    = ioc(iocNone, sndrvPCMIoctlType, 0x40, 0)
	SNDRV_PCM_IOCTL_RESET      = ioc(iocNone, sndrvPCMIoctlType, 0x41, 0)
	SNDRV_PCM_IOCTL_START      = ioc(iocNone, sndrvPCMIoctlType, 0x42, 0)
	SNDRV_PCM_IOCTL_DRAIN      = ioc(iocNone, sndrvPCMIoctlType, 0x44, 0)
	SNDRV_PCM_IOCTL_DROP       = ioc(iocNone, sndrvPCMIoctlType, 0x43, 0)
	SNDRV_PCM_IOCTL_PAUSE      = iow(sndrvPCMIoctlType, 0x45, unsafe.Sizeof(int32(0)))
	SNDRV_PCM_IOCTL_DELAY      = ior(sndrvPCMIoctlType, 0x21, unsafe.Sizeof(sndPCMSframes(0)))
	SNDRV_PCM_IOCTL_RESUME     = ioc(iocNone, sndrvPCMIoctlType, 0x47, 0)
	SNDRV_PCM_IOCTL_WRITEI_FRAMES = iow(sndrvPCMIoctlType, 0x50, unsafe.Sizeof(sndXferi{}))
)

// snd_pcm_uframes_t / snd_pcm_sframes_t are unsigned/signed long.
type (
	sndPCMUframes uint64 // unsigned long on the arm64/amd64 targets
	sndPCMSframes int64  // signed long
)

// PCM hardware param indices (sound/asound.h). The refine works on a fixed-size
// array of masks (params_masks) followed by intervals (params_intervals).
const (
	// Masks: SNDRV_PCM_HW_PARAM_ACCESS .. SNDRV_PCM_HW_PARAM_SUBFORMAT.
	sndrvPCMHwParamAccess    = 0
	sndrvPCMHwParamFormat    = 1
	sndrvPCMHwParamSubformat = 2
	sndrvPCMHwParamFirstMask = sndrvPCMHwParamAccess
	sndrvPCMHwParamLastMask  = sndrvPCMHwParamSubformat

	// Intervals: SNDRV_PCM_HW_PARAM_SAMPLE_BITS .. SNDRV_PCM_HW_PARAM_TICK_TIME.
	sndrvPCMHwParamSampleBits     = 8
	sndrvPCMHwParamFrameBits      = 9
	sndrvPCMHwParamChannels       = 10
	sndrvPCMHwParamRate           = 11
	sndrvPCMHwParamPeriodTime     = 12
	sndrvPCMHwParamPeriodSize     = 13
	sndrvPCMHwParamPeriodBytes    = 14
	sndrvPCMHwParamPeriods        = 15
	sndrvPCMHwParamBufferTime     = 16
	sndrvPCMHwParamBufferSize     = 17
	sndrvPCMHwParamBufferBytes    = 18
	sndrvPCMHwParamTickTime       = 19
	sndrvPCMHwParamFirstInterval  = sndrvPCMHwParamSampleBits
	sndrvPCMHwParamLastInterval   = sndrvPCMHwParamTickTime

	maskCount     = sndrvPCMHwParamLastMask - sndrvPCMHwParamFirstMask + 1     // 3
	intervalCount = sndrvPCMHwParamLastInterval - sndrvPCMHwParamFirstInterval + 1 // 12
)

// Access / format enum values (sound/asound.h).
const (
	sndrvPCMAccessRWInterleaved = 3 // SNDRV_PCM_ACCESS_RW_INTERLEAVED
	sndrvPCMFormatFloatLE       = 14 // SNDRV_PCM_FORMAT_FLOAT_LE
	sndrvPCMStreamPlayback      = 0
)

// SW params boundary / start threshold tunables.
const (
	sndrvPCMTstampNone = 0
)

// sndMask is SNDRV_MASK: a fixed bitmap of SNDRV_MASK_MAX (256) bits.
const sndrvMaskMax = 256

type sndMask struct {
	Bits [sndrvMaskMax / 32]uint32 // 8 x u32
}

func (m *sndMask) none() { m.Bits = [sndrvMaskMax / 32]uint32{} }
func (m *sndMask) set(v uint) {
	m.Bits[v>>5] |= 1 << (v & 31)
}

// sndInterval is SNDRV_INTERVAL: [min,max] with open/integer/empty flags. The
// flags field packs three bits: openmin(0), openmax(1), integer(2), empty(3).
type sndInterval struct {
	Min   uint32
	Max   uint32
	Flags uint32 // bit0 openmin, bit1 openmax, bit2 integer, bit3 empty
}

const (
	intervalOpenMin = 1 << 0
	intervalOpenMax = 1 << 1
	intervalInteger = 1 << 2
	intervalEmpty   = 1 << 3
)

// setInteger pins the interval to a single integer value [v,v].
func (iv *sndInterval) setInteger(v uint32) {
	iv.Min = v
	iv.Max = v
	iv.Flags = intervalInteger
}

// setRange pins an inclusive [min,max] integer interval.
func (iv *sndInterval) setRange(min, max uint32) {
	iv.Min = min
	iv.Max = max
	iv.Flags = intervalInteger
}

// sndPCMHwParams mirrors struct snd_pcm_hw_params (sound/asound.h). The trailing
// reserved padding makes the struct match the kernel size exactly (the ioctl
// number encodes sizeof, so this must be right).
type sndPCMHwParams struct {
	Flags     uint32
	Masks     [maskCount]sndMask     // params[FIRST_MASK..LAST_MASK]
	Mres      [5]sndMask             // reserved masks (SNDRV_PCM_HW_PARAM_LAST_MASK..) padding to 8 total
	Intervals [intervalCount]sndInterval
	Ires      [9]sndInterval // reserved intervals padding to 21 total
	Rmask     uint32
	Cmask     uint32
	Info      uint32
	MsBits    uint32
	RateNum   uint32
	RateDen   uint32
	FifoSize  sndPCMUframes
	Sync      [16]byte // R: synchronization ID
	Reserved  [48]byte // reserved for future
}

// sndPCMSwParams mirrors struct snd_pcm_sw_params (sound/asound.h).
type sndPCMSwParams struct {
	TstampMode       int32
	PeriodStep       uint32
	SleepMin         uint32
	AvailMin         sndPCMUframes
	XferAlign        sndPCMUframes
	StartThreshold   sndPCMUframes
	StopThreshold    sndPCMUframes
	SilenceThreshold sndPCMUframes
	SilenceSize      sndPCMUframes
	Boundary         sndPCMUframes
	Proto            uint32
	TstampType       uint32
	Reserved         [56]byte
}

// sndXferi mirrors struct snd_xferi (sound/asound.h): the WRITEI_FRAMES argument.
// result is the kernel's returned signed frame count; buf is a pointer to the
// interleaved sample buffer; frames is the request count.
type sndXferi struct {
	Result sndPCMSframes
	Buf    uintptr // void *buf
	Frames sndPCMUframes
}

// hwParamMaskAt returns a pointer to the mask for param p (p must be a mask index).
func (h *sndPCMHwParams) mask(p int) *sndMask {
	return &h.Masks[p-sndrvPCMHwParamFirstMask]
}

// hwParamIntervalAt returns a pointer to the interval for param p (p must be an
// interval index).
func (h *sndPCMHwParams) interval(p int) *sndInterval {
	return &h.Intervals[p-sndrvPCMHwParamFirstInterval]
}

// rmaskAll requests the kernel to refine every param: set rmask to all-ones and
// every mask/interval to its widest range so HW_REFINE narrows them.
func (h *sndPCMHwParams) setAllRanges() {
	h.Rmask = ^uint32(0)
	for i := range h.Masks {
		for j := range h.Masks[i].Bits {
			h.Masks[i].Bits[j] = ^uint32(0)
		}
	}
	for i := range h.Intervals {
		h.Intervals[i].Min = 0
		h.Intervals[i].Max = ^uint32(0)
		h.Intervals[i].Flags = 0
	}
}
