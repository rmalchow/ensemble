package audio

import (
	"fmt"
	"os/exec"
	"slices"
)

// Backend names a usable output backend (06 §1.1). Name is the canonical
// identifier gossiped in Caps.Sinks; Precise is true only for the alsa backend
// whose Delay() returns a precise kernel hardware figure.
type Backend struct {
	Name    string // "alsa" | "exec:aplay" | "exec:pw-play"
	Precise bool   // true => Delay() returns a precise kernel figure (alsa only)
}

// Backend Name constants — the canonical strings used in Caps.Sinks and in
// ProbeConfig.Disabled/Prefer (06 §1.1, §1.5).
const (
	BackendALSA       = "alsa"
	BackendALSALib    = "alsalib" // shared-access libasound via dlopen (plugin layer)
	BackendExecAplay  = "exec:aplay"
	BackendExecPwPlay = "exec:pw-play"
)

// ProbeConfig is the per-node config input to the probe (06 §1.1, §1.5): the
// device string, the set of backend Names the operator has DISABLED (subtracted
// from the probe result), and the preference order.
type ProbeConfig struct {
	Device   string   // per-node device string from NodeRecord (""=each backend's "default")
	Disabled []string // backend Names turned off in config (e.g. ["alsa"] to force coarse)
	Prefer   []string // preference order; missing names keep the default order after the prefixed ones
}

// defaultPrefer is the registry's built-in preference order. The shared
// libasound path (alsalib) leads: it is PRECISE (snd_pcm_delay) like the raw hw
// backend but opens devices through the plugin layer, so it works both on a
// bare Pi (dmix) AND on a desktop whose card a sound server owns — exactly
// where raw hw fails. Raw hw is next (no libasound needed), then the coarse
// exec players (pw-play ahead of aplay for the PipeWire case). Operators
// override via ProbeConfig.Prefer (06 §1.1 note 3).
var defaultPrefer = []string{BackendALSALib, BackendALSA, BackendExecPwPlay, BackendExecAplay}

// backendProbe is one compiled-in backend and its real liveness check. probe
// returns true only when the backend ACTUALLY works on this machine (06 §1.1:
// presence of the device node is necessary but not sufficient — the alsa probe
// opens and configures the PCM once; the exec probes resolve the binary on PATH).
type backendProbe struct {
	backend Backend
	probe   func(device string) bool
	open    func(device string) (AudioSink, error)
}

// backends is the compiled-in backend table in default preference order. There
// is no build-time selection (D12): every OS-supported backend is present and
// chosen at runtime by Probe/Open. The alsa entry's probe/open degrade to "not
// usable"/error on non-linux via the alsa_stub.go build split.
func backends() []backendProbe {
	return []backendProbe{
		{
			backend: Backend{Name: BackendALSALib, Precise: true},
			probe:   probeAlsaLib,
			open:    newAlsaLibSink,
		},
		{
			backend: Backend{Name: BackendALSA, Precise: true},
			probe:   probeALSA,
			open:    func(device string) (AudioSink, error) { return newALSASink(device) },
		},
		{
			backend: Backend{Name: BackendExecPwPlay, Precise: false},
			probe:   func(string) bool { return lookPath("pw-play") },
			open:    func(device string) (AudioSink, error) { return newExecSinkCmd(device, pwPlayCommand), nil },
		},
		{
			backend: Backend{Name: BackendExecAplay, Precise: false},
			probe:   func(string) bool { return lookPath("aplay") },
			open:    func(device string) (AudioSink, error) { return newExecSinkCmd(device, defaultPlayerCommand), nil },
		},
	}
}

// lookPath reports whether name resolves on PATH. Split out so tests can rely on
// PATH stubbing; it is the exec backends' liveness probe (06 §1.1).
func lookPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// newExecSinkCmd builds an ExecSink whose command template comes from tmplFn,
// resolving device "" to "default" so the template's {device} (or fixed device)
// is non-empty.
func newExecSinkCmd(device string, tmplFn func(string) []string) *ExecSink {
	if device == "" {
		device = "default"
	}
	return &ExecSink{device: device, command: tmplFn(device)}
}

// Probe tries every compiled-in backend, returns those that ACTUALLY WORK on
// this machine, minus config-disabled, in preference order. Cheap; run once at
// startup. A backend whose /dev/snd node exists but cannot be opened/configured
// (busy, owned by a sound server, format-incapable) is NOT returned (06 §1.1).
func Probe(cfg ProbeConfig) []Backend {
	disabled := make(map[string]struct{}, len(cfg.Disabled))
	for _, d := range cfg.Disabled {
		disabled[d] = struct{}{}
	}

	// 1. Liveness probe each backend; keep the ones that pass and are not disabled.
	usable := make([]Backend, 0, 3)
	for _, bp := range backends() {
		if _, off := disabled[bp.backend.Name]; off {
			continue
		}
		if bp.probe(cfg.Device) {
			usable = append(usable, bp.backend)
		}
	}

	// 2/3. Reorder per cfg.Prefer; names not listed in Prefer keep the default
	// order after the prefixed ones.
	order := orderedNames(cfg.Prefer)
	rank := make(map[string]int, len(order))
	for i, n := range order {
		rank[n] = i
	}
	slices.SortStableFunc(usable, func(a, b Backend) int {
		return rank[a.Name] - rank[b.Name]
	})
	return usable
}

// orderedNames returns the full ranking of backend names: the caller's prefer
// list first (de-duplicated, unknown names dropped), then the default-order
// remainder for any name not already listed (06 §1.1 step 3).
func orderedNames(prefer []string) []string {
	known := map[string]struct{}{
		BackendALSA: {}, BackendExecPwPlay: {}, BackendExecAplay: {},
	}
	seen := make(map[string]struct{}, len(known))
	out := make([]string, 0, len(known))
	for _, n := range prefer {
		if _, ok := known[n]; !ok {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	for _, n := range defaultPrefer {
		if _, dup := seen[n]; !dup {
			out = append(out, n)
		}
	}
	return out
}

// MaxRate reports the highest sample rate the best usable backend will run, for
// the caps MaxRateProber seam (06 §1.1/§1.5). MVP returns the canonical 48000
// when any backend is usable and 0 when none is (a sink-less node has no output
// rate). The precise alsa path can in principle probe higher rates via HW_REFINE;
// that refinement is deferred to the hardware spike — the canonical rate is the
// only rate the renderer commits today (A.12). The wiring piece adapts this into
// caps.MaxRateProber alongside Probe→caps.SinkProber, so caps never imports this
// package (doc 01 §2 layering).
func MaxRate(cfg ProbeConfig) int {
	if len(Probe(cfg)) == 0 {
		return 0
	}
	return canonicalRate
}

// canonicalRate is the pinned group profile rate (A.12). Quoted, not invented.
const canonicalRate = 48000

// Open opens the first backend in `preferred` that passes a fresh usability
// check AND constructs, binding it to `device` but NOT calling Start (the
// renderer commits rate/channels later via Start). The fallback is graceful at
// BOTH stages: a backend that probes ok but fails to open (lost a race for the
// device, library vanished) is skipped and the next one tried — never a hard
// stop while a later choice would work (06 §1.1). An empty `preferred` falls
// back to the default preference order. Returns an error only when NONE opens.
func Open(preferred []string, device string) (AudioSink, error) {
	s, _, err := OpenNamed(preferred, device)
	return s, err
}

// OpenNamed is Open returning also the chosen backend's name (for status/log
// lines: the operator must be able to SEE which fallback tier is playing).
func OpenNamed(preferred []string, device string) (AudioSink, string, error) {
	if len(preferred) == 0 {
		preferred = defaultPrefer
	}
	table := backends()
	byName := make(map[string]backendProbe, len(table))
	known := make([]string, 0, len(table))
	for _, bp := range table {
		byName[bp.backend.Name] = bp
		known = append(known, bp.backend.Name)
	}

	var failures []string
	for _, name := range preferred {
		bp, ok := byName[name]
		if !ok {
			return nil, "", fmt.Errorf("audio: unknown backend %q (known: %v)", name, known)
		}
		if !bp.probe(device) {
			failures = append(failures, name+": probe failed")
			continue
		}
		s, err := bp.open(device)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		return s, name, nil
	}
	return nil, "", fmt.Errorf("audio: no usable backend among %v (%v)", preferred, failures)
}
