//go:build linux

package audio

import (
	"errors"
	"math"
	"os"
	"testing"
	"unsafe"
)

// TestALSAUAPILayout pins the hand-transcribed uapi struct layouts to the kernel
// ABI (sound/asound.h, SNDRV_PCM_VERSION 2.0.x). The constants below are the
// `sizeof`/`offsetof` figures emitted by a C program compiled against the
// system headers (see the P4.6 report). A layout regression is caught here
// without any hardware (06 §1.3 verification spike: layout half is unit-testable).
func TestALSAUAPILayout(t *testing.T) {
	type szcheck struct {
		name string
		got  uintptr
		want uintptr
	}
	checks := []szcheck{
		{"snd_mask", unsafe.Sizeof(sndMask{}), 32},
		{"snd_interval", unsafe.Sizeof(sndInterval{}), 12},
		{"snd_pcm_hw_params", unsafe.Sizeof(sndPCMHwParams{}), 608},
		{"snd_pcm_sw_params", unsafe.Sizeof(sndPCMSwParams{}), 136},
		{"snd_xferi", unsafe.Sizeof(sndXferi{}), 24},
		{"snd_pcm_sframes_t", unsafe.Sizeof(sndPCMSframes(0)), 8},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("sizeof(%s) = %d, want %d", c.name, c.got, c.want)
		}
	}

	offs := []szcheck{
		{"hw_params.intervals", unsafe.Offsetof(sndPCMHwParams{}.Intervals), 260},
		{"hw_params.rmask", unsafe.Offsetof(sndPCMHwParams{}.Rmask), 512},
		{"hw_params.fifo_size", unsafe.Offsetof(sndPCMHwParams{}.FifoSize), 536},
		{"sw_params.avail_min", unsafe.Offsetof(sndPCMSwParams{}.AvailMin), 16},
		{"sw_params.boundary", unsafe.Offsetof(sndPCMSwParams{}.Boundary), 64},
		{"sw_params.proto", unsafe.Offsetof(sndPCMSwParams{}.Proto), 72},
		{"xferi.buf", unsafe.Offsetof(sndXferi{}.Buf), 8},
		{"xferi.frames", unsafe.Offsetof(sndXferi{}.Frames), 16},
	}
	for _, c := range offs {
		if c.got != c.want {
			t.Errorf("offsetof(%s) = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// TestALSAIoctlNumbers pins the ioctl request encodings to the kernel _IOC
// expansions from sound/asound.h. Computed independently of the structs so a
// size/typo regression in the _IOWR encoding is caught without hardware.
func TestALSAIoctlNumbers(t *testing.T) {
	// _IOC(dir,type,nr,size): dir<<30 | size<<16 | type<<8 | nr, on the
	// asm-generic layout used by arm64/amd64.
	ioc := func(dir, typ, nr, size uint32) uintptr {
		return uintptr(dir<<30 | size<<16 | typ<<8 | nr)
	}
	const A = 'A'
	tests := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"PVERSION", SNDRV_PCM_IOCTL_PVERSION, ioc(2, A, 0x00, 4)},
		{"HW_REFINE", SNDRV_PCM_IOCTL_HW_REFINE, ioc(3, A, 0x10, 608)},
		{"HW_PARAMS", SNDRV_PCM_IOCTL_HW_PARAMS, ioc(3, A, 0x11, 608)},
		{"SW_PARAMS", SNDRV_PCM_IOCTL_SW_PARAMS, ioc(3, A, 0x13, 136)},
		{"PREPARE", SNDRV_PCM_IOCTL_PREPARE, ioc(0, A, 0x40, 0)},
		{"DRAIN", SNDRV_PCM_IOCTL_DRAIN, ioc(0, A, 0x44, 0)},
		{"RESUME", SNDRV_PCM_IOCTL_RESUME, ioc(0, A, 0x47, 0)},
		{"DELAY", SNDRV_PCM_IOCTL_DELAY, ioc(2, A, 0x21, 8)},
		{"WRITEI_FRAMES", SNDRV_PCM_IOCTL_WRITEI_FRAMES, ioc(1, A, 0x50, 24)},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s ioctl = %#x, want %#x", tt.name, tt.got, tt.want)
		}
	}
}

// TestALSAMaskInterval exercises the mask/interval helpers against known byte
// patterns (catches a shift bug without hardware).
func TestALSAMaskInterval(t *testing.T) {
	var m sndMask
	m.none()
	m.set(sndrvPCMFormatFloatLE) // 14 -> word 0, bit 14
	if m.Bits[0] != (1 << 14) {
		t.Errorf("mask.set(14) Bits[0] = %#x, want %#x", m.Bits[0], uint32(1<<14))
	}
	m.set(sndrvPCMAccessRWInterleaved) // 3 -> word 0, bit 3
	if m.Bits[0] != (1<<14 | 1<<3) {
		t.Errorf("mask after set(3) = %#x", m.Bits[0])
	}
	// A bit past 31 lands in the next word.
	m.none()
	m.set(40) // word 1, bit 8
	if m.Bits[1] != (1 << 8) {
		t.Errorf("mask.set(40) Bits[1] = %#x, want %#x", m.Bits[1], uint32(1<<8))
	}

	var iv sndInterval
	iv.setInteger(48000)
	if iv.Min != 48000 || iv.Max != 48000 || iv.Flags != intervalInteger {
		t.Errorf("setInteger(48000) = %+v", iv)
	}
	iv.setRange(100, 200)
	if iv.Min != 100 || iv.Max != 200 || iv.Flags != intervalInteger {
		t.Errorf("setRange(100,200) = %+v", iv)
	}
}

// TestALSAHardware is the real-hardware acceptance gate (06 §1.3 / A.13 P4). It
// is SKIPPED unless ENSEMBLE_ALSA_HW=<device> is set, so CI stays toolchain-free.
// On a Pi+DAC: Start(48000,2) succeeds; a 1s 1kHz tone is fully consumed; Delay()
// reports ok=true with a plausible (<~200ms) figure; Close drains cleanly.
func TestALSAHardware(t *testing.T) {
	device := os.Getenv("ENSEMBLE_ALSA_HW")
	if device == "" {
		t.Skip("set ENSEMBLE_ALSA_HW=<device> (e.g. hw:0,0) to run the ALSA hardware acceptance test")
	}

	s, err := newALSASink(device)
	if err != nil {
		t.Fatalf("newALSASink: %v", err)
	}
	const (
		rate     = 48000
		channels = 2
	)
	if err := s.Start(rate, channels); err != nil {
		t.Fatalf("Start(%d,%d): %v", rate, channels, err)
	}
	defer s.Close()

	// 1 s of a 1 kHz sine, interleaved stereo.
	tone := make([]float32, rate*channels)
	for i := 0; i < rate; i++ {
		v := float32(0.25 * math.Sin(2*math.Pi*1000*float64(i)/float64(rate)))
		tone[i*2] = v
		tone[i*2+1] = v
	}
	n, err := s.Write(tone)
	if err != nil && !errors.Is(err, errUnderrun) {
		t.Fatalf("Write tone: %v", err)
	}
	if err == nil && n != len(tone) {
		t.Fatalf("Write consumed %d samples, want %d", n, len(tone))
	}

	if d, ok := s.Delay(); !ok {
		t.Errorf("Delay() ok=false on precise backend; want a kernel figure")
	} else if d < 0 || d > rate/4 { // <250ms outstanding is plausible for a ~100ms buffer
		t.Errorf("Delay() = %d frames, want a plausible (<%d) figure", d, rate/4)
	}

	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Close is idempotent.
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
