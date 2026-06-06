//go:build opus

package codec

// libopus runtime binding via purego/dlopen (P5.2 §5.5, A.11/R7). NO cgo and NO
// C toolchain at build time — purego calls the dynamic linker at RUNTIME. The
// library is opened ONCE (sync.Once) and its symbols resolved into a package
// table; every opusCodec instance reuses the resolved entry points. Graceful
// absence: if libopus.so.0 is not loadable, or any required symbol is missing,
// the table's `ok` stays false, OpusRuntimeAvailable() returns false, and
// NewOpus returns ErrUnsupportedCodec — so the rest of the system treats a
// box-without-libopus identically to the default (no-opus-tag) build.

import (
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Opus ABI constants (stable; from opus_defines.h). Inlined as Go consts per
// P5.2 §5.5/§9 Q5 — libopus's ABI is frozen, so these never change.
const (
	opusApplicationAudio = 2049 // OPUS_APPLICATION_AUDIO (music/general, doc 05 §5.4.2)

	opusSetBitrateRequest = 4002 // OPUS_SET_BITRATE_REQUEST
	opusSetVBRRequest     = 4006 // OPUS_SET_VBR_REQUEST
	opusResetState        = 4028 // OPUS_RESET_STATE (CTL with no payload arg)

	opusOK = 0 // OPUS_OK
)

// candidateLibs are the soname variants tried in order; the versioned soname is
// what ships on a stripped runtime (no -dev package needed).
var candidateLibs = []string{"libopus.so.0", "libopus.so"}

// opusLib is the resolved libopus symbol table, populated once by loadOpus().
type opusLib struct {
	handle uintptr
	ok     bool

	encoderCreate  uintptr // opus_encoder_create(Fs, channels, application, *err) -> *OpusEncoder
	encodeFloat    uintptr // opus_encode_float(st, *pcm, frame_size, *data, max_bytes) -> int(bytes|err)
	encoderCtl     uintptr // opus_encoder_ctl(st, request, ...) -> int
	encoderDestroy uintptr // opus_encoder_destroy(st)

	decoderCreate  uintptr // opus_decoder_create(Fs, channels, *err) -> *OpusDecoder
	decodeFloat    uintptr // opus_decode_float(st, *data, len, *pcm, frame_size, decode_fec) -> int(samples|err)
	decoderDestroy uintptr // opus_decoder_destroy(st)
}

var (
	loadOnce sync.Once
	lib      opusLib
)

// loadOpus opens libopus once and resolves the minimal symbol set (P5.2 §5.5).
// On any failure it leaves lib.ok == false (fail-soft, never panics).
func loadOpus() {
	var handle uintptr
	var err error
	for _, name := range candidateLibs {
		handle, err = purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err == nil && handle != 0 {
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

	l := opusLib{handle: handle}
	var ok1, ok2, ok3, ok4, ok5, ok6, ok7 bool
	l.encoderCreate, ok1 = sym("opus_encoder_create")
	l.encodeFloat, ok2 = sym("opus_encode_float")
	l.encoderCtl, ok3 = sym("opus_encoder_ctl")
	l.encoderDestroy, ok4 = sym("opus_encoder_destroy")
	l.decoderCreate, ok5 = sym("opus_decoder_create")
	l.decodeFloat, ok6 = sym("opus_decode_float")
	l.decoderDestroy, ok7 = sym("opus_decoder_destroy")

	l.ok = ok1 && ok2 && ok3 && ok4 && ok5 && ok6 && ok7
	lib = l
}

// opusAvailableBinding reports whether libopus loaded and every required symbol
// resolved. Cheap after the first call (sync.Once-cached).
func opusAvailableBinding() bool {
	loadOnce.Do(loadOpus)
	return lib.ok
}

// --- thin typed wrappers over purego.SyscallN (one C call each) ---
//
// Pointers to Go slices are passed for the duration of ONE call only; libopus
// copies the samples and does not retain the backing arrays (P5.2 §5.5), so no
// runtime.Pinner/escape gymnastics are needed on the hot path.

func opusEncoderCreate(fs, channels, application int) (uintptr, int) {
	var cerr int32
	r, _, _ := purego.SyscallN(lib.encoderCreate,
		uintptr(fs), uintptr(channels), uintptr(application),
		uintptr(unsafe.Pointer(&cerr)))
	return r, int(cerr)
}

func opusEncoderDestroy(st uintptr) {
	if st != 0 {
		purego.SyscallN(lib.encoderDestroy, st)
	}
}

func opusEncoderCtl1(st uintptr, request int, value int32) int {
	r, _, _ := purego.SyscallN(lib.encoderCtl, st, uintptr(request), uintptr(value))
	return int(int32(r))
}

func opusEncoderCtl0(st uintptr, request int) int {
	r, _, _ := purego.SyscallN(lib.encoderCtl, st, uintptr(request))
	return int(int32(r))
}

func opusEncodeFloat(st uintptr, pcm []float32, frameSize int, out []byte) int {
	r, _, _ := purego.SyscallN(lib.encodeFloat,
		st,
		uintptr(unsafe.Pointer(&pcm[0])),
		uintptr(frameSize),
		uintptr(unsafe.Pointer(&out[0])),
		uintptr(len(out)))
	return int(int32(r))
}

func opusDecoderCreate(fs, channels int) (uintptr, int) {
	var cerr int32
	r, _, _ := purego.SyscallN(lib.decoderCreate,
		uintptr(fs), uintptr(channels),
		uintptr(unsafe.Pointer(&cerr)))
	return r, int(cerr)
}

func opusDecoderDestroy(st uintptr) {
	if st != 0 {
		purego.SyscallN(lib.decoderDestroy, st)
	}
}

// opusDecodeFloat decodes one frame into pcm (frameSize samples/channel). When
// data is nil/empty it requests PLC (libopus synthesizes one frame) — that is
// the ConcealLoss path (doc 05 §5.6.3). decodeFEC is 0 (no in-band FEC here;
// loss recovery is the XOR/dup layer, A.12).
func opusDecodeFloat(st uintptr, data []byte, pcm []float32, frameSize int) int {
	var dataPtr uintptr
	var dataLen int
	if len(data) > 0 {
		dataPtr = uintptr(unsafe.Pointer(&data[0]))
		dataLen = len(data)
	}
	r, _, _ := purego.SyscallN(lib.decodeFloat,
		st,
		dataPtr,
		uintptr(dataLen),
		uintptr(unsafe.Pointer(&pcm[0])),
		uintptr(frameSize),
		0)
	return int(int32(r))
}
