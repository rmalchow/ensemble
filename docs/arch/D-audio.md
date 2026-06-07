# D — media sources (file / http / input)

Source of truth: [docs/README.md](../README.md) §6, §6.1, §8.1; integrator
decisions [docs/arch/DECISIONS.md](DECISIONS.md) **D9** (EOF semantics),
**D26** (scheme-keyed source factory, pull- vs live-paced); shared contracts
[docs/arch/S-skeleton.md](S-skeleton.md). This piece owns **`internal/audio/`**
only.

It depends on S for the canonical-PCM constants (`stream.FrameBytes`,
`stream.FrameSamples`, `stream.SampleRate`, `stream.Channels`,
`stream.FrameDuration`) and on two decode libs (`hajimehoshi/go-mp3`,
`mewkiz/flac`) plus a hand-rolled WAV reader. Nothing imports `internal/audio`
except the group engine (H), which calls `Open` and pulls frames via the
`Source` contract.

Design stance: **smallest thing that satisfies the spec.** One exported
`Source` interface, one `Open(uri)` scheme dispatcher, three media-source
implementations (`fileSource`, `httpSource`, `inputSource`), and **one shared
decode pipeline** (decoder adapter → mono-dup → linear resample → 20 ms
framing) that all three reuse. The only structural difference between the
three is *where the bytes come from and how the caller is paced* — pull-paced
(file: decode-ahead, real EOF) vs live-paced (http/input: never EOF, underflow
yields silence without stalling, D26).

The previous round's decoder + resampler design is kept verbatim where still
valid; the change is that decode is now wrapped behind the `Source` contract
and reused by the http source, and two new live sources (http, input) are
added.

---

## 1. Package / file layout

```
internal/audio/source.go       Source interface; Open(uri) scheme dispatch;
                               scheme registry; Schemes(); errors. ~120 lines.
internal/audio/decode.go       the shared decode pipeline: decoder dispatch by
                               format, mono-dup + resample + 20 ms framing into
                               a *framer producing canonical frames. ~160 lines.
internal/audio/file.go         fileSource: os.Open under MEDIA_DIR (traversal
                               guard) → framer; pull-paced; real io.EOF. ~80 lines.
internal/audio/http.go         httpSource: http.Get → streaming body → framer;
                               live-paced (never EOF); bounded read-ahead +
                               release-paced silence on underflow. ~150 lines.
internal/audio/input.go        inputSource: exec-capture (pw-record/arecord raw
                               s16le) → framer; live-paced; mirrors E's exec
                               sink backend probe. ~130 lines.
internal/audio/wav.go          hand-rolled RIFF/WAVE reader (PCM u8/s16/s24 +
                               IEEE float32), streaming sampleReader. ~110 lines.
internal/audio/mp3.go          go-mp3 adapter: wraps *mp3.Decoder. ~40 lines.
internal/audio/flac.go         mewkiz/flac adapter: wraps *flac.Stream. ~60 lines.
internal/audio/resample.go     linear-interpolation resampler (any rate → 48000),
                               stereo-interleaved fixed-point int math. ~80 lines.

internal/audio/source_test.go    scheme dispatch, Open errors, capability list,
                                 EOF contract (D9) end-to-end, live no-stall.
internal/audio/file_test.go      traversal guard, file pull-paced EOF.
internal/audio/http_test.go      httptest server: content-type/extension
                                 dispatch, live-paced never-EOF, underflow
                                 silence, bounded read-ahead, Close cancels.
internal/audio/input_test.go     fake capture command (exec a tiny helper that
                                 emits raw s16le), live framing, Close kills it.
internal/audio/wav_test.go       hand-rolled WAV parse: fixtures written in-test.
internal/audio/decode_test.go    framing/mono-dup/EOF over synthesized samples.
internal/audio/resample_test.go  ratio/length/passthrough/interpolation checks.
internal/audio/fixtures_test.go  helpers: writeWAV*, genTone, fake-capture exe,
                                 maybeFixture (gate mp3/flac on testdata).
```

`stream.FrameBytes`/`FrameSamples`/`SampleRate`/`Channels`/`FrameDuration`
come from `internal/stream/wire.go` (S). D never redefines them.

---

## 2. Concrete Go API

This is the contract H codes against. Exported surface: the `Source`
interface, `Open`, `Schemes`, the scheme name consts, and the error sentinels.
Everything else (the three source impls, decoder adapters, `framer`,
`resampler`) is unexported.

```go
package audio

import (
	"context"
	"errors"
)

// Scheme names (§6.1). A node's available schemes are reported in its
// capabilities (§1) via Schemes(); H/K assemble the capability list from it.
const (
	SchemeFile  = "file"  // file:<rel path> or a bare path under MEDIA_DIR
	SchemeHTTP  = "http"  // http:// or https:// remote stream/file
	SchemeInput = "input" // input: local capture (line-in/mic), exec-captured
)

// Errors.
var (
	// ErrUnsupportedScheme — Open got a URI whose scheme has no registered
	// source (caller maps to an API 4xx).
	ErrUnsupportedScheme = errors.New("audio: unsupported source scheme")
	// ErrUnsupportedFormat — a decodable source whose media format (extension /
	// content-type) is not wav/mp3/flac.
	ErrUnsupportedFormat = errors.New("audio: unsupported media format")
	// ErrBadMedia wraps any decoder/parse/transport failure on otherwise-known
	// media (truncated header, unsupported sub-format, HTTP non-2xx, etc.).
	ErrBadMedia = errors.New("audio: cannot open media")
	// ErrTraversal — a file URI resolving outside MEDIA_DIR (§6).
	ErrTraversal = errors.New("audio: path escapes media dir")
)

// Source is the one contract every media source satisfies (§6.1, D26). It
// produces canonical PCM (§8.1) one frame at a time and is owned by exactly
// one goroutine — H's release ticker. Not safe for concurrent use.
type Source interface {
	// ReadFrame fills dst[:stream.FrameBytes] with exactly one canonical 20 ms
	// frame (48 kHz stereo s16le, 3840 B) and returns nil; or returns io.EOF
	// (no bytes written) when the session has ended. D9 EOF semantics: the
	// final partial frame of a pull-paced source is zero-padded and returned
	// with err==nil; the *next* call returns io.EOF. dst must have
	// len >= stream.FrameBytes; ReadFrame never allocates the frame.
	//
	// Pacing class (D26) determines underflow behaviour:
	//   - Pull-paced (file): blocks only on local decode work (microseconds);
	//     returns io.EOF at true end of stream.
	//   - Live-paced (http, input): NEVER returns io.EOF. When no real audio is
	//     available within the frame deadline it fills dst with SILENCE and
	//     returns nil, so H's 20 ms ticker keeps a steady seq/pts cadence and
	//     can emit silence without stalling. The session ends only via Close.
	//
	// A genuine, unrecoverable error (mid-stream decode corruption, transport
	// teardown that is not a normal end) is returned wrapped in ErrBadMedia.
	ReadFrame(dst []byte) error

	// Live reports the pacing class: false = pull-paced (file, EOF-terminated),
	// true = live-paced (http/input, never-EOF, underflow→silence). H uses it
	// to decide whether natural EOF can end the session (§6.1, §8.6).
	Live() bool

	// Close releases the file/decoder/connection/subprocess. Idempotent;
	// unblocks any in-flight ReadFrame promptly. Safe to call from another
	// goroutine than the reader (it is how H stops a live source).
	Close() error
}

// Open parses uri's scheme and constructs the matching Source (D26). For a
// bare path with no scheme it assumes SchemeFile. mediaDir bounds file://
// resolution (traversal guard, §6). ctx governs setup *and* the lifetime of
// live sources (http dial / subprocess): cancelling ctx (or Close) tears the
// source down. Returns ErrUnsupportedScheme / ErrUnsupportedFormat /
// ErrBadMedia / ErrTraversal as documented above.
func Open(ctx context.Context, uri, mediaDir string) (Source, error)

// Schemes returns the media-source schemes this build can serve, for the
// capability record (§1, §6.1). It probes the host for input support (a
// pw-record/arecord binary on $PATH) so a node with no capture tool does not
// advertise "input". file and http are always present (pure Go). Order is
// stable: ["file","http"] (+ "input" when a capture binary exists).
func Schemes() []string
```

### 2.1 Internal sample reader (the decode seam)

Three *file formats* produce samples differently (byte stream vs. frame-of-
int32), so one tiny pull interface unifies them. It emits **interleaved int16
samples at the source's native rate and channel count** — never canonical yet.
The shared `framer` above it does mono-dup → resample → framing. This is the
same seam as the previous round (then named `pcmSource`); renamed
`sampleReader` only to free `Source` for the new public contract.

```go
// sampleReader is a native-rate, native-channel PCM sample producer — the
// single seam over the three decode libs. Implementations: wavSource (wav.go),
// mp3Source (mp3.go), flacSource (flac.go).
type sampleReader interface {
	// info reports native sample rate (Hz) and channel count (1 or 2; >2 is
	// rejected at construction as ErrBadMedia). Valid after construction.
	info() (sampleRate, channels int)

	// read appends up to a bounded number of interleaved int16 samples to dst
	// and returns the grown slice. Interleave is L,R,L,R for stereo, one
	// sample per frame for mono. May return data together with io.EOF (Go
	// convention), or io.EOF alone when drained. Any other error is a decode
	// failure (the framer wraps it in ErrBadMedia).
	read(dst []int16) ([]int16, error)

	io.Closer // releases the decoder (not the underlying byte source)
}

// newDecoder picks a sampleReader by media format and wraps r (a file handle
// or an HTTP body). format is "wav"/"mp3"/"flac"; an empty/unknown format
// triggers a 12-byte sniff (RIFF / "ID3"+sync / "fLaC") before giving up with
// ErrUnsupportedFormat. The returned sampleReader owns r for decode but NOT
// for Close — the caller (file/http source) owns the byte source's lifetime.
func newDecoder(r io.Reader, format string) (sampleReader, error)
```

`mp3Source` (go-mp3) and `wavSource` convert s16le bytes → int16 in `read`;
`flacSource` converts each frame's `Subframes[ch].Samples` (int32, correlation
already undone by `ParseNext`) to int16, scaling by `BitsPerSample`
(`>>(bps-16)` when bps>16, `<<(16-bps)` when bps<16). go-mp3 always emits
2-channel s16le, so `mp3Source.info()` reports `channels = 2` and no mono-dup
runs.

### 2.2 The shared framer (unexported)

The decode pipeline that turns a `sampleReader` into canonical 20 ms frames.
All three media sources embed one; only file uses its EOF, http/input wrap it
in a live pacer (§3).

```go
// framer pulls native int16 samples from src, mono-dups, resamples to 48 kHz,
// and slices canonical 20 ms frames into caller-owned dst. It owns no I/O
// lifetime; Close on the owning Source closes src.
type framer struct {
	src      sampleReader
	rs       *resampler // nil when native rate == 48000 (pass-through)
	channels int        // native channel count (for mono-dup)
	canon    []int16    // accumulated canonical (48k, stereo) samples
	scratch  []int16    // native-read scratch
	idx      uint64     // 0-based frame index (for callers that want it)
	eof      bool       // src drained
}

func newFramer(src sampleReader) *framer

// frame fills dst[:stream.FrameBytes] with the next canonical frame:
//   - returns nil after writing a full (or zero-padded final) frame;
//   - returns io.EOF (no write) once the buffer is empty and src is at EOF.
// This is exactly the pull-paced / D9 behaviour; the live sources call frame
// and translate its io.EOF into "no data right now" (§3).
func (f *framer) frame(dst []byte) error
```

### 2.3 Resampler (unexported — unchanged from the prior round)

```go
// resampler does linear interpolation from inRate to 48000 on interleaved
// stereo int16 (it runs AFTER mono→stereo duplication, so it is always
// 2-channel). Pass-through when inRate == 48000. It keeps the last input
// sample-frame across calls so block boundaries interpolate seamlessly (no
// clicks at 20 ms edges).
type resampler struct {
	inRate int
	pos    int64 // 32.32 fixed-point input position; step = (inRate<<32)/48000
	lastL  int16
	lastR  int16
	primed bool
}

func newResampler(inRate int) *resampler

// process consumes interleaved-stereo int16 input and appends interleaved-
// stereo int16 output (at 48000) to out, returning the grown slice. atEOF
// flushes the tail (blends the final input frame against its own duplicate).
func (r *resampler) process(in []int16, atEOF bool, out []int16) []int16
```

Implementation: `pos` is a 32.32 fixed-point cursor in input sample-frame
units, step `= (inRate << 32) / 48000` per output frame. For each output frame,
`i = pos>>32`, `frac = pos & 0xffffffff`, linearly blend input frames `i` and
`i+1`; emit while `i+1` is in the available input (carry the boundary frame to
the next call). All integer math; no float, no cgo.

---

## 3. Control flow, goroutines, locking

### 3.1 fileSource (pull-paced) — no goroutine, no lock

`Open` with a `file:` URI (or bare path):

1. Strip the `file:` prefix; `filepath.Clean`; reject absolute paths and any
   result escaping `mediaDir` after `filepath.Join(mediaDir, rel)` +
   `filepath.Rel` check → `ErrTraversal`.
2. `os.Open` the file (missing → `ErrBadMedia`).
3. `newDecoder(f, ext)` by extension; build a `framer`.
4. `ReadFrame` just calls `framer.frame(dst)` — it returns `nil` per frame and
   real `io.EOF` at end (D9). `Live()` returns **false**. `Close` closes the
   file once (guarded by a `closed` flag).

Single-goroutine, owned by H's release ticker. Decode runs ahead implicitly:
H pulls at the 20 ms cadence; the framer does only the work for the next frame.

### 3.2 httpSource (live-paced) — one fetch goroutine + a frame channel

`Open` with `http://`/`https://`:

1. `http.NewRequestWithContext(ctx, GET, uri)`; a client with **no overall
   timeout** (streams are infinite) but a dial/response-header timeout (`10 s`).
   Non-2xx → `ErrBadMedia`.
2. Pick the format from `Content-Type` (`audio/mpeg`→mp3, `audio/flac` or
   `audio/x-flac`→flac, `audio/wav`/`audio/x-wav`/`audio/vnd.wave`→wav),
   falling back to the URL path extension, then to a body sniff in
   `newDecoder`. Unknown → `ErrUnsupportedFormat`.
3. Wrap the body in a **bounded read-ahead**: a `framer` over the body feeds a
   buffered channel `frames chan [frameBytes]byte` of depth
   `readaheadFrames = 50` (~1 s). One **producer goroutine** loops
   `framer.frame` and pushes full frames into the channel; it blocks on the
   channel when the consumer is slow (that *is* the bound — TCP backpressure
   then stalls the body read, which is exactly right for a faster-than-realtime
   server). On the framer returning `io.EOF` (server closed a finite body) or
   any error, the producer records it and exits; the channel is closed.

`ReadFrame(dst)` is the **live pacer** and never blocks beyond one frame
period:

```
select {
case f, ok := <-frames:
    if !ok {                    // producer gone
        if fatal := loadErr(); fatal != nil { return wrap(ErrBadMedia) }
        copy(dst, silence); return nil   // server EOF on a live stream → silence, keep cadence
    }
    copy(dst, f[:]); return nil
case <-time.After(frameDeadline):        // underflow: nothing ready in ~20 ms
    copy(dst, silence); return nil       // emit silence, NEVER stall (§6.1)
case <-s.closed:
    return io.EOF                          // only Close yields EOF for a live source
}
```

`frameDeadline` is **one frame period** (`stream.FrameDuration` = 20 ms): H
calls `ReadFrame` once per tick, so if no frame is buffered within ~20 ms we
hand back silence and let the next tick try again. `Live()` returns **true**.
A finite HTTP body (a plain `.mp3` file served over HTTP) thus *plays through
then goes silent* rather than ending the session — consistent with §6.1
(live-paced sources end only on `stop`); H stops it explicitly.

`Close` cancels the request context (unblocking the body read), closes
`s.closed` (unblocking `ReadFrame`), and waits for the producer goroutine.
Idempotent.

**Locking:** the only shared state between the producer and `ReadFrame` is the
buffered channel plus an error guarded by one `sync.Mutex` (`loadErr`/store).
One mutex, never held across a channel op.

### 3.3 inputSource (live-paced) — exec-capture, mirrors E's exec sink

`Open` with `input:`:

1. Probe `$PATH` for the first of `pw-record`, `arecord` (same discovery style
   as E's exec playback backend, but for capture). None found → `ErrBadMedia`
   ("no capture backend"). Capability gating (§2's `Schemes()`) means H won't
   normally call this on a node without one.
2. Build the argv to emit **raw s16le 48 kHz stereo** on stdout:
   - `pw-record --rate 48000 --channels 2 --format s16 -` (stdout), or
   - `arecord -f S16_LE -r 48000 -c 2 -t raw -` .
   Because the capture tool already produces canonical-rate stereo s16le, the
   decode path is the trivial one: a `sampleReader` that just reads s16le bytes
   off the pipe at native 48 k/2ch, so the `framer`'s resampler is pass-through
   and there is no mono-dup. (If a tool can't be told the rate, the framer's
   resampler still normalizes it — but the default argv asks for 48 k.)
3. `exec.CommandContext(ctx, …)`; `cmd.StdoutPipe()`; `cmd.Start()`. Stderr is
   logged at debug. The pipe feeds the same producer-goroutine + bounded
   read-ahead + live-pacer machinery as http (§3.2) — **shared code**: http and
   input differ only in how the byte stream is obtained; both wrap a `framer`
   in `liveReader{frames, closed, err}`.

`ReadFrame` is identical to http's live pacer: a stalled capture (xrun, device
busy) yields silence, never a stall. `Live()` returns **true**. `Close` cancels
the context (which sends SIGKILL to the process group after a short grace, like
E's exec sink), closes `s.closed`, drains, and `cmd.Wait()`s. The process exit
or a closed pipe is treated as a transient underflow (silence), not a fatal
error, unless setup itself failed.

### 3.4 Shared live machinery

`http.go` and `input.go` both construct a `*liveReader`:

```go
// liveReader adapts a pull framer over an arbitrary byte stream into the
// live-paced Source semantics (never EOF; underflow→silence). Used by both
// httpSource and inputSource.
type liveReader struct {
	frames chan [stream.FrameBytes]byte
	closed chan struct{}
	mu     sync.Mutex
	err    error
	once   sync.Once // Close guard
	stop   context.CancelFunc
	done   chan struct{} // producer exited
}
```

so the never-block / silence-on-underflow / Close semantics live in **one
place** and are tested once. The file source does not use it (it wants real
EOF). This keeps each concrete source file tiny (just "obtain bytes + format"),
which is the whole point of the §6.1 abstraction.

### 3.5 Scheme registry / Open dispatch

`source.go` holds a tiny map literal `scheme → constructor`:

```go
var registry = map[string]func(ctx, uri, mediaDir) (Source, error){
	SchemeFile:  openFile,
	SchemeHTTP:  openHTTP,   // serves both http: and https:
	SchemeInput: openInput,
}
```

`Open` splits the scheme at `:` (no scheme ⇒ `file`), maps `https`→`http`
bucket, looks up the constructor, and calls it; unknown ⇒
`ErrUnsupportedScheme`. Adding a new source kind (Spotify, snapcast pipe, §6.1)
is one constructor + one map entry — the explicit "register a scheme"
extension point the spec calls for.

No global mutable state, no init-order coupling: the map is a package-level
literal, read-only after init, so `Open` needs no lock.

---

## 4. Edge cases & failure handling

- **Unknown scheme (§6/§6.1)** → `ErrUnsupportedScheme`. Bare path (no `:` or a
  Windows-free relative path) ⇒ treated as `file`. `https://` routes to the
  http source.
- **Path traversal (§6)**: `file:../../etc/passwd` and absolute paths are
  rejected with `ErrTraversal` *before* any `os.Open`; resolution is
  `filepath.Rel(mediaDir, filepath.Join(mediaDir, clean))` must not start with
  `..`.
- **Unknown media format**: extension/content-type not wav/mp3/flac and the
  12-byte sniff fails ⇒ `ErrUnsupportedFormat`.
- **Mono source (§8.1)**: duplicated to stereo *before* resampling, so the
  resampler is always 2-channel and the dup is cheap.
- **>2 channels**: rejected at construction as `ErrBadMedia` (no downmix matrix
  in v1; spec only requires mono-dup + rate convert).
- **Native rate == 48000 (§8.1)**: resampler is pass-through (identity),
  bit-exact frames — the common case (48 k WAV, captured input) stays cheap.
- **Arbitrary rate (44100/22050/96000…)**: linear interpolation to 48000;
  carrying the last input frame across `read()` calls prevents 20 ms-boundary
  clicks; final flush emits the tail.
- **Final partial frame, pull-paced (D9, §8.2)**: a file rarely ends on a 20 ms
  boundary; the last frame is **zero-padded to FrameBytes** and returned with
  `err==nil`; the *next* `ReadFrame` returns `io.EOF`. This lets H detect
  natural end, bump the generation, and clear playback status (§8.6).
- **EOF only for pull-paced (§6.1, D26)**: file's `ReadFrame` returns `io.EOF`;
  http/input `ReadFrame` **never** returns `io.EOF` except after `Close`
  (where it signals the reader to stop). H keys off `Source.Live()`: a
  pull-paced EOF ends the session; a live source ends only on explicit `stop`.
- **Live underflow (§6.1 — the load-bearing case)**: when no frame is buffered
  within one frame period (network stall, capture xrun), the live source fills
  `dst` with silence and returns `nil`. H's 20 ms release ticker therefore
  keeps emitting frames with monotonically advancing `seq`/`pts` — the cadence
  **never stalls**, exactly as the spec requires; the listener hears a brief
  silence, the stream recovers when bytes resume.
- **Finite HTTP body on a "live" URL**: a plain file served over HTTP plays to
  its end, then the live source emits silence (does not EOF) until H stops it —
  uniform with internet-radio behaviour; no special-casing.
- **HTTP non-2xx / dial failure**: `ErrBadMedia` at `Open` (setup), surfaced to
  H/API as a clear error. A mid-stream connection drop is *not* fatal to a live
  source: it becomes underflow→silence (radio reconnect is out of scope for v1;
  the watchdog/RESTART path lives in the sink/group, not here).
- **Bounded read-ahead**: the producer channel (depth ~50 frames ≈ 1 s) caps
  memory; a faster-than-realtime server is throttled by channel backpressure →
  TCP flow control, not by a sleep. A slow server simply underflows to silence.
- **Capture tool missing (§6.1)**: `Schemes()` omits `input`, so capabilities
  don't advertise it; if `input:` is requested anyway, `openInput` returns
  `ErrBadMedia` ("no capture backend").
- **Subprocess death / pipe EOF (input)**: treated as transient underflow
  (silence), matching live semantics; `Close` SIGKILLs and `Wait`s so no zombie
  is left (mirrors E's exec sink kill-on-Close, DECISIONS.md D21).
- **WAV sub-formats**: hand-rolled reader supports PCM `u8`, `s16`, `s24`
  (down-shifted), IEEE float32 (`fmt` tag 3, clamped to s16); a-law/μ-law/
  exotic tags → `ErrBadMedia`. It scans chunks for `fmt ` then `data`,
  tolerating intervening `LIST`/`fact`. Truncated `data` ⇒ EOF at the last
  whole sample-frame.
- **FLAC bit depth / correlation**: 16-bit passes through; 24/20/8-bit scaled
  by per-sample shift; mid/side/left-side/side-right correlation is already
  resolved by `flac.Stream.ParseNext`, so D reads plain L/R int32. go-mp3's
  `UnexpectedEOF` and flac truncation-at-end normalize to `io.EOF` (graceful
  end), not `ErrBadMedia`.
- **Empty / zero-sample file**: first `ReadFrame` on a file source returns
  `io.EOF` immediately; no panic, no padded silent frame.
- **`dst` shorter than FrameBytes**: documented contract violation; ReadFrame
  may panic (slice bounds) — H always passes a FrameBytes buffer. Tested as a
  documented precondition, never silent corruption.
- **Close during ReadFrame**: live sources' `Close` cancels ctx + closes
  `s.closed`, so a blocked `ReadFrame` returns promptly (`io.EOF`); file
  source's reads are local and short. Double Close is idempotent (`sync.Once`).
- **No allocation on the hot path**: `ReadFrame` writes into the caller's `dst`;
  the framer reuses `canon`/`scratch` slices; only setup allocates.

---

## 5. Test plan

All hardware-free: WAV fixtures synthesized in-test; an `httptest.Server`
serves bytes; a tiny **fake capture command** (a `go run`-built helper, or the
test binary re-exec'd with an env flag) emits raw s16le so the input source is
exercised with no real device. `internal/stream` constants are imported, never
duplicated. mp3/flac decode tests `t.Skip` when no committed testdata fixture
is present (IMPLEMENTATION.md D).

`internal/audio/source_test.go`
- `TestOpenSchemeDispatch` — `file:`, bare path, `http://`, `https://`,
  `input:` route to the right impl; `ftp://`/`spotify:` → `ErrUnsupportedScheme`.
- `TestSchemesReportsFileHTTPAlways` — `Schemes()` always contains file+http;
  contains `input` iff a capture binary is on a faked `$PATH`.
- `TestFileEOFContract` — D9 end-to-end: last frame nil err (zero-padded tail),
  next call `io.EOF`; `Live()==false`.
- `TestLiveNeverEOFThenSilence` — a live source whose producer is starved
  returns nil + an all-zero frame within ~one frame period; never `io.EOF`
  until `Close`; `Live()==true`.

`internal/audio/file_test.go`
- `TestFileTraversalRejected` — `file:../x`, absolute paths → `ErrTraversal`.
- `TestFileMissing` → `ErrBadMedia` (not panic).
- `TestFilePullPacedFrames` — 48 k stereo WAV: every frame FrameBytes, bytes
  match source exactly (no resample drift), real EOF at end.

`internal/audio/http_test.go`
- `TestHTTPContentTypeDispatch` — server sets `audio/wav` / `audio/mpeg` /
  `audio/flac`; correct decoder chosen.
- `TestHTTPExtensionFallback` — no/again-wrong content-type, URL `.wav` ext →
  wav; body sniff path covered.
- `TestHTTPLivePacedSilenceOnStall` — server trickles bytes slower than
  realtime; `ReadFrame` returns silence frames in the gaps, never blocks beyond
  ~a frame period, never `io.EOF`.
- `TestHTTPFiniteBodyGoesSilent` — finite body served fully → frames then
  silence (not EOF) until Close.
- `TestHTTPNon2xxIsBadMedia` — 404 → `ErrBadMedia` at Open.
- `TestHTTPBoundedReadahead` — fast server; channel depth caps in-flight frames
  (assert producer blocks, memory bounded) — observe via a counting reader.
- `TestHTTPCloseCancels` — Close mid-stream returns promptly, producer goroutine
  exits (no leak; `-race`).

`internal/audio/input_test.go`
- `TestInputFakeCaptureFrames` — fake command emits a known raw s16le tone;
  frames decode to that tone; `Live()==true`.
- `TestInputNoBackendIsBadMedia` — empty faked `$PATH` → `ErrBadMedia`.
- `TestInputCloseKillsProcess` — Close terminates the (long-running) fake
  command; `Wait` returns; no zombie, no goroutine leak (`-race`).
- `TestInputStallSilence` — fake command pauses output; `ReadFrame` yields
  silence, never stalls.

`internal/audio/wav_test.go`
- `TestWAVParseS16/U8/S24/Float32` — synthesized fixtures: rate/channels/
  samples correct; u8 `0x80→0`; s24 sign preserved; float clamped.
- `TestWAVSkipsAuxChunks` — `LIST`/`fact` before `data` skipped.
- `TestWAVTruncatedDataIsEOF` — short `data` → EOF at last whole frame.
- `TestWAVRejectsALaw` / `TestWAVRejectsMissingDataChunk` → `ErrBadMedia`.

`internal/audio/decode_test.go`
- `TestMonoDuplicated` — mono samples → L==R per frame.
- `TestFinalFramePadded` — non-20 ms-multiple length → last frame padded with
  zeros, nil err, then EOF.
- `TestEmptyImmediateEOF` — zero-sample source → first frame `io.EOF`.
- `TestSniffDispatch` — `newDecoder` with empty format sniffs RIFF/ID3/fLaC.

`internal/audio/resample_test.go`
- `TestResamplePassthrough48k` — identity bit-exact.
- `TestResampleHalfRate` — 24000→48000 doubles length (±1); midpoints = mean of
  neighbours.
- `TestResampleUpDownRatios` — 44100/96000/22050 lengths within ±1 of
  `n*48000/inRate`.
- `TestResampleBlockBoundaryContinuity` — two-chunk == one-chunk (carry frame).
- `TestResampleEOFFlush` / `TestResampleConstantSignal` — tail flush; DC stays
  DC (no overshoot).

`internal/audio/fixtures_test.go` (helpers)
- `writeWAVs16/u8/float32(t, rate, ch, samples)` — minimal RIFF/WAVE fixtures.
- `genTone(rate, ch, freq, dur)` — deterministic int16 tone.
- `fakeCaptureExe(t)` — builds/locates a helper emitting raw s16le on stdout
  (for input tests); honours a "stall" / "duration" arg.
- `maybeFixture(t, name)` — path + `skip=true` when an optional mp3/flac
  testdata file is absent.

`internal/audio/source_test.go` (fixture-gated)
- `TestDecodeMP3Fixture` / `TestDecodeFLACFixture` — when `testdata/tone.mp3` /
  `tone.flac` present, decode to ~expected duration of 48 k stereo frames;
  else `t.Skip`.

All tests use loopback `httptest`, in-process fakes, generated bytes, and a
faked capture command — no root, no audio hardware, no network egress.

---

## 6. Notes for the integrator / downstream

- **H consumes only `Open`/`Source`** (`ReadFrame`, `Live`, `Close`). The
  release ticker, lead, pts stamping, ring buffer, and burst priming all live
  in H/G — D only decodes and frames on demand and tells H its pacing class via
  `Live()`. `Live()` is the seam that lets H apply the §6.1 rule "pull-paced EOF
  ends the session; live-paced ends only on stop" without knowing the scheme.
- **Capabilities (§1, §6.1)**: `audio.Schemes()` is the source of the
  `capabilities.sources` list; K assembles capabilities at startup (DECISIONS.md
  D3) and calls `Schemes()` for the source schemes. `file`/`http` are always
  present; `input` is host-probed. `capabilities.formats` (`wav/mp3/flac`)
  stays a static list owned by the config/node layer — D just decodes all three.
- **D imports `internal/stream` only for the PCM constants** (DECISIONS.md
  "Confirmed as designed") — never the wire `Header`/`Mux`. D emits raw 3840 B
  PCM payloads; G prepends the header.
- **go.mod** adds `github.com/hajimehoshi/go-mp3` and `github.com/mewkiz/flac`
  (both in the allowed-deps closure). WAV is hand-rolled (~110 lines), so
  `go-audio/wav` is **not** a dependency (IMPLEMENTATION.md "prefer hand-rolled
  if under ~120 lines"). The http source uses only `net/http`; the input source
  only `os/exec` — no new deps, no cgo.
- **`input` exec discovery mirrors E** (DECISIONS.md D27): same probe style as
  the exec *playback* backend, kill-on-Close like the exec sink's write-deadline
  limitation (D21). The two pieces deliberately do not share a package (E owns
  output, D owns capture), but the exec idiom is identical so behaviour matches.
- **No `Resolver`/`SetEndpoints`/push-model anything** here — D is decode-only
  and entirely below the subscribe/stream layer (D18/D22); media URIs come from
  the play request, addresses never enter this package.
