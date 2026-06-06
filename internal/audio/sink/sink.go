// Package audio (import path internal/audio/sink) is the output-device seam for
// an Ensemble player node: the canonical AudioSink interface (README §6.1) plus
// the runtime backend registry (Probe/Open/Backend, 06 §1.1) that discovers and
// opens a usable output backend at startup.
//
// Two tiers of sink satisfy the seam, selected at RUNTIME (never via build tags):
//
//   - precise — the alsa backend (sink_alsa.go), which drives the kernel PCM
//     character device /dev/snd/pcmC<card>D<dev>p DIRECTLY through ioctls via
//     golang.org/x/sys/unix. No libasound, no dlopen/purego, no cgo — the whole
//     binary stays pure-Go and cross-compiles to arm64 with no C toolchain (D24,
//     06 §1.3). Its precise Delay() reads the kernel hardware pointer.
//   - coarse — the exec backends (sink_exec.go), which spawn a player subprocess
//     (aplay / pw-play) and pipe interleaved S16_LE PCM to its stdin. The pipe is
//     the backpressure; Delay() returns (0,false).
//
// Selection is a runtime choice made by the registry: every OS-supported backend
// is compiled into the one pure-Go binary, probed for real usability at startup,
// minus the operator's config-disabled paths. A node with zero usable+enabled
// backends reports Render=false upstream (06 §1.5). The package is a leaf: the
// render loop imports THIS; it imports no sibling audio package, no group, no web.
package audio

import "errors"

// AudioSink is a blocking PCM output device. Write blocks until the frames are
// accepted (i.e. it provides backpressure = the playout pacing). All audio is
// interleaved float32 in [-1,1], `channels` channels per frame. Reproduced
// verbatim from README §6.1 / 06 §1; do not redefine.
type AudioSink interface {
	// Start opens the device at rate Hz / channels. Idempotent error if already started.
	Start(rate, channels int) error
	// Write enqueues interleaved frames (len must be a multiple of channels). Returns
	// the number of float32 SAMPLES consumed (not frames). Blocks for backpressure.
	Write(frames []float32) (n int, err error)
	// Delay reports samples-per-channel still outstanding in the device (not yet
	// played out the DAC). ok=false => no precise figure available; the drift loop
	// then falls back to the coarse content model. The alsa backend returns a kernel
	// figure (ok=true); the exec sinks (pipe to aplay/pw-play) return ok=false.
	Delay() (samples int, ok bool)
	Close() error
}

// ErrUnderrun is returned by alsaSink.Write on -EPIPE (device underrun) after it
// re-PREPAREs the PCM, so the renderer treats it as a gross error and reseeks.
// It lives here (not in the linux-tagged ALSA file) so it is comparable via
// errors.Is in every build, including the !linux dev host. (06 §1.3, §6.4)
//
// Exported so the render loop (internal/audio/render, P4.7) can distinguish a
// gross device underrun from a transient write error via errors.Is and reseek
// immediately (doc 06 §6.4, P4.7 risk 1). errUnderrun is the package-internal
// alias the ALSA backend returns.
var ErrUnderrun = errors.New("alsa: underrun (-EPIPE)")

// errUnderrun is the package-internal alias for ErrUnderrun (kept so the ALSA
// backend's existing call sites are unchanged).
var errUnderrun = ErrUnderrun
