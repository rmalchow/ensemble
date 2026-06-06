package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultWebPort is the control-plane mTLS base port (Appendix A.12). On a bind
// conflict the node retries DefaultWebPort+1, +2, … (doc 01 §3.1/§5.1); that
// retry happens at bind time in the web/cmd layer, not here.
const DefaultWebPort = 8443

// Default ports (Appendix A.12: "control mTLS / clock UDP / audio UDP = 8443 /
// 9000 / 9100"; doc 01 §5.1 gossip --bind-port default 7946).
const (
	defaultBindPort  = 7946
	defaultClockPort = 9000
	defaultAudioPort = 9100
)

// Config holds a node's startup defaults. Values are sourced, in increasing
// order of precedence, from: built-in defaults -> the config.yaml file ->
// explicit command-line flags (the flag overlay happens in cmd via fs.Visit;
// config provides only the first two layers).
//
// The data directory itself is NOT part of this struct: it is the bootstrap
// anchor selected by --data, and is where config.yaml is looked up by default.
// This file carries only the operator startup surface (ports, mDNS, audio
// capability masking). It does NOT carry the PIN, the admin password, or per-node
// channel/gain/HWDelayUs — those are cluster/node state in the replicated
// ConfigDoc and the persisted Identity (node.json).
type Config struct {
	// NodeName is the human-friendly name shown in the UI and used as a tiebreak.
	NodeName string        `yaml:"node_name"`
	Cluster  ClusterConfig `yaml:"cluster"`
	Web      WebConfig     `yaml:"web"`
	Audio    AudioConfig   `yaml:"audio"`
}

// ClusterConfig configures peer-to-peer membership, the sync planes' ports, and
// discovery (doc 01 §5.1, Appendix A.12 ports). There is no group password /
// shared secret here: cluster trust is mTLS plus the gossip key delivered via
// adoption (A.9), not a YAML password.
type ClusterConfig struct {
	// BindPort is the memberlist gossip port (SWIM membership), default 7946.
	BindPort int `yaml:"bind_port"`
	// ClockPort is the clock-plane UDP port (4-timestamp offset estimator),
	// default 9000.
	ClockPort int `yaml:"clock_port"`
	// AudioPort is the audio-plane UDP port (streamed PCM/Opus chunks),
	// default 9100.
	AudioPort int `yaml:"audio_port"`
	// MDNS enables zeroconf announce/browse for zero-config LAN bootstrap,
	// default true. Disabled with --no-mdns.
	MDNS bool `yaml:"mdns"`
	// Seeds are explicit gossip seeds (host:port) used in addition to / instead of
	// mDNS. Mirrors the repeatable --join flag.
	Seeds []string `yaml:"seeds"`
}

// WebConfig configures the embedded control-plane web interface.
type WebConfig struct {
	// Listen enables the embedded web UI/API and defaults to true: the web wizard
	// is how an unconfigured node is provisioned, so every node serves it unless
	// explicitly disabled. A YAML that sets it to false is honored.
	Listen bool `yaml:"listen"`
	// Port is the control mTLS base port, default DefaultWebPort (8443). On a bind
	// conflict the node retries +1 at bind time (doc 01 §3.1/§5.1).
	Port int `yaml:"port"`
}

// AudioConfig is the per-node audio capability-masking schema (doc 07 §2.4.2).
// config parses and validates these keys but applies NO intersection: the
// effective-caps computation (detected ∩ enabled, doc 07 §2.4.1) is owned by
// audio/sink + group at startup. The mask is intent; the intersection with the
// live probe is what actually ships.
type AudioConfig struct {
	// Render is a tri-state: nil (key absent) => let the runtime probe decide;
	// false => force Render=false (sink-less / control-only, Sinks emptied)
	// regardless of probe; true => allow render if a backend probes (doc 07
	// §2.4.2).
	Render   *bool          `yaml:"render"`
	Backends BackendsConfig `yaml:"backends"`
	Codecs   CodecsConfig   `yaml:"codecs"`
	// Device is the optional default sink device (e.g. ALSA "hw:0"); --device and
	// Identity.Device interact with it at the cmd layer (precedence fixed there).
	Device string `yaml:"device"`
}

// BackendsConfig masks and orders the audio output backends.
type BackendsConfig struct {
	// Disable lists probed backends to never use (e.g. ["pipewire"]); removed from
	// detected before they enter Sinks.
	Disable []string `yaml:"disable"`
	// Prefer is the ordered preference among the remaining enabled+probed backends,
	// passed as the preferred list to Open() (README §6.1). Ordering only — it does
	// not add or remove caps.
	Prefer []string `yaml:"prefer"`
}

// CodecsConfig masks the wire codecs.
type CodecsConfig struct {
	// Disable lists codecs dropped from EncodeCodecs/DecodeCodecs even if the
	// encoder links (e.g. ["opus"]).
	Disable []string `yaml:"disable"`
}

// Defaults returns a Config populated with the built-in defaults (Appendix A.12).
func Defaults() Config {
	return Config{
		Cluster: ClusterConfig{
			BindPort:  defaultBindPort,
			ClockPort: defaultClockPort,
			AudioPort: defaultAudioPort,
			MDNS:      true,
		},
		Web: WebConfig{
			Listen: true,
			Port:   DefaultWebPort,
		},
		// Audio.Render stays nil (probe-driven); no masking by default.
	}
}

// Load reads YAML from path and overlays it onto the built-in defaults. Keys
// absent from the file keep their default values. A missing file is only an error
// when required is true (i.e. the path came from an explicit --config). After the
// overlay, normalize backfills any port zeroed by a partial YAML block and
// validates the audio-masking tokens.
func Load(path string, required bool) (Config, error) {
	cfg := Defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !required {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config %q: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %q: %w", path, err)
	}
	if err := cfg.normalize(); err != nil {
		return cfg, fmt.Errorf("config %q: %w", path, err)
	}
	return cfg, nil
}

// normalize runs after the overlay. It backfills any port a partial YAML block
// zeroed (so a file that sets only one sibling keeps the built-in defaults for
// the others) and validates the audio-masking tokens, erroring on an unknown
// backend/codec name so a typo cannot silently fail to mask. Booleans (MDNS,
// Listen) are intentionally NOT backfilled: an explicit false is honored.
func (cfg *Config) normalize() error {
	if cfg.Web.Port <= 0 {
		cfg.Web.Port = DefaultWebPort
	}
	if cfg.Cluster.BindPort <= 0 {
		cfg.Cluster.BindPort = defaultBindPort
	}
	if cfg.Cluster.ClockPort <= 0 {
		cfg.Cluster.ClockPort = defaultClockPort
	}
	if cfg.Cluster.AudioPort <= 0 {
		cfg.Cluster.AudioPort = defaultAudioPort
	}
	for _, b := range cfg.Audio.Backends.Disable {
		if !validBackendToken(b) {
			return fmt.Errorf("audio.backends.disable: unknown backend %q "+
				"(want \"alsa\", \"pipewire\" or \"exec:<prog>\")", b)
		}
	}
	for _, b := range cfg.Audio.Backends.Prefer {
		if !validBackendToken(b) {
			return fmt.Errorf("audio.backends.prefer: unknown backend %q "+
				"(want \"alsa\", \"pipewire\" or \"exec:<prog>\")", b)
		}
	}
	for _, c := range cfg.Audio.Codecs.Disable {
		if !validCodecToken(c) {
			return fmt.Errorf("audio.codecs.disable: unknown codec %q "+
				"(want \"pcm\" or \"opus\")", c)
		}
	}
	return nil
}

// validBackendToken reports whether s is a syntactically plausible audio backend
// token (README §6.1): the fixed names "alsa"/"pipewire" or an exec backend of
// the form "exec:<prog>" (e.g. "exec:aplay").
func validBackendToken(s string) bool {
	switch s {
	case "alsa", "pipewire":
		return true
	}
	if prog, ok := strings.CutPrefix(s, "exec:"); ok {
		return prog != ""
	}
	return false
}

// validCodecToken reports whether s is a known wire codec (README §6.5).
// "mp3"/"flac" are source decoders, not wire codecs (R2), and are rejected here.
func validCodecToken(s string) bool {
	switch s {
	case "pcm", "opus":
		return true
	}
	return false
}
