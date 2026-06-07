// Package config resolves flags + env fallbacks, the data/media directories,
// and node.json persistence (id, name, volume, outputDelayMs). It is pure and
// unit-testable: no sockets, no goroutines, no hardware. Capability detection
// (playback backend, codec/format lists) lives elsewhere (D3); config only
// carries the persistent identity and the resolved knobs main wires in.
package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"ensemble/internal/id"
)

// Defaults (spec §2). Ports are bases; bind-or-increment happens in netx/main.
const (
	DefaultHTTPPort   = 8080
	DefaultStreamPort = 9090
	DefaultSourcePort = 9200
	DefaultGossipPort = 7946
	DefaultDataDir    = "data" // relative to CWD if not overridden
	DefaultOutput     = ""     // "" = auto-detect backend (sink decides; "auto")
)

// Env var names (spec §2, §8.5). Flags override env; env overrides defaults.
const (
	EnvHTTPPort   = "ENSEMBLE_HTTP_PORT"
	EnvStreamPort = "ENSEMBLE_STREAM_PORT"
	EnvSourcePort = "ENSEMBLE_SOURCE_PORT"
	EnvGossipPort = "ENSEMBLE_GOSSIP_PORT"
	EnvDataDir    = "ENSEMBLE_DATA_DIR"
	EnvMediaDir   = "ENSEMBLE_MEDIA_DIR"
	EnvOutput     = "ENSEMBLE_OUTPUT"  // named sink backend: "", "auto", "exec", "null", "file:<path>", "alsa"
	EnvJoin       = "ENSEMBLE_JOIN"    // dev seed list: comma-separated host:gossipPort (§2, D20)
	EnvNoMDNS     = "ENSEMBLE_NO_MDNS" // "1"/"true": disable mDNS register+browse (tests; gossip via --join)
)

// Config is the fully-resolved startup configuration. All fields are final:
// flags+env precedence applied, dirs made absolute, node.json loaded/created.
// Plain value; safe to copy. Holds no open resources.
type Config struct {
	// Identity (from node.json; see Store).
	NodeID   id.ID  // immutable, persisted (§1)
	NodeName string // current name; first 8 hex of id on first start (§1)

	// Live per-node knobs (from node.json; see Store). Defaulted on a
	// back-compat load when absent (§1, D35/D36).
	Volume        float64  // playback software gain 0.0–1.0; default 1.0 (D35)
	OutputDelayMs int      // hardware latency calibration; default 0, clamped ±500 (D36)
	OutputDevice  string   // selected ALSA output device id; default "default" (D37)
	Disabled      []string // operator-disabled features; subset of {playback,opus,input} (D40)

	// Resolved, absolute directories (§2).
	DataDir  string // e.g. /abs/data; contains node.json
	MediaDir string // e.g. /abs/data/media; default DataDir/media

	// Base ports (§2). Actual bound ports are decided later by netx (K).
	HTTPPort   int
	StreamPort int
	SourcePort int // audio source: subscriptions + stream control (§8.7)
	GossipPort int

	// Sink backend override (§8.5). "" => auto-detect ("auto"); selects a NAMED
	// backend: "auto" | "exec" | "null" | "file:<path>" | "alsa" (where built).
	// The sink piece (E) interprets the value; config only carries it verbatim.
	Output string

	// Join is the dev-only gossip seed list (§2, D20): comma-separated
	// host:gossipPort entries, parsed from --join / ENSEMBLE_JOIN. Empty in
	// production (mDNS is the discovery path). main (K) passes it to
	// cluster.Join for hermetic loopback e2e tests; config only carries it.
	Join []string

	// NoMDNS disables mDNS discovery entirely (register + browse). Dev/test
	// only: hermetic e2e clusters must not advertise into — or absorb nodes
	// from — the surrounding LAN. Gossip then needs --join seeds.
	NoMDNS bool

	// store is the node.json handle for runtime mutations. Unexported; use
	// Rename / SetVolume / SetOutputDelayMs.
	store *Store
}

// Options lets the caller (main, tests) inject argv and an env lookup so the
// package is testable without touching the real process environment.
type Options struct {
	Args   []string                // flag arguments, e.g. os.Args[1:]
	Getenv func(key string) string // nil => os.Getenv
}

// Load resolves configuration (flags > env > defaults), resolves+creates the
// data/media dirs, then opens or creates node.json via Store. Errors are fatal
// to main: bad flag, unwritable data dir, corrupt node.json. See A-config.md §2.
func Load(opts Options) (*Config, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}

	// Parse flags with zero sentinels so we can tell "unset" from "set to
	// default": 0 for ports, "" for strings. Real defaults are applied after,
	// only when both the flag and env are unset.
	fs := flag.NewFlagSet("ensemble", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		fHTTP   = fs.Int("http-port", 0, "HTTP API base port")
		fStream = fs.Int("stream-port", 0, "stream + clock base port")
		fSource = fs.Int("source-port", 0, "audio source base port")
		fGossip = fs.Int("gossip-port", 0, "memberlist gossip base port")
		fData   = fs.String("data", "", "data directory (contains node.json)")
		fMedia  = fs.String("media", "", "media directory (default DATA_DIR/media)")
		fName   = fs.String("name", "", "initial node name (first start only)")
		fJoin   = fs.String("join", "", "dev gossip seed list: host:gossipPort,...")
		fNoMDNS = fs.Bool("no-mdns", false, "disable mDNS discovery (tests; use --join)")
	)
	if err := fs.Parse(opts.Args); err != nil {
		return nil, fmt.Errorf("config: parse flags: %w", err)
	}

	cfg := &Config{}

	var err error
	if cfg.HTTPPort, err = resolvePort(*fHTTP, getenv(EnvHTTPPort), DefaultHTTPPort, EnvHTTPPort); err != nil {
		return nil, err
	}
	if cfg.StreamPort, err = resolvePort(*fStream, getenv(EnvStreamPort), DefaultStreamPort, EnvStreamPort); err != nil {
		return nil, err
	}
	if cfg.SourcePort, err = resolvePort(*fSource, getenv(EnvSourcePort), DefaultSourcePort, EnvSourcePort); err != nil {
		return nil, err
	}
	if cfg.GossipPort, err = resolvePort(*fGossip, getenv(EnvGossipPort), DefaultGossipPort, EnvGossipPort); err != nil {
		return nil, err
	}

	// Directories: flag > env > default; then made absolute.
	dataDir := resolveString(*fData, getenv(EnvDataDir), DefaultDataDir)
	if cfg.DataDir, err = filepath.Abs(dataDir); err != nil {
		return nil, fmt.Errorf("config: resolve data dir: %w", err)
	}
	mediaDir := resolveString(*fMedia, getenv(EnvMediaDir), "")
	if mediaDir == "" {
		mediaDir = filepath.Join(cfg.DataDir, "media")
	}
	if cfg.MediaDir, err = filepath.Abs(mediaDir); err != nil {
		return nil, fmt.Errorf("config: resolve media dir: %w", err)
	}

	cfg.Output = resolveString("", getenv(EnvOutput), DefaultOutput)
	cfg.Join = parseJoin(resolveString(*fJoin, getenv(EnvJoin), ""))
	cfg.NoMDNS = *fNoMDNS || isTruthy(getenv(EnvNoMDNS))

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("config: create data dir: %w", err)
	}
	if err := os.MkdirAll(cfg.MediaDir, 0o755); err != nil {
		return nil, fmt.Errorf("config: create media dir: %w", err)
	}

	cfg.store = NewStore(cfg.DataDir)
	nf, err := cfg.store.LoadOrCreate(*fName)
	if err != nil {
		return nil, err
	}
	cfg.NodeID = nf.ID
	cfg.NodeName = nf.Name
	cfg.Volume = nf.Volume
	cfg.OutputDelayMs = nf.OutputDelayMs
	cfg.OutputDevice = nf.OutputDevice
	cfg.Disabled = nf.Disabled

	return cfg, nil
}

// Rename changes the node name and atomically rewrites node.json, updating
// c.NodeName on success only. Replicating (cluster.SetName) is the caller's job.
func (c *Config) Rename(name string) error {
	nf, err := c.store.Rename(c.NodeID, name)
	if err != nil {
		return err
	}
	c.NodeName = nf.Name
	return nil
}

// SetVolume persists the playback gain (D35) and atomically rewrites node.json,
// updating c.Volume on success only. v is clamped to [0.0, 1.0] before write.
func (c *Config) SetVolume(v float64) error {
	nf, err := c.store.SetVolume(c.NodeID, v)
	if err != nil {
		return err
	}
	c.Volume = nf.Volume
	return nil
}

// SetOutputDelayMs persists the output-delay calibration (D36) and atomically
// rewrites node.json, updating c.OutputDelayMs on success only. ms is clamped to
// [-500, 500] before write.
func (c *Config) SetOutputDelayMs(ms int) error {
	nf, err := c.store.SetOutputDelayMs(c.NodeID, ms)
	if err != nil {
		return err
	}
	c.OutputDelayMs = nf.OutputDelayMs
	return nil
}

// SetOutputDevice persists the selected ALSA output device (D37) and atomically
// rewrites node.json, updating c.OutputDevice on success only. The value is
// normalized (blank → "default", trimmed, capped) before write.
func (c *Config) SetOutputDevice(device string) error {
	nf, err := c.store.SetOutputDevice(c.NodeID, device)
	if err != nil {
		return err
	}
	c.OutputDevice = nf.OutputDevice
	return nil
}

// SetDisabled persists the operator-disabled feature list (D40) and atomically
// rewrites node.json, updating c.Disabled on success only. The list is normalized
// (subset of {playback,opus,input}, deduped + sorted) before write.
func (c *Config) SetDisabled(disabled []string) error {
	nf, err := c.store.SetDisabled(c.NodeID, disabled)
	if err != nil {
		return err
	}
	c.Disabled = nf.Disabled
	return nil
}

// NodeFilePath returns DataDir/node.json (for logs / tests).
func (c *Config) NodeFilePath() string {
	return c.store.Path()
}

// resolvePort applies flag > env > default precedence for a port knob. A flag
// value of 0 means "unset" (port 0 is meaningless for us); the env value, if
// non-empty, must be a valid 1–65535 port number.
func resolvePort(flagVal int, envVal string, def int, envName string) (int, error) {
	if flagVal != 0 {
		return validatePort(flagVal, "--"+strings.TrimPrefix(envName, "ENSEMBLE_"))
	}
	if envVal != "" {
		n, err := strconv.Atoi(envVal)
		if err != nil {
			return 0, fmt.Errorf("config: %s=%q is not a number: %w", envName, envVal, err)
		}
		return validatePort(n, envName)
	}
	return def, nil
}

func validatePort(n int, who string) (int, error) {
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("config: %s port %d out of range [1,65535]", who, n)
	}
	return n, nil
}

// resolveString applies flag > env > default precedence for a string knob.
func resolveString(flagVal, envVal, def string) string {
	if flagVal != "" {
		return flagVal
	}
	if envVal != "" {
		return envVal
	}
	return def
}

// parseJoin splits a comma-separated seed list, trims whitespace, and drops
// empty fields. An unset (empty) value yields nil, not an empty non-nil slice.
func parseJoin(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// isTruthy reports whether an env value means "on": 1/true/yes (case-insensitive).
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
