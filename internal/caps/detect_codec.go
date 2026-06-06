package caps

// canonicalRate is the cluster's canonical audio sample rate, 48000 Hz (A.12
// "Audio canonical rate 48000"). Used as the MaxRate fallback when a sink is
// usable but the MaxRateProber yields ≤ 0 (e.g. the coarse exec backend, which
// cannot query the device, §5.2).
const canonicalRate = 48000

// Wire codec tokens (A.11 R2, D3). The wire codec set is {pcm, opus} only —
// FLAC/MP3 are source decoders, never wire codecs (A.11 R2).
const (
	codecPCM  = "pcm"
	codecOpus = "opus"
)

// FEC scheme tokens (A.10, D4). All three are pure-Go and always supported.
const (
	fecNone      = "none"
	fecXORParity = "xorParity"
	fecDuplicate = "duplicate"
)

// detectCodecs returns this node's originate (encode) and play (decode) wire
// codec sets (A.11 R2, D3). PCM is the mandatory baseline (no encoder/decoder
// needed) and is ALWAYS present; Opus is added to both iff the Opus binding is
// available at runtime. This build links one shared libopus that does both
// encode and decode, so the answer is identical for the two lists (P5.2 §4.4).
func detectCodecs() (encode, decode []string) {
	encode = []string{codecPCM}
	decode = []string{codecPCM}
	if opusAvailable() {
		encode = append(encode, codecOpus)
		decode = append(decode, codecOpus)
	}
	return encode, decode
}

// opusProber is the runtime Opus-availability hook (P5.2 §4.4). It is nil on the
// default build (no `opus` tag) so opusAvailable() reports absent — matching the
// P2.6 MVP default and never aborting the node (R7 fail-soft, §9 open question
// 4). The `opus`-tagged detect_codec_opus.go sets it from an init() to a cached
// dlopen liveness check (codec.OpusRuntimeAvailable). Assigned once at init,
// read-only thereafter, so it needs no lock.
var opusProber func() bool

// opusAvailable reports whether the Opus encoder/decoder binding is usable at
// runtime: false unless the `opus`-tagged build wired a probe AND that probe
// (a cached dlopen of libopus + a test encoder/decoder construction) succeeds.
func opusAvailable() bool {
	return opusProber != nil && opusProber()
}

// detectFEC returns the supported FEC schemes (A.10, D4). All three are pure-Go
// and unconditionally advertised; there is no FEC masking key (07 §2.4.2).
func detectFEC() []string {
	return []string{fecNone, fecXORParity, fecDuplicate}
}

// detectMaxRate resolves the effective device max rate from the prober seam
// (06 §1.1). When a sink is usable but the prober yields ≤ 0 (e.g. the coarse
// exec backend cannot query the device), it falls back to the canonical rate
// (A.12, §5.2). When no sink is usable it returns 0 (no device to clamp).
func detectMaxRate(probed int, haveSink bool) int {
	if !haveSink {
		return 0
	}
	if probed > 0 {
		return probed
	}
	return canonicalRate
}
