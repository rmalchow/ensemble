package sink

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"ensemble/internal/contracts"
)

// Factory builds a backend from its spec argument (the part after the colon in
// ENSEMBLE_OUTPUT, e.g. the path for "file:/tmp/x", "" for "null"/"exec").
type Factory func(arg string, log *slog.Logger) (contracts.Backend, error)

var (
	regMu    sync.RWMutex
	registry = map[string]Factory{}
)

// Register adds a named backend factory. Called from each backend file's
// init(). Replacing an existing name panics (programmer error).
func Register(name string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, ok := registry[name]; ok {
		panic("sink: backend already registered: " + name)
	}
	registry[name] = f
}

func init() {
	Register("null", func(_ string, _ *slog.Logger) (contracts.Backend, error) {
		return newNullBackend(), nil
	})
	Register("file", func(arg string, _ *slog.Logger) (contracts.Backend, error) {
		return newFileBackend(arg)
	})
	Register("exec", func(_ string, log *slog.Logger) (contracts.Backend, error) {
		return newExecBackend(log)
	})
	// "alsa" registers itself in backend_alsa.go's init() only when the
	// dl.Open probe of libasound.so.2 succeeds (D34).
}

func isRegistered(name string) bool {
	regMu.RLock()
	defer regMu.RUnlock()
	_, ok := registry[name]
	return ok
}

// BackendNames returns the registered backend names, sorted, for
// capabilities.backends (§1; assembled by K, D3). Pure; no process spawn.
func BackendNames() []string {
	regMu.RLock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	regMu.RUnlock()
	sort.Strings(names)
	return names
}

// HasPlayback reports whether a real (non-null) backend is usable on this host:
// true iff "alsa" is registered (libasound loaded) or "exec" can resolve a
// player tool on $PATH. Drives capabilities.playback (§1, D27). Pure lookup; no
// spawn.
func HasPlayback() bool {
	if isRegistered("alsa") {
		return true
	}
	_, _, ok := lookExecTool()
	return ok
}

// Open resolves an ENSEMBLE_OUTPUT-style spec into a backend (D2/D27).
//
//	"" | "auto"      -> best available: alsa -> exec -> null (never errors)
//	"alsa"           -> alsaBackend; errors if unregistered or device won't open
//	"exec"           -> first player on $PATH; degrades to null+WARN if none
//	"null"           -> nullBackend
//	"file:/abs/path" -> fileBackend appending raw PCM
//	"<name>[:arg]"   -> any registered factory, arg after the first colon
//
// Returns the backend and the resolved name (for /api/status + logging).
func Open(spec string, log *slog.Logger) (contracts.Backend, string, error) {
	if log == nil {
		log = slog.Default()
	}
	log = log.With("comp", "sink")

	name, arg := splitSpec(spec)

	switch name {
	case "", "auto":
		return openAuto(log)
	case "exec":
		b, err := openFactory("exec", "", log)
		if err != nil {
			log.Warn("exec backend unavailable, degrading to null", "err", err)
			nb, _ := openFactory("null", "", log)
			return nb, "null", nil
		}
		log.Info("backend selected", "backend", "exec", "reason", "explicit")
		return b, "exec", nil
	}

	b, err := openFactory(name, arg, log)
	if err != nil {
		return nil, "", err
	}
	log.Info("backend selected", "backend", name, "reason", "explicit")
	return b, name, nil
}

func openAuto(log *slog.Logger) (contracts.Backend, string, error) {
	if isRegistered("alsa") {
		if b, err := openFactory("alsa", "", log); err == nil {
			log.Info("backend selected", "backend", "alsa", "reason", "auto")
			return b, "alsa", nil
		} else {
			log.Warn("alsa registered but failed to open, trying exec", "err", err)
		}
	}
	if _, _, ok := lookExecTool(); ok {
		if b, err := openFactory("exec", "", log); err == nil {
			log.Info("backend selected", "backend", "exec", "reason", "auto")
			return b, "exec", nil
		} else {
			log.Warn("exec backend failed to open, falling back to null", "err", err)
		}
	}
	b, _ := openFactory("null", "", log)
	log.Info("backend selected", "backend", "null", "reason", "auto (no playback device)")
	return b, "null", nil
}

func openFactory(name, arg string, log *slog.Logger) (contracts.Backend, error) {
	regMu.RLock()
	f, ok := registry[name]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("sink: backend %q not registered", name)
	}
	return f(arg, log)
}

// splitSpec splits "name:arg" on the first colon. "name" alone => arg "".
func splitSpec(spec string) (name, arg string) {
	spec = strings.TrimSpace(spec)
	if i := strings.IndexByte(spec, ':'); i >= 0 {
		return spec[:i], spec[i+1:]
	}
	return spec, ""
}
