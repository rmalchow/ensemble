# D — audio source & decoding

Source of truth: [docs/README.md](../README.md) §6, §8.1, §8.2; contracts:
[docs/arch/S-skeleton.md](S-skeleton.md). This piece owns **`internal/audio/`**
only. It depends on S for the canonical PCM constants
(`stream.FrameBytes`, `stream.FrameSamples`, `stream.SampleRate`,
`stream.Channels`) and on three decode libs (`hajimehoshi/go-mp3`,
`mewkiz/flac`) plus a hand-rolled WAV reader. Nothing else imports `internal/audio`
except the group engine (H), which calls `Open` and pulls frames.

Design stance: **smallest thing that satisfies the spec.** One concrete
`FrameReader` type, one `Open` dispatch by extension, three thin per-format
decoder adapters feeding a single shared "samples → canonical 20 ms frames"
pipeline (resample + mono-dup + framing). No interfaces beyond the one decode
source adapter (three implementations exist, so an interface is justified).

---

## 1. Package / file layout

```
internal/audio/audio.go        Open(path) dispatch by extension; FrameReader type;
                               ReadFrame/Close/Format; pcmSource interface; the
                               framing+resample+mono-dup pipeline; errors.
internal/audio/wav.go          hand-rolled RIFF/WAVE reader (PCM s16/s24/u8 +
                               IEEE float32), streaming sample source. ~110 lines.
internal/audio/mp3.go          go-mp3 adapter: wraps *mp3.Decoder as a pcmSource.
internal/audio/flac.go         mewkiz/flac adapter: wraps *flac.Stream as a pcmSource.
internal/audio/resample.go     linear-interpolation resampler (any rate → 48000),
                               stereo-interleaved float-free fixed-point/int math.

internal/audio/audio_test.go     Open dispatch, end-to-end framing, EOF, errors.
internal/audio/wav_test.go       hand-rolled WAV parse: fixtures written by test code.
internal/audio/resample_test.go  ratio/length/passthrough/interpolation checks.
internal/audio/fixtures_test.go  test helpers: writeWAV(...) generators (s16/u8/float),
                                 mp3/flac fixtures gated on build-time presence.
```

Note: `stream.FrameBytes`/`FrameSamples`/`SampleRate`/`Channels` come from
`internal/stream/wire.go` (S). D never redefines them.

---

## 2. Concrete Go API

This is the contract H codes against. Only `Open`, `FrameReader`,
`ReadFrame`, `Close`, `Format`, `Format*` consts, and the error sentinels are
exported. Everything else (`pcmSource`, `wavSource`, `mp3Source`, `flacSource`,
`resampler`, framing) is unexported.

```go
package audio

import (
	"errors"
	"io"
)

// Format identifies the container/codec detected from the file extension.
type Format string

const (
	FormatWAV  Format = "wav"
	FormatMP3  Format = "mp3"
	FormatFLAC Format = "flac"
)

// Errors.
var (
	// ErrUnsupportedFormat is returned by Open for an unknown extension.
	ErrUnsupportedFormat = errors.New("audio: unsupported format")
	// ErrBadFile wraps any decoder/parse failure on otherwise-known formats.
	ErrBadFile = errors.New("audio: cannot decode file")
)

// FrameReader yields canonical PCM frames (§8.1): 48 kHz, stereo, s16le,
// 20 ms = stream.FrameSamples (960) per channel = stream.FrameBytes (3840 B).
// Every frame except possibly the last is exactly FrameBytes; the final frame
// is zero-padded to FrameBytes (silence) so all frames are full-length.
//
// Not safe for concurrent use; one goroutine owns a FrameReader.
type FrameReader struct {
	// unexported: src pcmSource, rs *resampler, format Format, buffered samples,
	// frameIndex uint64, done bool, closer io.Closer.
}

// Open detects the format from path's extension (case-insensitive: .wav .mp3
// .flac), opens and prepares a streaming decoder, and returns a FrameReader
// positioned at sample 0. The file is held open until Close. Returns
// ErrUnsupportedFormat for unknown extensions and ErrBadFile (wrapped) for
// parse/decode setup failures (truncated header, unsupported sub-format, etc.).
func Open(path string) (*FrameReader, error)

// ReadFrame fills dst[:stream.FrameBytes] with the next canonical frame and
// returns the number of *audio* (non-padding) bytes that were real samples
// (always FrameBytes except possibly the final frame), the frame's 0-based
// index, and an error. At true end of stream it returns io.EOF *after* the
// last (possibly padded) frame has been delivered — i.e. the last frame is
// returned with err == nil, then the next call returns (0, idx, io.EOF).
//
// dst must have len >= stream.FrameBytes; ReadFrame never allocates the frame.
// On a mid-stream decode error it returns a wrapped ErrBadFile.
func (fr *FrameReader) ReadFrame(dst []byte) (n int, index uint64, err error)

// Format returns the detected source format.
func (fr *FrameReader) Format() Format

// Close releases the underlying file/decoder. Idempotent; always safe.
func (fr *FrameReader) Close() error
```

### 2.1 Internal source adapter (unexported, the only interface here)

Three formats produce samples differently (byte stream vs. frame-of-int32),
so one tiny pull interface unifies them. It emits **interleaved int16 samples
at the source's native rate and channel count** — never canonical yet. The
pipeline above it does resample → mono-dup → framing.

```go
// pcmSource is a native-rate, native-channel PCM sample producer. It is the
// single seam over the three decode libs. Implementations: wavSource (wav.go),
// mp3Source (mp3.go), flacSource (flac.go).
type pcmSource interface {
	// info reports the source's native sample rate (Hz) and channel count
	// (1 or 2; >2 is rejected at Open as ErrBadFile). Valid after construction.
	info() (sampleRate, channels int)

	// read appends up to a bounded number of interleaved int16 samples to dst
	// and returns the grown slice. Interleaving is L,R,L,R for stereo, or one
	// sample per frame for mono. Returns io.EOF together with any final samples
	// (Go convention: data + io.EOF allowed) or on a subsequent empty call.
	// Any other error is a decode failure (wrapped ErrBadFile by the caller).
	read(dst []int16) ([]int16, error)

	// Close releases the source.
	io.Closer
}
```

`mp3Source` (go-mp3) and `wavSource` byte streams convert s16le bytes → int16
in `read`; `flacSource` converts each frame's `Subframes[ch].Samples` (int32,
correlation already undone by `ParseNext`) down to int16, scaling by
`BitsPerSample` (`>>(bps-16)` when bps>16, `<<(16-bps)` when bps<16).

### 2.2 Resampler (unexported)

```go
// resampler does linear interpolation from inRate to 48000 on interleaved
// stereo int16 (it runs *after* mono→stereo duplication, so it is always
// 2-channel). Pass-through (no allocation copy logic) when inRate == 48000.
// It keeps the last input sample-frame across calls so block boundaries
// interpolate seamlessly (no clicks at 20 ms edges).
type resampler struct {
	inRate   int
	pos      int64 // fixed-point input position, 32.32; advances by inRate/48000 per out frame
	// last L,R input sample-frame carried across read() calls; primed bool.
}

func newResampler(inRate int) *resampler

// process consumes interleaved-stereo int16 input and appends interleaved-stereo
// int16 output (at 48000) to out, returning the grown slice. atEOF=true flushes
// the tail (emits remaining interpolated output up to the last input frame).
func (r *resampler) process(in []int16, atEOF bool, out []int16) []int16
```

Implementation: maintain `pos` as a 32.32 fixed-point cursor in *input
sample-frame* units, step `= (inRate << 32) / 48000` per output frame. For each
output frame, take `i = pos>>32`, `frac = pos & 0xffffffff`, linearly blend
input frames `i` and `i+1`; emit while `i+1` is within the available input
(carry the boundary frame to the next call). EOF flush blends against a
duplicate of the last frame. All integer math; no float, no cgo.

---

## 3. Control flow, goroutines, locking

**Single-goroutine, no locks, no channels.** A `FrameReader` is a pull-based
pipeline owned by one caller (the group master's release ticker in H). The
spec's real-time pacing (ticker, lead, pts) lives in H, not here — D only
decodes and frames on demand.

### Startup (`Open`)
1. Lowercase the extension; dispatch to `newWavSource` / `newMp3Source` /
   `newFlacSource`. Unknown → `ErrUnsupportedFormat`.
2. Open the file (`os.Open`); construct the format source; read native
   `(rate, channels)` via `info()`. Reject `channels < 1 || channels > 2`
   and `rate <= 0` as `ErrBadFile`. On any setup error close the file and
   return wrapped `ErrBadFile`.
3. Build the `resampler` if `rate != 48000` (else mark pass-through).
4. Pre-allocate the internal sample scratch buffers; `frameIndex = 0`.

### Steady state (`ReadFrame`)
Loop until the internal canonical-sample buffer holds ≥ `FrameSamples*2`
(960×2 = 1920 int16) **or** source EOF:
1. `src.read(scratch)` → native interleaved int16 (+ maybe io.EOF).
2. **Mono-dup**: if `channels == 1`, expand each sample `s` → `s, s`.
3. **Resample**: if not pass-through, `rs.process(stereo, atEOF, canonBuf)`;
   else append stereo directly.
4. Accumulate into the canonical buffer.

Then slice one frame:
- If buffer ≥ 1920 int16: encode 960×2 samples little-endian into `dst`,
  drop them from the buffer, `n = FrameBytes`, return `(FrameBytes, idx, nil)`,
  `idx++`.
- If buffer < 1920 **and** source EOF and buffer non-empty: zero-pad to 1920,
  emit the final (padded) frame, mark `done`, `n` = real bytes, return nil err.
- If buffer empty and EOF: return `(0, idx, io.EOF)`.

No goroutine is spawned. Back-pressure is implicit: H calls `ReadFrame` at
20 ms cadence; D does only the work needed for the next frame.

### Shutdown (`Close`)
Closes the underlying `*os.File`/decoder once; sets a `closed` flag so repeat
calls and post-close `ReadFrame` return `io.EOF`/`os.ErrClosed` cleanly.

---

## 4. Edge cases & failure handling

- **Unknown / missing extension (§6)**: `Open` → `ErrUnsupportedFormat`
  (caller maps to an API 4xx). Extension match is case-insensitive
  (`.WAV` == `.wav`); detection is by extension only, per piece scope — no
  content sniffing.
- **Mono source (§8.1)**: duplicated to stereo *before* resampling, so the
  resampler is always 2-channel and the dup is cheap.
- **>2 channels**: rejected at `Open` as `ErrBadFile`. v1 has no downmix
  matrix (out of scope; spec only requires mono-dup + rate convert).
- **Native rate == 48000 (§8.1)**: resampler is pass-through (identity), no
  interpolation, bit-exact frames — the common case for 48 k WAV stays cheap.
- **Arbitrary rate (44100, 22050, 96000…) (§8.1)**: linear interpolation to
  48000. Carrying the last input frame across `read()` calls prevents clicks at
  20 ms boundaries. Length: output frames ≈ `inSamples*48000/inRate`; the final
  flush emits the tail.
- **Final partial frame (§8.2)**: the file rarely ends on a 20 ms boundary; the
  last frame is **zero-padded to FrameBytes** (trailing silence) so every wire
  frame is full-length and the pts math in H (`sessionStart+index·20ms`) stays
  integral. `ReadFrame` reports the real byte count via `n` for callers that
  care, but H sends full 3840 B frames.
- **EOF semantics (§8.6)**: last real/padded frame returns `err == nil`; the
  *next* call returns `(0, idx, io.EOF)`. This lets H detect natural end and
  bump the generation/clear playback status. go-mp3's `UnexpectedEOF` and
  flac's truncation-at-stream-end are normalized to `io.EOF` (graceful end),
  not `ErrBadFile`.
- **Mid-stream decode corruption**: a non-EOF decoder error becomes wrapped
  `ErrBadFile`; H stops the session and reports it. We do **not** try to skip
  bad frames (keep it simple); a corrupt file just ends early with an error.
- **WAV sub-formats**: hand-rolled reader supports PCM `u8`, `s16`, `s24`
  (down-shifted to s16), and IEEE float32 (`fmt` tag 3, clamped to s16);
  anything else (a-law, μ-law, >32-bit, exotic tags) → `ErrBadFile`. It scans
  chunks for `fmt ` then `data`, tolerating intervening chunks (`LIST`, `fact`).
  Truncated `data` is treated as EOF at the last whole sample-frame.
- **FLAC bit depth**: 16-bit passes through; 24/20/8-bit scaled to s16 by the
  per-sample shift; correlation (mid/side/left-side/side-right) is already
  resolved by `flac.Stream.ParseNext`, so D reads plain L/R int32.
- **mp3 always-stereo**: go-mp3 emits 2-channel s16le regardless of source
  channel count; `mp3Source.info()` reports `channels = 2` so no mono-dup runs
  (correct — the lib already duplicated).
- **Empty / zero-sample file**: first `ReadFrame` returns `(0, 0, io.EOF)`;
  no panic, no padded silent frame.
- **Close-during-read / double Close**: guarded by a `closed` flag; idempotent.
- **No allocation on the hot path**: `ReadFrame` writes into the caller's `dst`
  and reuses internal scratch slices across calls; only initial buffers are
  allocated at `Open`.

---

## 5. Test plan

All tests are hardware-free: WAV fixtures are synthesized in-test;
`internal/stream` constants are imported, never duplicated. mp3/flac decode
tests are skipped (`t.Skip`) when no committed fixture is present, per
IMPLEMENTATION.md D ("generate a wav fixture programmatically, skip
codec-specific tests if no fixture").

`internal/audio/audio_test.go`
- `TestOpenDispatchByExtension` — `.wav/.mp3/.flac` pick the right Format;
  `.WAV` (uppercase) works; `.ogg`/none → `ErrUnsupportedFormat`.
- `TestOpenMissingFile` — non-existent path → error (not panic).
- `TestReadFrame48kStereoPassthrough` — 48 k stereo WAV: every frame is
  FrameBytes, bytes match source exactly (no resample drift).
- `TestReadFrameMonoDuplicated` — mono WAV: each output frame has L==R for
  every sample-frame.
- `TestReadFrameResamples44kTo48k` — 44.1 k WAV of known length: output frame
  count ≈ `inFrames*48000/44100` (±1), no panic, monotonic.
- `TestFinalFramePadded` — file length not a 20 ms multiple: last frame is
  FrameBytes, `n < FrameBytes`, tail bytes are zero; next call → io.EOF.
- `TestEOFAfterLastFrame` — last real frame returns nil err; subsequent call
  returns `(0, idx, io.EOF)`; index increments by exactly 1 per frame.
- `TestEmptyWAVImmediateEOF` — zero-sample data chunk → first ReadFrame io.EOF.
- `TestReadFrameShortDstRejected` — `dst` shorter than FrameBytes → error/panic
  per contract (documented), never silent corruption.
- `TestCloseIdempotent` — double Close ok; ReadFrame after Close → io.EOF.
- `TestMidStreamCorruptionIsBadFile` — truncated/garbled fixture → ErrBadFile
  (not io.EOF) when corruption is mid-stream.

`internal/audio/wav_test.go`
- `TestWAVParseS16` — synthesized s16 PCM WAV: rate/channels/samples correct.
- `TestWAVParseU8` — 8-bit unsigned scaled to s16 (0x80 → 0).
- `TestWAVParseS24` — 24-bit down-shifted to s16, sign preserved.
- `TestWAVParseFloat32` — IEEE float (fmt=3) clamped to s16 range.
- `TestWAVSkipsAuxChunks` — `LIST`/`fact` chunks before `data` are skipped.
- `TestWAVTruncatedDataIsEOF` — data shorter than declared → EOF at last whole
  sample-frame, no error.
- `TestWAVRejectsALaw` — fmt tag 6/7 (a-law/μ-law) → ErrBadFile.
- `TestWAVRejectsMissingDataChunk` — no `data` chunk → ErrBadFile.

`internal/audio/resample_test.go`
- `TestResamplePassthrough48k` — inRate 48000 → output == input bit-exact.
- `TestResampleHalfRate` — 24000→48000 doubles length (±1), interpolated
  midpoints equal the average of neighbors.
- `TestResampleUpDownRatios` — 44100/96000/22050 produce expected lengths
  within ±1 of `n*48000/inRate`.
- `TestResampleBlockBoundaryContinuity` — feeding input in two chunks yields
  the same output as one chunk (no boundary click; carry-frame works).
- `TestResampleEOFFlush` — `atEOF` flush emits the trailing tail and stops.
- `TestResampleConstantSignal` — a DC/constant input stays constant through
  interpolation (no overshoot).

`internal/audio/fixtures_test.go` (helpers, not tests)
- `writeWAVs16(t, rate, ch, samples)` / `writeWAVu8` / `writeWAVfloat32` —
  build minimal RIFF/WAVE byte fixtures in a `t.TempDir()` file.
- `genTone(rate, ch, freq, dur)` — deterministic int16 tone for assertions.
- `maybeFixture(t, name)` — returns path + `skip=true` if an optional mp3/flac
  testdata file is absent (keeps the suite green without binary fixtures).

`internal/audio/audio_test.go` (optional, fixture-gated)
- `TestDecodeMP3Fixture` — if `testdata/tone.mp3` present: decodes to ~expected
  duration of 48 k stereo frames; else `t.Skip`.
- `TestDecodeFLACFixture` — if `testdata/tone.flac` present: 16-bit FLAC decodes
  to canonical frames matching the source tone within tolerance; else `t.Skip`.

---

## 6. Notes for the integrator / downstream

- D imports `internal/stream` only for the PCM constants; it does **not** import
  the wire `Header`/`Mux` (framing for the wire is G's job — D emits raw PCM
  payloads, G prepends the header).
- Capabilities `formats: ["wav","mp3","flac"]` (§1) is reported by the config/
  node layer, not D; D just needs to actually decode all three. If a build ever
  drops a decoder, that's a separate concern — v1 always has all three.
- `Open`/`ReadFrame` are blocking-free of I/O surprises: they read from a local
  file only. Network media is out of scope (§6: local `MEDIA_DIR`).
- go.mod adds `github.com/hajimehoshi/go-mp3` and `github.com/mewkiz/flac`
  (both already in the allowed-deps closure). WAV is hand-rolled (~110 lines),
  so `go-audio/wav` is **not** taken as a dependency — fewer deps, matches the
  IMPLEMENTATION.md preference ("prefer hand-rolled if under ~120 lines").
```
