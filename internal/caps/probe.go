package caps

// This file holds the runtime-probe seam to the P4.6 sink registry and the
// assembly of the raw Detected set (07 §2.4.1 "detected", 06 §1.5). caps never
// imports internal/audio/sink: it consumes the sink half ONLY through the
// SinkProber/MaxRateProber function values defined here, so it builds and
// unit-tests standalone before P4.6 lands (doc 01 §2 layering, §4.2).

// SinkProber is the seam to the P4.6 sink registry (06 §1.5 Probe). It returns
// the backends that actually work on this machine, in registry preference
// order. P2.6 consumes ONLY this function value; P4.6 supplies the real
// implementation (internal/audio/sink.Probe wrapped, mapping sink.Backend ->
// caps.Backend), and tests / cmd-before-P4.6 supply a stub.
type SinkProber func() []Backend

// Backend mirrors README §6.1 / 06 §1.1 (audio/sink.Backend) BY VALUE so
// internal/caps need not import internal/audio/sink (avoids the dependency until
// P4.6 exists). P4.6's adapter maps sink.Backend -> caps.Backend 1:1.
type Backend struct {
	Name    string // "alsa" | "exec:aplay" | "exec:pw-play"
	Precise bool   // true => precise (kernel-ioctl ALSA) Delay()
}

// MaxRateProber reports the highest sample rate the chosen device will run
// (06 §1.1). Seam to P4.6 (the precise backend can query the device; the coarse
// exec path returns the canonical 48000). Returns 0 when no sink is usable.
type MaxRateProber func() int

// ProbeDeps bundles the seams + the local detectors so Probe is fully injectable
// for tests. Codec/FEC detection is local (detect_codec.go) and needs no seam
// for the MVP.
type ProbeDeps struct {
	Sinks   SinkProber    // REQUIRED; from P4.6 (or stub)
	MaxRate MaxRateProber // REQUIRED; from P4.6 (or stub)
}

// Probe runs the seams + local detectors and assembles the raw Detected
// (pre-mask, §4.2). The returned Detected slices are sorted/deduplicated by the
// detectors and sinkNames so the value is byte-stable. A nil Sinks/MaxRate seam
// is treated as "no sink" (defensive: a caller before P4.6 must inject stubs,
// but a nil must not panic the node — R7 fail-soft).
func Probe(d ProbeDeps) Detected {
	var backends []Backend
	if d.Sinks != nil {
		backends = d.Sinks()
	}
	sinks := sinkNames(backends)
	haveSink := len(sinks) > 0

	probedRate := 0
	if d.MaxRate != nil {
		probedRate = d.MaxRate()
	}

	encode, decode := detectCodecs()
	return Detected{
		Sinks:        sinks,
		EncodeCodecs: canonical(encode),
		DecodeCodecs: canonical(decode),
		FEC:          canonical(detectFEC()),
		MaxRate:      detectMaxRate(probedRate, haveSink),
	}
}

// sinkNames projects the probed backends to their canonical Names, sorted and
// deduplicated. Returns nil for an empty/nil input so a sink-less node yields a
// byte-stable empty Sinks set.
func sinkNames(backends []Backend) []string {
	if len(backends) == 0 {
		return nil
	}
	names := make([]string, 0, len(backends))
	for _, b := range backends {
		names = append(names, b.Name)
	}
	return canonical(names)
}
