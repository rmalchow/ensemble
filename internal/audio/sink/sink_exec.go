package audio

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
)

// ExecSink is the coarse, always-available AudioSink: it spawns a player
// subprocess (aplay or pw-play) and writes interleaved S16_LE PCM to its stdin.
// The pipe to the player provides natural backpressure — the player drains at
// the DAC rate, so once its kernel/player buffer fills, Write blocks, which is
// exactly the playout pacing the renderer relies on. It needs no audio library
// at all (only the player binary on PATH), so the registry can almost always
// offer it as the universal fallback to the precise alsa backend.
//
// Delay() always reports (0,false): a pipe gives no readback of the player's
// outstanding DAC samples, so the renderer's drift loop falls back to its
// coarse content model. This is the deliberate accuracy tradeoff for the
// portable coarse tier (06 §1.2/§1.4).
type ExecSink struct {
	// command is the player command template. argv[0] is the binary; the
	// remaining elements are passed verbatim. The placeholders below are
	// substituted in Start once rate/channels are known:
	//   {rate} -> sample rate in Hz
	//   {channels} -> channel count
	//   {device} -> the device string
	// A nil command falls back to defaultPlayerCommand(device).
	command []string
	device  string

	mu      sync.Mutex
	started bool
	closed  bool
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	// buf is a reusable byte scratch slice for the f32->S16_LE conversion so the
	// hot Write path does not allocate per call.
	buf      []byte
	channels int
}

// NewExecSink builds a coarse exec sink for the given device using the default
// player command (aplay). device "" maps to "default". Channels/rate are fixed
// at Start. The registry constructs a pw-play variant directly via the command
// field (see pwPlayCommand); tests do the same with a stub script.
func NewExecSink(device string) *ExecSink {
	if device == "" {
		device = "default"
	}
	return &ExecSink{device: device}
}

// defaultPlayerCommand is the aplay invocation with placeholders. We keep aplay
// "quiet" (-q), take raw S16_LE PCM from stdin ("-"), and let aplay manage its
// own ring; the jitter buffer is the renderer's Ring, not aplay's, so we don't
// inflate aplay's buffer here (06 §1.2).
func defaultPlayerCommand(device string) []string {
	return []string{
		"aplay",
		"-q",
		"-t", "raw",
		"-f", "S16_LE",
		"-r", "{rate}",
		"-c", "{channels}",
		"-D", device,
		"-",
	}
}

// pwPlayCommand is the PipeWire pw-play invocation with placeholders: raw
// S16_LE PCM from stdin, the same f32->s16 wire format as aplay. On a PipeWire
// box the card is owned by the sound server (so the precise alsa backend's
// probe fails) and pw-play is the right coarse path (06 §1.1 verification spike
// note). device is passed via --target; "default" lets pw-play pick the sink.
func pwPlayCommand(device string) []string {
	return []string{
		"pw-play",
		"--rate", "{rate}",
		"--channels", "{channels}",
		"--format", "s16",
		"--target", device,
		"-",
	}
}

// Start launches the player process and grabs its stdin pipe. It is an error to
// Start twice or to Start a closed sink.
func (s *ExecSink) Start(rate, channels int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("audio: exec sink closed")
	}
	if s.started {
		return errors.New("audio: exec sink already started")
	}
	if rate <= 0 || channels <= 0 {
		return fmt.Errorf("audio: invalid rate/channels %d/%d", rate, channels)
	}

	tmpl := s.command
	if tmpl == nil {
		tmpl = defaultPlayerCommand(s.device)
	}
	argv := expandPlayerCommand(tmpl, rate, channels, s.device)
	if len(argv) == 0 || argv[0] == "" {
		return errors.New("audio: empty player command")
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("audio: player stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("audio: start player %q: %w", argv[0], err)
	}

	s.cmd = cmd
	s.stdin = stdin
	s.channels = channels
	s.started = true
	return nil
}

// expandPlayerCommand substitutes the {rate}/{channels}/{device} placeholders in
// the command template. It is split out so tests can verify substitution without
// launching a process.
func expandPlayerCommand(tmpl []string, rate, channels int, device string) []string {
	out := make([]string, len(tmpl))
	for i, a := range tmpl {
		switch a {
		case "{rate}":
			out[i] = strconv.Itoa(rate)
		case "{channels}":
			out[i] = strconv.Itoa(channels)
		case "{device}":
			out[i] = device
		default:
			out[i] = a
		}
	}
	return out
}

// Write converts the interleaved f32 frames to S16_LE and writes them to the
// player's stdin, blocking for backpressure. It returns the number of float32
// samples consumed (always len(frames) on success, mirroring the seam contract).
// A broken pipe / dead player surfaces as an error so the renderer can recover.
func (s *ExecSink) Write(frames []float32) (int, error) {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return 0, errors.New("audio: exec sink not started")
	}
	if s.closed {
		s.mu.Unlock()
		return 0, errors.New("audio: exec sink closed")
	}
	if s.channels > 0 && len(frames)%s.channels != 0 {
		s.mu.Unlock()
		return 0, fmt.Errorf("audio: frames len %d not a multiple of channels %d", len(frames), s.channels)
	}
	stdin := s.stdin
	// Reuse the scratch buffer (it is only touched under the lock).
	if cap(s.buf) < len(frames)*2 {
		s.buf = make([]byte, len(frames)*2)
	}
	out := s.buf[:len(frames)*2]
	f32ToS16LE(frames, out)
	s.mu.Unlock()

	if _, err := stdin.Write(out); err != nil {
		return 0, fmt.Errorf("audio: write to player: %w", err)
	}
	return len(frames), nil
}

// Delay reports no precise outstanding-sample figure: a pipe to the player has
// no readback. The renderer therefore uses its coarse drift model (06 §1.2).
func (s *ExecSink) Delay() (int, bool) { return 0, false }

// Close closes the player's stdin (signalling EOF so it drains its buffer) and
// waits for the process to exit. It is idempotent and safe to call without a
// successful Start.
func (s *ExecSink) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	stdin := s.stdin
	cmd := s.cmd
	s.stdin = nil
	s.cmd = nil
	s.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil {
		if err := cmd.Wait(); err != nil {
			// A non-zero exit after we closed stdin is the normal drain-then-exit
			// for some players; surface it so callers can log, but it is not fatal.
			return fmt.Errorf("audio: player exit: %w", err)
		}
	}
	return nil
}

// f32ToS16LE converts interleaved float32 samples in [-1,1] to little-endian
// signed 16-bit PCM, writing 2 bytes per sample into dst (len(dst) must be
// 2*len(src)). Out-of-range inputs are clamped to [-1,1] so a hot sample never
// wraps to the opposite rail. Scale is 32767 (symmetric, never overflows the
// +1.0 rail into +32768). It is a pure function so it is table-tested directly.
func f32ToS16LE(src []float32, dst []byte) {
	for i, v := range src {
		if v > 1 {
			v = 1
		} else if v < -1 {
			v = -1
		}
		// Round to nearest to avoid a consistent downward bias on conversion.
		var n int32
		if v >= 0 {
			n = int32(v*32767 + 0.5)
		} else {
			n = int32(v*32767 - 0.5)
		}
		u := uint16(int16(n))
		dst[i*2] = byte(u)
		dst[i*2+1] = byte(u >> 8)
	}
}

// statically assert ExecSink satisfies the seam.
var _ AudioSink = (*ExecSink)(nil)
