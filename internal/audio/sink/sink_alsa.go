//go:build linux

package audio

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// alsaSink is the precise AudioSink (06 §1.3, verification spike): it drives the
// kernel PCM character device /dev/snd/pcmC<card>D<dev>p DIRECTLY through ioctls
// via golang.org/x/sys/unix — no libasound, no dlopen, no cgo. Its Delay() reads
// the kernel hardware pointer, giving the drift loop sub-ms control.
//
// All ioctls are serialised by mu: a single PCM fd is not concurrency-safe, and
// the renderer's consumer goroutine calls Write/Delay while Close may race on
// shutdown. The shape (method set, errUnderrun on -EPIPE, idempotent draining
// Close, Delay degrading to ok=false) matches the former cgo sink's contract.
type alsaSink struct {
	mu sync.Mutex

	device   string
	channels int
	started  bool
	closed   bool

	fd int
}

// newALSASink builds the ALSA sink for the given device. It does NOT open the
// device; Start does, so construction never touches hardware. device "" maps to
// the first usable playback node ("default"). The error return mirrors the
// !linux stub so callers handle both builds uniformly.
func newALSASink(device string) (*alsaSink, error) {
	if device == "" {
		device = "default"
	}
	return &alsaSink{device: device, fd: -1}, nil
}

// resolveDevice maps a device string to a /dev/snd/pcmC<c>D<d>p path. "default"
// (or "") picks the lowest-numbered playback node. An explicit "hw:C,D" or
// "C,D" form is parsed; anything else is treated as a literal path if it exists.
func resolveDevice(device string) (string, error) {
	switch device {
	case "", "default":
		nodes, err := playbackNodes()
		if err != nil {
			return "", err
		}
		if len(nodes) == 0 {
			return "", fmt.Errorf("alsa: no playback PCM node under /dev/snd")
		}
		return nodes[0], nil
	}
	if c, d, ok := parseHWDevice(device); ok {
		return fmt.Sprintf("/dev/snd/pcmC%dD%dp", c, d), nil
	}
	if _, err := os.Stat(device); err == nil {
		return device, nil
	}
	return "", fmt.Errorf("alsa: cannot resolve device %q", device)
}

// parseHWDevice parses "hw:C,D" / "hw:C" / "C,D" / "C" into card/device numbers.
func parseHWDevice(s string) (card, dev int, ok bool) {
	t := s
	if len(t) > 3 && t[:3] == "hw:" {
		t = t[3:]
	}
	dev = 0
	n, err := fmt.Sscanf(t, "%d,%d", &card, &dev)
	if err == nil && n == 2 {
		return card, dev, true
	}
	if n, err := fmt.Sscanf(t, "%d", &card); err == nil && n == 1 {
		return card, 0, true
	}
	return 0, 0, false
}

// playbackNodes enumerates /dev/snd/pcmC*D*p playback character devices, sorted
// by card then device so "default" is deterministic.
func playbackNodes() ([]string, error) {
	entries, err := filepath.Glob("/dev/snd/pcmC*D*p")
	if err != nil {
		return nil, err
	}
	sort.Strings(entries)
	return entries, nil
}

// ioctl issues a raw ioctl on fd with the given request and arg pointer. It
// returns the kernel's negative errno as a unix.Errno (>0) on failure.
func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// Start opens the playback node O_RDWR and runs the standard uapi bring-up:
// PVERSION gate -> HW_REFINE -> HW_PARAMS (FLOAT_LE / RW_INTERLEAVED / channels /
// rate / ~100ms buffer) -> SW_PARAMS -> PREPARE. Blocking mode is the
// backpressure. It is an error to Start an already-started or closed sink.
func (s *alsaSink) Start(rate, channels int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("alsa: sink closed")
	}
	if s.started {
		return fmt.Errorf("alsa: already started")
	}
	if rate <= 0 || channels <= 0 {
		return fmt.Errorf("alsa: invalid rate/channels %d/%d", rate, channels)
	}

	path, err := resolveDevice(s.device)
	if err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("alsa: open %s: %w", path, err)
	}
	// From here on, any failure must close fd.
	cleanup := func(e error) error {
		_ = unix.Close(fd)
		return e
	}

	// PVERSION gate: require the kernel's major to match our pinned layout, else
	// fail closed (degrade to coarse) rather than risk a layout mismatch.
	var pver int32
	if err := ioctl(fd, SNDRV_PCM_IOCTL_PVERSION, unsafe.Pointer(&pver)); err != nil {
		return cleanup(fmt.Errorf("alsa: PVERSION: %w", err))
	}
	if (uint32(pver) >> 16) != sndrvPCMVersionMajor {
		return cleanup(fmt.Errorf("alsa: unsupported PCM uapi version 0x%x (want major %d)", uint32(pver), sndrvPCMVersionMajor))
	}

	if err := s.configure(fd, rate, channels); err != nil {
		return cleanup(err)
	}

	s.fd = fd
	s.channels = channels
	s.started = true
	return nil
}

// configure runs HW_REFINE/HW_PARAMS/SW_PARAMS/PREPARE on fd for the canonical
// interleaved-float format. Split out so the bring-up is testable in isolation.
func (s *alsaSink) configure(fd, rate, channels int) error {
	// HW_REFINE: widen everything, let the kernel narrow to what the card supports.
	var hw sndPCMHwParams
	hw.setAllRanges()
	if err := ioctl(fd, SNDRV_PCM_IOCTL_HW_REFINE, unsafe.Pointer(&hw)); err != nil {
		return fmt.Errorf("alsa: HW_REFINE: %w", err)
	}

	// HW_PARAMS: pin our desired format. Rebuild from a clean all-ranges base, then
	// fix access/format masks and the channels/rate/period/buffer intervals.
	hw = sndPCMHwParams{}
	hw.setAllRanges()

	acc := hw.mask(sndrvPCMHwParamAccess)
	acc.none()
	acc.set(sndrvPCMAccessRWInterleaved)

	fmtMask := hw.mask(sndrvPCMHwParamFormat)
	fmtMask.none()
	fmtMask.set(sndrvPCMFormatFloatLE)

	hw.interval(sndrvPCMHwParamChannels).setInteger(uint32(channels))
	hw.interval(sndrvPCMHwParamRate).setInteger(uint32(rate))

	// ~100ms device buffer; the Ring is the real jitter buffer (06 §1.3). Pin the
	// buffer to ~100ms of frames and let the kernel choose periods within it.
	bufFrames := uint32(rate / 10)
	periodFrames := bufFrames / 4 // ~25ms periods, 4 per buffer
	if periodFrames == 0 {
		periodFrames = bufFrames
	}
	hw.interval(sndrvPCMHwParamBufferSize).setRange(bufFrames, bufFrames*2)
	hw.interval(sndrvPCMHwParamPeriodSize).setRange(periodFrames, bufFrames)

	hw.Rmask = ^uint32(0)
	if err := ioctl(fd, SNDRV_PCM_IOCTL_HW_PARAMS, unsafe.Pointer(&hw)); err != nil {
		return fmt.Errorf("alsa: HW_PARAMS: %w", err)
	}

	// Read back the granted buffer/period sizes for SW_PARAMS thresholds.
	grantedBuf := hw.interval(sndrvPCMHwParamBufferSize).Max
	grantedPeriod := hw.interval(sndrvPCMHwParamPeriodSize).Max
	if grantedBuf == 0 {
		grantedBuf = bufFrames
	}
	if grantedPeriod == 0 {
		grantedPeriod = periodFrames
	}

	// SW_PARAMS: start playback once a full period is queued; never auto-stop
	// (boundary), so a transient underrun reports -EPIPE (which we recover) rather
	// than silently stopping. proto carries our uapi version.
	var sw sndPCMSwParams
	sw.TstampMode = sndrvPCMTstampNone
	sw.PeriodStep = 1
	sw.AvailMin = sndPCMUframes(grantedPeriod)
	sw.StartThreshold = sndPCMUframes(grantedPeriod)
	// boundary must be a power-of-two multiple of buffer size; use a large LCM-ish
	// value. The kernel only requires stop_threshold/silence comparisons against it.
	boundary := sndPCMUframes(grantedBuf)
	for boundary < (1 << 40) {
		boundary *= 2
	}
	sw.Boundary = boundary
	sw.StopThreshold = boundary // never auto-stop on xrun; we PREPARE to recover
	sw.SilenceThreshold = 0
	sw.SilenceSize = 0
	sw.Proto = uint32(sndrvPCMVersion)
	if err := ioctl(fd, SNDRV_PCM_IOCTL_SW_PARAMS, unsafe.Pointer(&sw)); err != nil {
		return fmt.Errorf("alsa: SW_PARAMS: %w", err)
	}

	if err := ioctl(fd, SNDRV_PCM_IOCTL_PREPARE, nil); err != nil {
		return fmt.Errorf("alsa: PREPARE: %w", err)
	}
	return nil
}

// Write submits interleaved float frames via WRITEI_FRAMES in a loop until all
// are accepted; returns consumed SAMPLES (frames×channels). On -EPIPE (underrun)
// it PREPAREs to recover and returns errUnderrun (renderer reseeks). On -ESTRPIPE
// (suspend) it RESUME/PREPAREs and retries silently. len(frames) must be a
// multiple of channels. Blocking mode is the backpressure.
func (s *alsaSink) Write(frames []float32) (int, error) {
	if len(frames) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.fd < 0 {
		return 0, fmt.Errorf("alsa: write before start")
	}
	if s.channels > 0 && len(frames)%s.channels != 0 {
		return 0, fmt.Errorf("alsa: %d samples not a multiple of %d channels", len(frames), s.channels)
	}

	consumed := 0
	for consumed < len(frames) {
		rem := frames[consumed:]
		nframes := len(rem) / s.channels
		if nframes == 0 {
			break
		}
		xfer := sndXferi{
			Buf:    uintptr(unsafe.Pointer(&rem[0])),
			Frames: sndPCMUframes(nframes),
		}
		err := ioctl(s.fd, SNDRV_PCM_IOCTL_WRITEI_FRAMES, unsafe.Pointer(&xfer))
		if err == nil {
			// xfer.Result is the frame count the kernel accepted.
			got := int(xfer.Result)
			if got <= 0 {
				break
			}
			consumed += got * s.channels
			continue
		}
		switch err {
		case unix.EPIPE:
			// Underrun: recover by re-PREPARE, then surface as a gross error.
			_ = ioctl(s.fd, SNDRV_PCM_IOCTL_PREPARE, nil)
			return consumed, errUnderrun
		case unix.ESTRPIPE:
			// Suspended: try RESUME until it succeeds, else PREPARE, then retry.
			for {
				rerr := ioctl(s.fd, SNDRV_PCM_IOCTL_RESUME, nil)
				if rerr != unix.EAGAIN {
					break
				}
			}
			_ = ioctl(s.fd, SNDRV_PCM_IOCTL_PREPARE, nil)
			continue
		case unix.EINTR, unix.EAGAIN:
			continue
		default:
			return consumed, fmt.Errorf("alsa: WRITEI_FRAMES: %w", err)
		}
	}
	return consumed, nil
}

// Delay reports the samples-per-channel still outstanding in the device via
// SNDRV_PCM_IOCTL_DELAY (ok=true): the precise figure the drift loop wants. A
// negative or failed query degrades to (0,false) — never lies (06 §1.3).
func (s *alsaSink) Delay() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.fd < 0 {
		return 0, false
	}
	var d sndPCMSframes
	if err := ioctl(s.fd, SNDRV_PCM_IOCTL_DELAY, unsafe.Pointer(&d)); err != nil {
		return 0, false
	}
	if d < 0 {
		return 0, false
	}
	return int(d), true
}

// Close DRAINs buffered audio then closes the fd. It is idempotent and safe to
// call without a successful Start.
func (s *alsaSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.started = false
	if s.fd < 0 {
		return nil
	}
	fd := s.fd
	s.fd = -1
	// Best-effort drain of buffered audio, then close.
	_ = ioctl(fd, SNDRV_PCM_IOCTL_DRAIN, nil)
	if err := unix.Close(fd); err != nil {
		return fmt.Errorf("alsa: close: %w", err)
	}
	return nil
}

// probeALSA is the alsa backend's liveness check (06 §1.1): a usable card must
// open AND complete HW_PARAMS/PREPARE for the canonical float format, else it is
// NOT advertised (busy / owned by a sound server / format-incapable). It opens,
// configures, and closes a real node — presence of the /dev/snd node alone is
// not sufficient.
func probeALSA(device string) bool {
	s, err := newALSASink(device)
	if err != nil {
		return false
	}
	// Probe at the canonical 48k/stereo float format the renderer commits.
	if err := s.Start(48000, 2); err != nil {
		return false
	}
	_ = s.Close()
	return true
}

var _ AudioSink = (*alsaSink)(nil)
