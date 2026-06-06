package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefaults(t *testing.T) {
	d := Defaults()
	if d.Web.Port != 8443 {
		t.Errorf("Web.Port = %d, want 8443", d.Web.Port)
	}
	if !d.Web.Listen {
		t.Errorf("Web.Listen = false, want true")
	}
	if d.Cluster.BindPort != 7946 {
		t.Errorf("Cluster.BindPort = %d, want 7946", d.Cluster.BindPort)
	}
	if d.Cluster.ClockPort != 9000 {
		t.Errorf("Cluster.ClockPort = %d, want 9000", d.Cluster.ClockPort)
	}
	if d.Cluster.AudioPort != 9100 {
		t.Errorf("Cluster.AudioPort = %d, want 9100", d.Cluster.AudioPort)
	}
	if !d.Cluster.MDNS {
		t.Errorf("Cluster.MDNS = false, want true")
	}
	if d.Audio.Render != nil {
		t.Errorf("Audio.Render = %v, want nil (probe-driven)", *d.Audio.Render)
	}
}

func TestLoadMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")

	// required=false -> Defaults(), no error.
	cfg, err := Load(missing, false)
	if err != nil {
		t.Fatalf("Load(required=false): %v", err)
	}
	if !reflect.DeepEqual(cfg, Defaults()) {
		t.Errorf("Load(required=false) = %+v, want Defaults()", cfg)
	}

	// required=true -> wrapped read error.
	if _, err := Load(missing, true); err == nil {
		t.Fatalf("Load(required=true): want error, got nil")
	} else if !strings.Contains(err.Error(), "read config") {
		t.Errorf("error %q does not mention \"read config\"", err)
	}
}

// writeYAML writes a config.yaml in a fresh temp dir and returns its path.
func writeYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return path
}

func TestLoadFullOverlay(t *testing.T) {
	const body = `
node_name: studio
cluster:
  bind_port: 17946
  clock_port: 19000
  audio_port: 19100
  mdns: false
  seeds:
    - 10.0.0.1:7946
    - 10.0.0.2:7946
web:
  listen: false
  port: 18443
audio:
  render: true
  device: hw:1
  backends:
    disable: [pipewire]
    prefer: [alsa, exec:aplay]
  codecs:
    disable: [opus]
`
	cfg, err := Load(writeYAML(t, body), true)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NodeName != "studio" {
		t.Errorf("NodeName = %q", cfg.NodeName)
	}
	wantCluster := ClusterConfig{
		BindPort: 17946, ClockPort: 19000, AudioPort: 19100,
		MDNS: false, Seeds: []string{"10.0.0.1:7946", "10.0.0.2:7946"},
	}
	if !reflect.DeepEqual(cfg.Cluster, wantCluster) {
		t.Errorf("Cluster = %+v, want %+v", cfg.Cluster, wantCluster)
	}
	if cfg.Web.Listen || cfg.Web.Port != 18443 {
		t.Errorf("Web = %+v, want {Listen:false Port:18443}", cfg.Web)
	}
	if cfg.Audio.Render == nil || !*cfg.Audio.Render {
		t.Errorf("Audio.Render = %v, want non-nil true", cfg.Audio.Render)
	}
	if cfg.Audio.Device != "hw:1" {
		t.Errorf("Audio.Device = %q, want hw:1", cfg.Audio.Device)
	}
	if !reflect.DeepEqual(cfg.Audio.Backends.Disable, []string{"pipewire"}) {
		t.Errorf("Backends.Disable = %v", cfg.Audio.Backends.Disable)
	}
	if !reflect.DeepEqual(cfg.Audio.Backends.Prefer, []string{"alsa", "exec:aplay"}) {
		t.Errorf("Backends.Prefer = %v", cfg.Audio.Backends.Prefer)
	}
	if !reflect.DeepEqual(cfg.Audio.Codecs.Disable, []string{"opus"}) {
		t.Errorf("Codecs.Disable = %v", cfg.Audio.Codecs.Disable)
	}
}

func TestLoadPartialBlockBackfill(t *testing.T) {
	// Only bind_port is set; the zeroed siblings must be backfilled.
	cfg, err := Load(writeYAML(t, "cluster:\n  bind_port: 7000\n"), true)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Cluster.BindPort != 7000 {
		t.Errorf("BindPort = %d, want 7000", cfg.Cluster.BindPort)
	}
	if cfg.Cluster.ClockPort != 9000 {
		t.Errorf("ClockPort = %d, want backfilled 9000", cfg.Cluster.ClockPort)
	}
	if cfg.Cluster.AudioPort != 9100 {
		t.Errorf("AudioPort = %d, want backfilled 9100", cfg.Cluster.AudioPort)
	}
	if cfg.Web.Port != 8443 {
		t.Errorf("Web.Port = %d, want backfilled 8443", cfg.Web.Port)
	}
}

func TestLoadWebListenFalseHonored(t *testing.T) {
	cfg, err := Load(writeYAML(t, "web:\n  listen: false\n"), true)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Web.Listen {
		t.Errorf("Web.Listen = true, want explicit false honored")
	}
	// Port still backfills to its default.
	if cfg.Web.Port != 8443 {
		t.Errorf("Web.Port = %d, want 8443", cfg.Web.Port)
	}
}

func TestLoadRenderTriState(t *testing.T) {
	tests := []struct {
		name string
		body string
		want *bool
	}{
		{"absent", "audio:\n  device: hw:0\n", nil},
		{"true", "audio:\n  render: true\n", boolPtr(true)},
		{"false", "audio:\n  render: false\n", boolPtr(false)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeYAML(t, tt.body), true)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			switch {
			case tt.want == nil && cfg.Audio.Render != nil:
				t.Errorf("Render = %v, want nil", *cfg.Audio.Render)
			case tt.want != nil && cfg.Audio.Render == nil:
				t.Errorf("Render = nil, want %v", *tt.want)
			case tt.want != nil && *cfg.Audio.Render != *tt.want:
				t.Errorf("Render = %v, want %v", *cfg.Audio.Render, *tt.want)
			}
		})
	}
}

func TestLoadValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantSub string
	}{
		{"unknown backend", "audio:\n  backends:\n    disable: [bogus]\n", "unknown backend"},
		{"unknown codec", "audio:\n  codecs:\n    disable: [mp3]\n", "unknown codec"},
		{"malformed yaml", "audio: [this is: not valid\n", "parse config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeYAML(t, tt.body), true)
			if err == nil {
				t.Fatalf("want error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q does not contain %q", err, tt.wantSub)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }
