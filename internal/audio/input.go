package audio

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"

	"ensemble/internal/stream"
)

// inputSource is a live-paced source over an exec-captured raw s16le pipe
// (pw-record/arecord), mirroring E's exec playback backend (§6.1, D27).
type inputSource struct {
	*liveReader
}

// openInput starts a capture subprocess emitting raw 48 kHz stereo s16le on
// stdout and frames it live-paced. The URI may name a capture device after the
// scheme — "input:" = system default, "input:<dev>" selects a specific source
// (a PipeWire source node name for pw-record, or an ALSA "hw:C,D" for arecord;
// see ListInputDevices). Everything after the first colon is the device, so an
// ALSA "hw:1,0" passes through intact.
func openInput(ctx context.Context, uri, _ string) (Source, error) {
	device := ""
	if i := strings.IndexByte(uri, ':'); i >= 0 {
		device = strings.TrimSpace(uri[i+1:])
	}

	bin := findCaptureBinary()
	if bin == "" {
		return nil, fmt.Errorf("%w: no capture backend (pw-record/arecord)", ErrBadMedia)
	}

	cctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cctx, bin, captureArgs(bin, device)...)
	cmd.Cancel = func() error { return cmd.Process.Kill() }

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: capture stdout: %v", ErrBadMedia, err)
	}
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("%w: capture start: %v", ErrBadMedia, err)
	}
	slog.Debug("capture started", "comp", "audio", "bin", bin)

	dec := &rawS16Source{r: stdout}
	fr := newFramer(dec)
	cleanup := func() {
		_ = cmd.Wait()
	}
	lr := newLiveReader(fr, cancel, cleanup)
	return &inputSource{liveReader: lr}, nil
}

// captureArgs builds the argv for the capture tool to emit raw s16le 48k stereo.
// A non-empty device selects a specific source: arecord takes "-D <hw:C,D>";
// pw-record links to a "--target <node>".
func captureArgs(bin, device string) []string {
	switch baseName(bin) {
	case "arecord":
		args := []string{"-f", "S16_LE", "-r", "48000", "-c", "2", "-t", "raw"}
		if device != "" {
			args = append(args, "-D", device)
		}
		return append(args, "-")
	default: // pw-record and look-alikes
		args := []string{"--rate", "48000", "--channels", "2", "--format", "s16"}
		if device != "" {
			args = append(args, "--target", device)
		}
		return append(args, "-")
	}
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// rawS16Source reads raw interleaved s16le 48 kHz stereo bytes off a pipe. The
// resampler is pass-through and there is no mono-dup.
type rawS16Source struct {
	r   io.Reader
	odd []byte // carry for a partial 4-byte sample-frame
	eof bool
}

func (s *rawS16Source) info() (int, int) { return stream.SampleRate, stream.Channels }

func (s *rawS16Source) Close() error { return nil }

func (s *rawS16Source) read(dst []int16) ([]int16, error) {
	if s.eof {
		return dst, io.EOF
	}
	const blk = 8192
	buf := make([]byte, len(s.odd)+blk)
	copy(buf, s.odd)
	n, err := s.r.Read(buf[len(s.odd):])
	total := len(s.odd) + n
	buf = buf[:total]
	s.odd = nil

	whole := (total / 2) * 2
	for off := 0; off+1 < whole; off += 2 {
		dst = append(dst, int16(binary.LittleEndian.Uint16(buf[off:])))
	}
	if rem := buf[whole:]; len(rem) > 0 {
		s.odd = append([]byte(nil), rem...)
	}

	if err == io.EOF || err == io.ErrUnexpectedEOF {
		s.eof = true
		return dst, io.EOF
	}
	if err != nil {
		// A pipe read error ends the producer; the live layer turns the closed
		// channel into silence, so this is not surfaced as fatal.
		s.eof = true
		return dst, io.EOF
	}
	return dst, nil
}
