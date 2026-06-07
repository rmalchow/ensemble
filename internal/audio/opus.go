package audio

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"ensemble/internal/dl"
	"ensemble/internal/stream"
)

// Opus session constants — tied to canonical PCM (§8.1) and the 20 ms frame.
const (
	opusSampleRate = 48000   // Hz; libopus full-band
	opusChannels   = 2       // stereo
	opusFrameSize  = 960     // samples/channel per 20 ms call (3840 B s16le)
	opusBitrate    = 128_000 // 128 kbps (§8.3, set via CTL)
	opusMaxPacket  = 1500    // encode output bound (one frame fits easily)

	opusApplicationAudio = 2049 // OPUS_APPLICATION_AUDIO
	opusSetBitrateReq    = 4002 // OPUS_SET_BITRATE_REQUEST
)

// OpusBitrate is the encoder bitrate in bits/sec (§8.3), exported for operator
// logging at session start.
const OpusBitrate = opusBitrate

var opusSonames = []string{"libopus.so.0", "libopus.so"}

var opusSymbols = []string{
	"opus_encoder_create",
	"opus_encoder_ctl",
	"opus_encode",
	"opus_encoder_destroy",
	"opus_decoder_create",
	"opus_decode",
	"opus_decoder_destroy",
}

// opusFns holds the bound libopus functions for one loaded library handle.
type opusFns struct {
	lib *dl.Lib

	encoderCreate  func(fs int32, channels int32, application int32, err *int32) uintptr
	encoderCtl     func(st uintptr, request int32, value int32) int32
	encode         func(st uintptr, pcm *int16, frameSize int32, data *byte, maxData int32) int32
	encoderDestroy func(st uintptr)

	decoderCreate  func(fs int32, channels int32, err *int32) uintptr
	decode         func(st uintptr, data *byte, len int32, pcm *int16, frameSize int32, decodeFEC int32) int32
	decoderDestroy func(st uintptr)
}

// loadOpus loads libopus and binds the symbols. Returns dl.ErrUnavailable when
// the library or any symbol is missing.
func loadOpus() (*opusFns, error) {
	lib, err := dl.Open(opusSonames, opusSymbols)
	if err != nil {
		return nil, err
	}
	f := &opusFns{lib: lib}
	lib.Func(&f.encoderCreate, "opus_encoder_create")
	lib.Func(&f.encoderCtl, "opus_encoder_ctl")
	lib.Func(&f.encode, "opus_encode")
	lib.Func(&f.encoderDestroy, "opus_encoder_destroy")
	lib.Func(&f.decoderCreate, "opus_decoder_create")
	lib.Func(&f.decode, "opus_decode")
	lib.Func(&f.decoderDestroy, "opus_decoder_destroy")
	return f, nil
}

// OpusEncoder wraps a C-side OpusEncoder* obtained via internal/dl. Owned by
// exactly one goroutine; not safe for concurrent use.
type OpusEncoder struct {
	fns    *opusFns
	st     uintptr
	buf    []byte
	closed bool
}

// NewOpusEncoder loads libopus, creates a 48 kHz / 2 ch / AUDIO encoder, and
// sets 128 kbps. Returns dl.ErrUnavailable when libopus is unloadable.
func NewOpusEncoder() (*OpusEncoder, error) {
	fns, err := loadOpus()
	if err != nil {
		return nil, err
	}
	var cerr int32
	st := fns.encoderCreate(opusSampleRate, opusChannels, opusApplicationAudio, &cerr)
	if st == 0 || cerr != 0 {
		fns.lib.Close()
		return nil, fmt.Errorf("%w: opus_encoder_create status %d", ErrBadMedia, cerr)
	}
	if rc := fns.encoderCtl(st, opusSetBitrateReq, opusBitrate); rc != 0 {
		fns.encoderDestroy(st)
		fns.lib.Close()
		return nil, fmt.Errorf("%w: opus set bitrate status %d", ErrBadMedia, rc)
	}
	e := &OpusEncoder{fns: fns, st: st, buf: make([]byte, opusMaxPacket)}
	runtime.SetFinalizer(e, (*OpusEncoder).Close)
	return e, nil
}

// Encode compresses exactly one canonical frame (stream.FrameBytes). The
// returned slice aliases the encoder's reused buffer (valid until the next
// Encode); the caller copies before fan-out.
func (e *OpusEncoder) Encode(pcm []byte) ([]byte, error) {
	if len(pcm) < stream.FrameBytes {
		return nil, fmt.Errorf("%w: opus encode short frame %d", ErrBadMedia, len(pcm))
	}
	samples := unsafe.Slice((*int16)(unsafe.Pointer(&pcm[0])), opusFrameSize*opusChannels)
	n := e.fns.encode(e.st, &samples[0], opusFrameSize, &e.buf[0], int32(len(e.buf)))
	runtime.KeepAlive(pcm)
	if n < 0 {
		return nil, fmt.Errorf("%w: opus_encode status %d", ErrBadMedia, n)
	}
	return e.buf[:n], nil
}

// Close destroys the C encoder once; idempotent.
func (e *OpusEncoder) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	runtime.SetFinalizer(e, nil)
	if e.st != 0 {
		e.fns.encoderDestroy(e.st)
		e.st = 0
	}
	return e.fns.lib.Close()
}

// OpusDecoder mirrors OpusEncoder for the receive path. Owned by exactly one
// goroutine; not concurrent-safe.
type OpusDecoder struct {
	fns    *opusFns
	st     uintptr
	buf    []byte
	closed bool
}

// NewOpusDecoder loads libopus and creates a 48 kHz / 2 ch decoder.
func NewOpusDecoder() (*OpusDecoder, error) {
	fns, err := loadOpus()
	if err != nil {
		return nil, err
	}
	var cerr int32
	st := fns.decoderCreate(opusSampleRate, opusChannels, &cerr)
	if st == 0 || cerr != 0 {
		fns.lib.Close()
		return nil, fmt.Errorf("%w: opus_decoder_create status %d", ErrBadMedia, cerr)
	}
	d := &OpusDecoder{fns: fns, st: st, buf: make([]byte, stream.FrameBytes)}
	runtime.SetFinalizer(d, (*OpusDecoder).Close)
	return d, nil
}

// Decode expands one opus packet back to exactly one canonical frame. No PLC
// (D33): Decode is only called with a real packet.
func (d *OpusDecoder) Decode(packet []byte) ([]byte, error) {
	if len(packet) == 0 {
		return nil, fmt.Errorf("%w: opus decode empty packet", ErrBadMedia)
	}
	out := unsafe.Slice((*int16)(unsafe.Pointer(&d.buf[0])), opusFrameSize*opusChannels)
	n := d.fns.decode(d.st, &packet[0], int32(len(packet)), &out[0], opusFrameSize, 0)
	runtime.KeepAlive(packet)
	if n < 0 {
		return nil, fmt.Errorf("%w: opus_decode status %d", ErrBadMedia, n)
	}
	if int(n) != opusFrameSize {
		return nil, fmt.Errorf("%w: opus_decode returned %d samples", ErrBadMedia, n)
	}
	return d.buf[:stream.FrameBytes], nil
}

// Close destroys the C decoder once; idempotent.
func (d *OpusDecoder) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	runtime.SetFinalizer(d, nil)
	if d.st != 0 {
		d.fns.decoderDestroy(d.st)
		d.st = 0
	}
	return d.fns.lib.Close()
}

// opusAvailable caches the libopus load probe (D3).
var (
	opusProbeOnce sync.Once
	opusProbeOK   bool
)

// OpusAvailable reports whether libopus can be loaded on this host (D3). Cached.
func OpusAvailable() bool {
	opusProbeOnce.Do(func() {
		lib, err := dl.Open(opusSonames, opusSymbols)
		if err == nil {
			opusProbeOK = true
			lib.Close()
		}
	})
	return opusProbeOK
}
