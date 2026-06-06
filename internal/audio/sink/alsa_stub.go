//go:build !linux

package audio

import (
	"fmt"
	"runtime"
)

// This stub provides the alsa backend's symbols on non-linux dev hosts so the
// package builds and cross-compiles everywhere. It is an OS portability guard
// only — NOT a capability switch (D12 still holds: on linux every backend is
// compiled in and selected at runtime by the registry). On a non-linux host the
// alsa probe always fails (so Probe never returns it) and newALSASink errors.

// alsaSink is an unconstructable placeholder so the registry's type plumbing and
// the AudioSink assertion compile on non-linux. It is never instantiated here.
type alsaSink struct{}

func (*alsaSink) Start(int, int) error          { return errALSAUnsupported() }
func (*alsaSink) Write([]float32) (int, error)  { return 0, errALSAUnsupported() }
func (*alsaSink) Delay() (int, bool)            { return 0, false }
func (*alsaSink) Close() error                  { return nil }

func errALSAUnsupported() error {
	return fmt.Errorf("alsa: direct-ioctl sink unsupported on %s (linux only)", runtime.GOOS)
}

// newALSASink returns an error on non-linux: the direct kernel ioctl path exists
// only on linux. Callers (Open) propagate the error; Probe never reaches here
// because probeALSA returns false.
func newALSASink(string) (*alsaSink, error) {
	return nil, errALSAUnsupported()
}

// probeALSA always reports "not usable" on non-linux, so Probe never advertises
// the alsa backend off a Linux kernel.
func probeALSA(string) bool { return false }

// probeAlsaLib / newAlsaLibSink mirror the stubs for the shared libasound
// (dlopen) backend: linux-only, never usable elsewhere.
func probeAlsaLib(string) bool { return false }

func newAlsaLibSink(string) (AudioSink, error) {
	return nil, fmt.Errorf("alsalib: libasound sink unsupported on %s (linux only)", runtime.GOOS)
}

var _ AudioSink = (*alsaSink)(nil)
