//go:build linux

package audio

// libasound runtime binding via purego/dlopen (the opus_binding.go pattern): a
// SHARED-ACCESS ALSA sink that opens devices through the userspace plugin layer
// ("default", "plughw:…", dmix, the PipeWire/Pulse ALSA plugins), so a desktop
// box whose card is owned by a sound server can still render precisely — the
// raw kernel-ioctl backend (sink_alsa.go) requires exclusive hw access. NO cgo
// and NO C toolchain at build time; purego asks the dynamic linker at RUNTIME
// and fails SOFT: a host without libasound simply reports this backend
// unusable and the registry falls back down the preference chain (06 §1.1).
//
// Precise: snd_pcm_delay reads the live device delay, so the drift loop gets a
// real figure (Delay ok=true), same tier as the raw hw backend.

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// alsaLibCandidates are the sonames tried in order.
var alsaLibCandidates = []string{"libasound.so.2", "libasound.so"}

// libasound constants (sound/asound.h / alsa-lib pcm.h — stable ABI).
const (
	alsalibStreamPlayback   = 0 // SND_PCM_STREAM_PLAYBACK
	alsalibFormatS16LE      = 2 // SND_PCM_FORMAT_S16_LE
	alsalibAccessRWInterlvd = 3 // SND_PCM_ACCESS_RW_INTERLEAVED

	// alsalibLatencyUs is the device buffer target handed to
	// snd_pcm_set_params: ~100 ms keeps a Pi-class box underrun-safe while the
	// renderer's own LeadMs ring (300 ms) does the real buffering.
	alsalibLatencyUs = 100_000
)

// alsaLib is the resolved symbol set (loaded once, fail-soft).
type alsaLib struct {
	handle uintptr
	ok     bool

	open      uintptr // snd_pcm_open(**pcm, name, stream, mode) -> int
	setParams uintptr // snd_pcm_set_params(pcm, fmt, access, ch, rate, resample, latencyUs) -> int
	writei    uintptr // snd_pcm_writei(pcm, buf, frames) -> sframes
	recover_  uintptr // snd_pcm_recover(pcm, err, silent) -> int
	delay     uintptr // snd_pcm_delay(pcm, *sframes) -> int
	closePCM  uintptr // snd_pcm_close(pcm) -> int
}

var (
	alsalibOnce sync.Once
	alsalib     alsaLib
)

func loadAlsaLib() {
	var handle uintptr
	for _, name := range alsaLibCandidates {
		h, err := purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err == nil && h != 0 {
			handle = h
			break
		}
	}
	if handle == 0 {
		return
	}
	sym := func(name string) (uintptr, bool) {
		p, e := purego.Dlsym(handle, name)
		return p, e == nil && p != 0
	}
	l := alsaLib{handle: handle}
	var ok [6]bool
	l.open, ok[0] = sym("snd_pcm_open")
	l.setParams, ok[1] = sym("snd_pcm_set_params")
	l.writei, ok[2] = sym("snd_pcm_writei")
	l.recover_, ok[3] = sym("snd_pcm_recover")
	l.delay, ok[4] = sym("snd_pcm_delay")
	l.closePCM, ok[5] = sym("snd_pcm_close")
	l.ok = ok[0] && ok[1] && ok[2] && ok[3] && ok[4] && ok[5]
	alsalib = l
}

// alsaLibAvailable reports whether libasound loaded with every needed symbol.
func alsaLibAvailable() bool {
	alsalibOnce.Do(loadAlsaLib)
	return alsalib.ok
}

// alsalibName resolves the device string for libasound: "" means the system
// "default" PCM (the shared path through dmix/PipeWire/Pulse plugins).
func alsalibName(device string) string {
	if device == "" {
		return "default"
	}
	return device
}

// alsalibOpenPCM opens a playback PCM handle by name.
func alsalibOpenPCM(name string) (uintptr, error) {
	cname := append([]byte(name), 0)
	var pcm uintptr
	r, _, _ := purego.SyscallN(alsalib.open,
		uintptr(unsafe.Pointer(&pcm)),
		uintptr(unsafe.Pointer(&cname[0])),
		uintptr(alsalibStreamPlayback), 0)
	if int32(r) < 0 || pcm == 0 {
		return 0, fmt.Errorf("alsalib: snd_pcm_open(%q) = %d", name, int32(r))
	}
	return pcm, nil
}

func alsalibClose(pcm uintptr) {
	_, _, _ = purego.SyscallN(alsalib.closePCM, pcm)
}

// probeAlsaLib reports whether the shared libasound path actually works for
// device on this host: the library loads AND the PCM opens (06 §1.1 — presence
// is necessary but not sufficient). The handle is closed immediately; opening a
// shared PCM ("default"/plug) does not disturb a concurrent stream.
func probeAlsaLib(device string) bool {
	if !alsaLibAvailable() {
		return false
	}
	pcm, err := alsalibOpenPCM(alsalibName(device))
	if err != nil {
		return false
	}
	alsalibClose(pcm)
	return true
}

// alsaLibSink is the shared-access AudioSink over libasound.
type alsaLibSink struct {
	device   string
	pcm      uintptr
	channels int
	buf      []int16 // reused S16 conversion buffer
}

// newAlsaLibSink constructs the sink (the PCM opens at Start, like the other
// backends — Open must not hold a device the renderer never starts).
func newAlsaLibSink(device string) (AudioSink, error) {
	if !alsaLibAvailable() {
		return nil, fmt.Errorf("alsalib: libasound unavailable")
	}
	return &alsaLibSink{device: device}, nil
}

// Start opens the PCM and commits rate/channels via snd_pcm_set_params (which
// also negotiates the plugin chain — resampling/mixing as the device needs).
func (s *alsaLibSink) Start(rate, channels int) error {
	if s.pcm != 0 {
		return fmt.Errorf("alsalib: already started")
	}
	pcm, err := alsalibOpenPCM(alsalibName(s.device))
	if err != nil {
		return err
	}
	r, _, _ := purego.SyscallN(alsalib.setParams, pcm,
		uintptr(alsalibFormatS16LE), uintptr(alsalibAccessRWInterlvd),
		uintptr(channels), uintptr(rate), 1 /* soft_resample */, uintptr(alsalibLatencyUs))
	if int32(r) < 0 {
		alsalibClose(pcm)
		return fmt.Errorf("alsalib: snd_pcm_set_params(rate=%d ch=%d) = %d", rate, channels, int32(r))
	}
	s.pcm = pcm
	s.channels = channels
	return nil
}

// Write converts interleaved float32 to S16_LE and feeds snd_pcm_writei, which
// blocks for backpressure (the playout pacing). On -EPIPE it recovers the PCM
// and reports ErrUnderrun so the renderer reseeks (06 §6.4).
func (s *alsaLibSink) Write(frames []float32) (int, error) {
	if s.pcm == 0 {
		return 0, fmt.Errorf("alsalib: not started")
	}
	if len(frames) == 0 || s.channels == 0 {
		return 0, nil
	}
	if cap(s.buf) < len(frames) {
		s.buf = make([]int16, len(frames))
	}
	buf := s.buf[:len(frames)]
	for i, v := range frames {
		switch {
		case v > 1:
			v = 1
		case v < -1:
			v = -1
		}
		buf[i] = int16(v * 32767)
	}

	written := 0
	total := len(frames) / s.channels // frames, not samples
	for written < total {
		off := written * s.channels
		r, _, _ := purego.SyscallN(alsalib.writei, s.pcm,
			uintptr(unsafe.Pointer(&buf[off])), uintptr(total-written))
		n := int64(r)
		if n < 0 {
			// Recover EPIPE/ESTRPIPE in place; report the underrun upward so the
			// renderer treats it as a gross error and reseeks.
			rr, _, _ := purego.SyscallN(alsalib.recover_, s.pcm, uintptr(r), 1)
			if int32(rr) < 0 {
				return written * s.channels, fmt.Errorf("alsalib: writei = %d (recover = %d)", int32(n), int32(rr))
			}
			return written * s.channels, ErrUnderrun
		}
		written += int(n)
	}
	return written * s.channels, nil
}

// Delay reports the device's outstanding frames (precise — snd_pcm_delay).
func (s *alsaLibSink) Delay() (int, bool) {
	if s.pcm == 0 {
		return 0, false
	}
	var d int64
	r, _, _ := purego.SyscallN(alsalib.delay, s.pcm, uintptr(unsafe.Pointer(&d)))
	if int32(r) < 0 || d < 0 {
		return 0, false
	}
	return int(d), true
}

func (s *alsaLibSink) Close() error {
	if s.pcm != 0 {
		alsalibClose(s.pcm)
		s.pcm = 0
	}
	return nil
}
