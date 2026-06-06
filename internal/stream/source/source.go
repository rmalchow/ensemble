// Package source is the master's input decode stage: it turns a media input — an
// mp3, FLAC, or WAV/PCM file in the node's data/ folder, or an HTTP(S) stream URL
// (D14) — into a single continuous interleaved float32 PCM stream at the group's
// canonical rate (48000 Hz, 2 channels; A.12), looping seamlessly at EOF so the
// group timeline is one unbroken sample counter (05 §5.2.1). It is wire-codec
// agnostic: it knows nothing about PCM/Opus framing, streamGen, FEC, or the wire
// header — it only produces rate-uniform PCM for internal/stream/origin (P4.3).
package source

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hajimehoshi/go-mp3"
)

// Reader is the master's decoded-PCM input stream. Read fills dst with up to
// len(dst) interleaved float32 frames at the canonical rate; it LOOPS at EOF and
// NEVER returns io.EOF while looping (05 §5.3). n is always a multiple of
// Channels().
type Reader interface {
	Read(dst []float32) (frames int, err error)
	Rate() int     // canonical rate, e.g. 48000
	Channels() int // 2
	Close() error
}

// MediaInfo describes one playable item in data/ (fields drive the Media screen,
// 09). Name is the value stored in ConfigDoc.Groups[].Media.File (07).
type MediaInfo struct {
	Name       string // base filename
	Format     string // "mp3" | "flac" | "wav"
	SizeBytes  int64
	SampleRate int // native rate from the file header (0 if unknown w/o full decode)
	Channels   int // native channel count (0 if unknown)
}

// frameSource is a finite, native-rate decoder; loopReader/resampler wrap it.
type frameSource interface {
	read(dst []float32) (n int, err error) // native rate/channels; io.EOF at true end
	rate() int
	channels() int
	seekStart() error // reposition to frame 0 for looping (loop.go)
	close() error
}

// Open decodes a source (mp3/FLAC/WAV) from the data/ folder — or an HTTP(S)
// stream URL — and returns a looping Reader emitting interleaved float32 at
// canonicalRate / channels (05 §5.3). path is either a filesystem path to a media
// file (the caller resolves it under the node's data/ folder, config Paths.Data)
// or an http(s):// URL.
func Open(path string, canonicalRate, channels int) (Reader, error) {
	if canonicalRate <= 0 || channels <= 0 {
		return nil, fmt.Errorf("source: invalid canonical config %d/%d", canonicalRate, channels)
	}
	rc, reopen, err := openInput(path)
	if err != nil {
		return nil, err
	}
	fs, freopen, err := decodeSource(rc, path, reopen)
	if err != nil {
		return nil, err
	}
	return assemble(fs, freopen, canonicalRate, channels)
}

// decodeSource sniffs the format (extension + magic bytes), builds the matching
// frameSource over rc, and returns a frameSource-level reopen closure for the loop
// layer (non-nil only for non-seekable HTTP inputs).
func decodeSource(rc io.ReadCloser, path string, reopen func() (io.ReadCloser, error)) (frameSource, func() (frameSource, error), error) {
	format, peeked, err := sniff(rc, path)
	if err != nil {
		rc.Close()
		return nil, nil, err
	}
	// If we peeked bytes off a non-seekable stream, prepend them so the decoder
	// sees the full input. For a seekable file we rewind instead (cheaper, lets
	// FLAC/mp3/WAV open seekable for in-place loop).
	input, err := rewindOrPrepend(rc, peeked)
	if err != nil {
		rc.Close()
		return nil, nil, err
	}

	open := func(in io.ReadCloser) (frameSource, error) {
		switch format {
		case "mp3":
			return openMP3(in)
		case "flac":
			return openFLAC(in)
		case "wav":
			return openWAV(in)
		}
		in.Close()
		return nil, fmt.Errorf("source: unsupported format %q", format)
	}

	fs, err := open(input)
	if err != nil {
		return nil, nil, err
	}
	var freopen func() (frameSource, error)
	if reopen != nil {
		// HTTP path: looping re-issues the GET, re-sniffs implicitly (same URL,
		// same format) and re-decodes from the top.
		freopen = func() (frameSource, error) {
			fresh, oerr := reopen()
			if oerr != nil {
				return nil, oerr
			}
			return open(fresh)
		}
	}
	return fs, freopen, nil
}

// assemble wraps fs in the content resampler (only if native rate != canonical)
// and then the loopReader, yielding the public Reader. It validates the decoded
// channel count and adapts a mono source to stereo when channels==2.
func assemble(fs frameSource, freopen func() (frameSource, error), canonicalRate, channels int) (Reader, error) {
	wrapMono := func(s frameSource) (frameSource, error) {
		switch {
		case s.channels() == channels:
			return s, nil
		case s.channels() == 1 && channels == 2:
			return &monoToStereo{src: s}, nil
		default:
			err := fmt.Errorf("source: %d-channel media cannot map to %d channels", s.channels(), channels)
			s.close()
			return nil, err
		}
	}
	wrapRate := func(s frameSource) frameSource {
		if s.rate() == canonicalRate {
			return s
		}
		return newResampler(s, canonicalRate)
	}

	top, err := wrapMono(fs)
	if err != nil {
		return nil, err
	}
	top = wrapRate(top)

	var loopReopen func() (frameSource, error)
	if freopen != nil {
		loopReopen = func() (frameSource, error) {
			s, err := freopen()
			if err != nil {
				return nil, err
			}
			m, err := wrapMono(s)
			if err != nil {
				return nil, err
			}
			return wrapRate(m), nil
		}
	}
	return newLoopReader(top, canonicalRate, channels, loopReopen), nil
}

// rewindOrPrepend returns a ReadCloser that re-delivers `peeked` bytes ahead of
// rc. A seekable rc is rewound to 0 (so the decoder can re-read and stay seekable
// for in-place looping). A non-seekable rc is wrapped so peeked bytes prepend the
// remaining stream (losing seekability — looping uses reopen instead).
func rewindOrPrepend(rc io.ReadCloser, peeked []byte) (io.ReadCloser, error) {
	if s, ok := rc.(io.Seeker); ok {
		if _, err := s.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return rc, nil
	}
	return &prependReader{head: peeked, rest: rc}, nil
}

// prependReader serves `head` bytes first, then the rest of an underlying stream.
// It is non-seekable (used only for HTTP bodies after a magic-byte peek).
type prependReader struct {
	head []byte
	off  int
	rest io.ReadCloser
}

func (p *prependReader) Read(b []byte) (int, error) {
	if p.off < len(p.head) {
		n := copy(b, p.head[p.off:])
		p.off += n
		return n, nil
	}
	return p.rest.Read(b)
}

func (p *prependReader) Close() error { return p.rest.Close() }

// monoToStereo duplicates a single source channel into an interleaved stereo
// frame (§9: mono→stereo duplication lives here, ahead of the resampler).
type monoToStereo struct {
	src frameSource
	mono []float32 // reusable native-rate mono scratch
}

func (m *monoToStereo) rate() int     { return m.src.rate() }
func (m *monoToStereo) channels() int { return 2 }

func (m *monoToStereo) read(dst []float32) (int, error) {
	frames := len(dst) / 2
	if frames == 0 {
		return 0, nil
	}
	if cap(m.mono) < frames {
		m.mono = make([]float32, frames)
	}
	n, err := m.src.read(m.mono[:frames])
	for i := 0; i < n; i++ {
		v := m.mono[i]
		dst[2*i] = v
		dst[2*i+1] = v
	}
	return n * 2, err
}

func (m *monoToStereo) seekStart() error { return m.src.seekStart() }
func (m *monoToStereo) close() error     { return m.src.close() }

// sniff determines the source format from the filename extension and the leading
// magic bytes. It returns the format and the bytes consumed from rc (so the caller
// can rewind or prepend them). Magic wins over extension on conflict.
func sniff(rc io.Reader, path string) (format string, peeked []byte, err error) {
	const peek = 16
	buf := make([]byte, peek)
	n, rerr := io.ReadFull(rc, buf)
	if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
		return "", nil, fmt.Errorf("source: peek %s: %w", path, rerr)
	}
	buf = buf[:n]
	if f := sniffMagic(buf); f != "" {
		return f, buf, nil
	}
	if f := formatFromExt(path); f != "" {
		return f, buf, nil
	}
	return "", nil, fmt.Errorf("source: unrecognized media format for %q", path)
}

// sniffMagic recognizes the leading bytes of mp3 (ID3 tag or MPEG sync 0xFFEx/Fx),
// FLAC (fLaC), and WAV (RIFF....WAVE).
func sniffMagic(b []byte) string {
	if len(b) >= 4 && string(b[0:4]) == "fLaC" {
		return "flac"
	}
	if len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WAVE" {
		return "wav"
	}
	if len(b) >= 3 && string(b[0:3]) == "ID3" {
		return "mp3"
	}
	// MPEG audio frame sync: 11 set bits (0xFF followed by 0xE_/0xF_).
	if len(b) >= 2 && b[0] == 0xFF && (b[1]&0xE0) == 0xE0 {
		return "mp3"
	}
	return ""
}

// formatFromExt maps a filename extension to a source format.
func formatFromExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp3":
		return "mp3"
	case ".flac":
		return "flac"
	case ".wav", ".wave":
		return "wav"
	}
	return ""
}

// List enumerates playable media in dataDir (non-recursive), filtered to
// {mp3,flac,wav} by extension, sorted by Name. SampleRate/Channels are read from
// the file header without a full decode (0 if unavailable). An empty dir yields an
// empty slice and nil error; a missing dir yields an error.
func List(dataDir string) ([]MediaInfo, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, err
	}
	out := make([]MediaInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		format := formatFromExt(e.Name())
		if format == "" {
			continue
		}
		mi := MediaInfo{Name: e.Name(), Format: format}
		if info, ierr := e.Info(); ierr == nil {
			mi.SizeBytes = info.Size()
		}
		rate, ch := probeHeader(filepath.Join(dataDir, e.Name()), format)
		mi.SampleRate = rate
		mi.Channels = ch
		out = append(out, mi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SubDirs enumerates the immediate subdirectories of dir (non-recursive,
// sorted, hidden dot-dirs skipped) so a media browser can descend into a
// per-album folder layout. An empty dir yields an empty slice and nil error.
func SubDirs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// probeHeader reads the native rate/channels from a file header cheaply (no full
// decode). Returns 0,0 on any error so List degrades gracefully.
func probeHeader(path, format string) (rate, channels int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	switch format {
	case "flac":
		s, err := openFLAC(noClose{f})
		if err != nil {
			return 0, 0
		}
		return s.rate(), s.channels()
	case "wav":
		s, err := openWAV(noClose{f})
		if err != nil {
			return 0, 0
		}
		return s.rate(), s.channels()
	case "mp3":
		dec, err := mp3.NewDecoder(f)
		if err != nil {
			return 0, 0
		}
		return dec.SampleRate(), mp3Channels
	}
	return 0, 0
}

// noClose adapts an *os.File whose lifetime is managed by the caller (probeHeader
// defers f.Close) to the io.ReadCloser the decoders expect, without
// double-closing. It preserves seekability so the probed decoders open seekable.
type noClose struct{ io.ReadSeeker }

func (noClose) Close() error { return nil }
