package sink

import (
	"fmt"
	"log/slog"
	"sync"
	"unsafe"

	"ensemble/internal/contracts"
	"ensemble/internal/dl"
	"ensemble/internal/stream"
	"os"
	"strconv"
	"time"
)

// ALSA simple-API constants (D34).
const (
	sndPCMStreamPlayback   = 0 // SND_PCM_STREAM_PLAYBACK
	sndPCMFormatS16LE      = 2 // SND_PCM_FORMAT_S16_LE
	sndPCMAccessRWInterlvd = 3 // SND_PCM_ACCESS_RW_INTERLEAVED
	alsaSoftResample       = 1 // enable libasound's own resampler if needed
	errEPIPE               = -32
	errESTRPIPE            = -86
)

// alsaSonames / alsaSymbols feed the dl.Open probe (D32/D34).
var (
	alsaSonames = []string{"libasound.so.2", "libasound.so"}
	alsaSymbols = []string{
		"snd_pcm_open", "snd_pcm_set_params", "snd_pcm_writei",
		"snd_pcm_recover", "snd_pcm_delay", "snd_pcm_close",
		"snd_pcm_drop", "snd_pcm_prepare",
	}
)

// alsaFuncs holds the bound libasound symbols, bound once at init().
type alsaFuncs struct {
	lib       *dl.Lib
	open      func(pcmp *uintptr, name string, stream, mode int32) int32
	setParams func(pcm uintptr, format, access, channels, rate, softResample, latencyUs int32) int32
	writei    func(pcm uintptr, buf uintptr, frames uintptr) int
	recover   func(pcm uintptr, err, silent int32) int32
	delay     func(pcm uintptr, delayp *int) int32
	close     func(pcm uintptr) int32
	drop      func(pcm uintptr) int32
	prepare   func(pcm uintptr) int32
}

var (
	alsaProbeOnce sync.Once
	alsaBound     *alsaFuncs
)

// init attempts the dl.Open probe; on success it binds the symbols and registers
// the "alsa" backend. No build tag (D32) — on a host without libasound the probe
// soft-fails and "alsa" is simply absent from the registry.
func init() {
	f, err := probeAlsa()
	if err != nil {
		return // capability off; "alsa" not registered (D3)
	}
	alsaBound = f
	Register("alsa", func(arg string, log *slog.Logger) (contracts.Backend, error) {
		// arg carries the configured device (D37, via OpenDevice); empty => default.
		return newAlsaBackend(arg, log)
	})
}

func probeAlsa() (*alsaFuncs, error) {
	lib, err := dl.Open(alsaSonames, alsaSymbols)
	if err != nil {
		return nil, err
	}
	f := &alsaFuncs{lib: lib}
	lib.Func(&f.open, "snd_pcm_open")
	lib.Func(&f.setParams, "snd_pcm_set_params")
	lib.Func(&f.writei, "snd_pcm_writei")
	lib.Func(&f.recover, "snd_pcm_recover")
	lib.Func(&f.delay, "snd_pcm_delay")
	lib.Func(&f.drop, "snd_pcm_drop")
	lib.Func(&f.prepare, "snd_pcm_prepare")
	lib.Func(&f.close, "snd_pcm_close")
	return f, nil
}

// alsaBackend writes canonical PCM straight to an ALSA PCM device via
// runtime-loaded libasound (D34). It implements contracts.DelayReporter
// (snd_pcm_delay) for exact servo measurement (§3.5).
type alsaBackend struct {
	xruns       uint64
	lastXrunLog time.Time
	f           *alsaFuncs
	pcm         uintptr
	log         *slog.Logger
	mu          sync.Mutex
	closed      bool
}

// newAlsaBackend opens the named device (empty => "default") and sets canonical
// params. Called only when the probe at init() succeeded (alsaBound != nil).
func newAlsaBackend(device string, log *slog.Logger) (*alsaBackend, error) {
	if alsaBound == nil {
		return nil, fmt.Errorf("alsa: libasound not loaded")
	}
	if log == nil {
		log = slog.Default()
	}
	log = log.With("backend", "alsa") // comp=sink already attributed by the registry

	if device == "" {
		device = "default"
	}
	var pcm uintptr
	if rc := alsaBound.open(&pcm, device, sndPCMStreamPlayback, 0); rc < 0 {
		return nil, fmt.Errorf("alsa: snd_pcm_open(%s) failed: %d", device, rc)
	}
	// Small FIXED device latency, deliberately NOT bufferMs: bufferMs is the
	// NETWORK jitter budget held in our own jitter buffer; handing it to the
	// device as well doubled the end-to-end delay on alsa nodes and created a
	// ~180ms audible phase offset against pipe-backend nodes (whose players
	// buffer far less). Default 100ms: 60ms proved too tight on Pi-class
	// hardware (scheduling jitter -> device xruns = chop with clean pipeline
	// counters). Tunable per host without rebuild via ENSEMBLE_ALSA_LATENCY_MS
	// (20..500); a constant inter-device difference is what the per-node
	// outputDelayMs calibration is for (D36).
	latencyMs := 100
	if v := os.Getenv("ENSEMBLE_ALSA_LATENCY_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 20 && ms <= 500 {
			latencyMs = ms
		}
	}
	latencyUs := int32(latencyMs * 1000)
	if rc := alsaBound.setParams(pcm, sndPCMFormatS16LE, sndPCMAccessRWInterlvd,
		stream.Channels, stream.SampleRate, alsaSoftResample, latencyUs); rc < 0 {
		alsaBound.close(pcm)
		return nil, fmt.Errorf("alsa: snd_pcm_set_params failed: %d", rc)
	}
	log.Info("alsa backend opened", "device", device)
	return &alsaBackend{f: alsaBound, pcm: pcm, log: log}, nil
}

func (b *alsaBackend) Write(frame []byte) error {
	if len(frame) != stream.FrameBytes {
		return fmt.Errorf("alsa: frame %d bytes, want %d", len(frame), stream.FrameBytes)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return fmt.Errorf("alsa: closed")
	}
	buf := uintptr(unsafe.Pointer(&frame[0]))
	n := b.f.writei(b.pcm, buf, uintptr(stream.FrameSamples))
	if n < 0 {
		// underrun (-EPIPE) or suspend (-ESTRPIPE): recover and retry once.
		// Every recovery is an AUDIBLE glitch the pipeline counters cannot
		// see — make it loud in the logs (rate-limited to 1/s) so "choppy
		// but clean counters" is diagnosable from the log alone.
		b.xruns++
		if now := time.Now(); now.Sub(b.lastXrunLog) > time.Second {
			b.lastXrunLog = now
			b.log.Warn("alsa xrun (device underrun) — audible glitch; consider a larger ENSEMBLE_ALSA_LATENCY_MS",
				"err", n, "xruns", b.xruns)
		}
		rc := b.f.recover(b.pcm, int32(n), 1)
		if rc < 0 {
			return fmt.Errorf("alsa: writei %d, recover %d", n, rc)
		}
		n = b.f.writei(b.pcm, buf, uintptr(stream.FrameSamples))
		if n < 0 {
			return fmt.Errorf("alsa: writei after recover: %d", n)
		}
	}
	return nil
}

// DeviceDelay returns the queued audio between Write and the speaker in ns
// (contracts.DelayReporter). The servo prefers this exact measurement (§3.5).
func (b *alsaBackend) DeviceDelay() (int64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, false
	}
	var frames int
	if rc := b.f.delay(b.pcm, &frames); rc < 0 {
		return 0, false
	}
	if frames < 0 {
		frames = 0
	}
	ns := int64(frames) * stream.FrameNanos / int64(stream.FrameSamples)
	return ns, true
}

func (b *alsaBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	if rc := b.f.close(b.pcm); rc < 0 {
		return fmt.Errorf("alsa: snd_pcm_close: %d", rc)
	}
	return nil
}

// Flush drops queued-but-unplayed audio and re-prepares the device
// (contracts.Flusher): a session ended — whatever the device retains must
// never replay at the next session's start.
func (b *alsaBackend) Flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	if rc := b.f.drop(b.pcm); rc < 0 {
		b.log.Debug("alsa drop failed", "rc", rc)
	}
	if rc := b.f.prepare(b.pcm); rc < 0 {
		b.log.Debug("alsa prepare after drop failed", "rc", rc)
	}
}
