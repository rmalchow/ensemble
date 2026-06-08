package audio

import (
	"context"
	"errors"
	"os/exec"
	"strings"
)

// Scheme names (§6.1).
const (
	SchemeFile  = "file"  // file:<rel path> or a bare path under MEDIA_DIR
	SchemeHTTP  = "http"  // http:// or https:// remote stream/file
	SchemeInput = "input" // input: local capture (line-in/mic), exec-captured
)

// Errors.
var (
	// ErrUnsupportedScheme — Open got a URI whose scheme has no registered source.
	ErrUnsupportedScheme = errors.New("audio: unsupported source scheme")
	// ErrUnsupportedFormat — a decodable source whose media format is not wav/mp3/flac.
	ErrUnsupportedFormat = errors.New("audio: unsupported media format")
	// ErrBadMedia wraps any decoder/parse/transport failure on otherwise-known media.
	ErrBadMedia = errors.New("audio: cannot open media")
	// ErrTraversal — a file URI resolving outside MEDIA_DIR (§6).
	ErrTraversal = errors.New("audio: path escapes media dir")
)

// Source is the one contract every media source satisfies (§6.1, D26). It
// produces canonical PCM (§8.1) one frame at a time and is owned by exactly one
// goroutine — H's release ticker. Not safe for concurrent use.
type Source interface {
	// ReadFrame fills dst[:stream.FrameBytes] with exactly one canonical 20 ms
	// frame and returns nil; or returns io.EOF (no bytes written) when the
	// session has ended (D9). Live sources never return io.EOF except after
	// Close — momentary underflow yields a silence frame and nil.
	ReadFrame(dst []byte) error
	// Live reports the pacing class: false = pull-paced (file, EOF-terminated),
	// true = live-paced (http/input, never-EOF, underflow→silence).
	Live() bool
	// Close releases the file/decoder/connection/subprocess. Idempotent.
	Close() error
}

// constructor builds a Source for a parsed URI.
type constructor func(ctx context.Context, uri, mediaDir string) (Source, error)

var registry = map[string]constructor{
	SchemeFile:      openFile,
	SchemeHTTP:      openHTTP, // serves both http: and https:
	SchemeInput:     openInput,
	SchemeCalibrate: openCalibrate,
}

// Open parses uri's scheme and constructs the matching Source (D26). For a bare
// path with no scheme it assumes SchemeFile. mediaDir bounds file resolution.
func Open(ctx context.Context, uri, mediaDir string) (Source, error) {
	scheme := schemeOf(uri)
	c, ok := registry[scheme]
	if !ok {
		return nil, ErrUnsupportedScheme
	}
	return c(ctx, uri, mediaDir)
}

// schemeOf extracts the (normalized) scheme bucket of a URI. A bare path with no
// "scheme:" prefix is treated as file; https maps to the http bucket.
func schemeOf(uri string) string {
	i := strings.Index(uri, ":")
	if i <= 0 {
		return SchemeFile
	}
	s := strings.ToLower(uri[:i])
	switch s {
	case "file":
		return SchemeFile
	case "http", "https":
		return SchemeHTTP
	case "input":
		return SchemeInput
	case "calibrate":
		return SchemeCalibrate
	}
	// A bare relative path that happens to contain a colon is rare; if the
	// prefix isn't a known scheme word, treat it as an unsupported scheme so
	// callers get a clear 4xx rather than a confusing file lookup.
	return s
}

// captureBinaries, in probe order, mirror E's exec playback discovery (§6.1).
var captureBinaries = []string{"pw-record", "arecord"}

// Schemes returns the media-source schemes this build can serve (§1, §6.1).
// file and http are always present; input is reported only when a capture
// binary is on $PATH.
func Schemes() []string {
	out := []string{SchemeFile, SchemeHTTP}
	if findCaptureBinary() != "" {
		out = append(out, SchemeInput)
	}
	return out
}

// findCaptureBinary returns the first capture tool found on $PATH, or "".
func findCaptureBinary() string {
	for _, b := range captureBinaries {
		if p, err := exec.LookPath(b); err == nil {
			return p
		}
	}
	return ""
}
