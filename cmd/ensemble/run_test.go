package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/config"
)

// TestBuildOptions exercises the flag/config precedence ladder (doc 01 §5.1,
// P0.3 §7 T1): defaults → config.yaml → only the explicitly-set flags. It drives
// the pure buildOptions so nothing binds a socket.
func TestBuildOptions(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"

	tests := []struct {
		name       string
		argv       []string
		configYAML string
		id         config.Identity
		want       func(o optView)
	}{
		{
			name: "defaults only",
			argv: nil,
			id:   config.Identity{NodeID: id},
			want: func(o optView) {
				if o.WebPort != 8443 || o.ClockPort != 9000 || o.AudioPort != 9100 || o.BindPort != 7946 {
					o.t.Errorf("ports = %d/%d/%d/%d, want 8443/9000/9100/7946", o.WebPort, o.ClockPort, o.AudioPort, o.BindPort)
				}
				if !o.UseMDNS {
					o.t.Errorf("UseMDNS = false, want true (mDNS on by default)")
				}
				if o.Name != id[:8] {
					o.t.Errorf("Name = %q, want id[:8] %q", o.Name, id[:8])
				}
			},
		},
		{
			name:       "config sets ports, no flag",
			argv:       nil,
			configYAML: "cluster:\n  clock_port: 9500\n  audio_port: 9600\n  bind_port: 7000\nweb:\n  port: 8500\n",
			id:         config.Identity{NodeID: id},
			want: func(o optView) {
				if o.WebPort != 8500 || o.ClockPort != 9500 || o.AudioPort != 9600 || o.BindPort != 7000 {
					o.t.Errorf("ports = %d/%d/%d/%d, want config 8500/9500/9600/7000", o.WebPort, o.ClockPort, o.AudioPort, o.BindPort)
				}
			},
		},
		{
			name:       "flag overrides config; unset flag keeps config",
			argv:       []string{"--web-port", "8600"},
			configYAML: "cluster:\n  clock_port: 9500\nweb:\n  port: 8500\n",
			id:         config.Identity{NodeID: id},
			want: func(o optView) {
				if o.WebPort != 8600 {
					o.t.Errorf("WebPort = %d, want 8600 (flag overrides config)", o.WebPort)
				}
				if o.ClockPort != 9500 {
					o.t.Errorf("ClockPort = %d, want 9500 (unset flag keeps config)", o.ClockPort)
				}
			},
		},
		{
			name: "no-mdns flips UseMDNS",
			argv: []string{"--no-mdns"},
			id:   config.Identity{NodeID: id},
			want: func(o optView) {
				if o.UseMDNS {
					o.t.Errorf("UseMDNS = true, want false (--no-mdns)")
				}
			},
		},
		{
			name: "repeatable join accumulates",
			argv: []string{"--join", "a:1", "--join", "b:2"},
			id:   config.Identity{NodeID: id},
			want: func(o optView) {
				if !reflect.DeepEqual(o.Seeds, []string{"a:1", "b:2"}) {
					o.t.Errorf("Seeds = %v, want [a:1 b:2]", o.Seeds)
				}
			},
		},
		{
			name:       "join accumulates onto config seeds",
			argv:       []string{"--join", "flag:1"},
			configYAML: "cluster:\n  seeds: [cfg:9]\n",
			id:         config.Identity{NodeID: id},
			want: func(o optView) {
				if !reflect.DeepEqual(o.Seeds, []string{"cfg:9", "flag:1"}) {
					o.t.Errorf("Seeds = %v, want [cfg:9 flag:1]", o.Seeds)
				}
			},
		},
		{
			name: "name precedence: flag wins",
			argv: []string{"--name", "kitchen"},
			id:   config.Identity{NodeID: id, Name: "persisted"},
			want: func(o optView) {
				if o.Name != "kitchen" {
					o.t.Errorf("Name = %q, want kitchen (flag)", o.Name)
				}
			},
		},
		{
			name:       "name precedence: config over persisted",
			argv:       nil,
			configYAML: "node_name: livingroom\n",
			id:         config.Identity{NodeID: id, Name: "persisted"},
			want: func(o optView) {
				if o.Name != "livingroom" {
					o.t.Errorf("Name = %q, want livingroom (config)", o.Name)
				}
			},
		},
		{
			name: "name precedence: persisted over id prefix",
			argv: nil,
			id:   config.Identity{NodeID: id, Name: "persisted"},
			want: func(o optView) {
				if o.Name != "persisted" {
					o.t.Errorf("Name = %q, want persisted (id.Name)", o.Name)
				}
			},
		},
		{
			name:       "web.listen false disables web port",
			argv:       nil,
			configYAML: "web:\n  listen: false\n",
			id:         config.Identity{NodeID: id},
			want: func(o optView) {
				if o.WebPort != 0 {
					o.t.Errorf("WebPort = %d, want 0 (web.listen false)", o.WebPort)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rf, set, err := parseRunFlags(tt.argv)
			if err != nil {
				t.Fatalf("parseRunFlags(%v): %v", tt.argv, err)
			}
			cfg := config.Defaults()
			if tt.configYAML != "" {
				cfg = loadConfigYAML(t, tt.configYAML)
			}
			opts := buildOptions(set, cfg, rf, config.Paths{}, tt.id, nil)
			tt.want(optView{t: t, WebPort: opts.WebPort, ClockPort: opts.ClockPort,
				AudioPort: opts.AudioPort, BindPort: opts.BindPort, UseMDNS: opts.UseMDNS,
				Name: opts.Name, Seeds: opts.Seeds, Device: opts.Device})
		})
	}
}

// optView is the flattened subset asserted by the table (keeps the want closures
// readable and carries *testing.T).
type optView struct {
	t                                       *testing.T
	WebPort, ClockPort, AudioPort, BindPort int
	UseMDNS                                 bool
	Name, Device                            string
	Seeds                                   []string
}

// loadConfigYAML writes yaml to a temp file and loads it through config.Load so
// the test exercises the real overlay/normalize path (not a hand-built struct).
func loadConfigYAML(t *testing.T, yaml string) config.Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path, true)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// TestDispatch exercises the subcommand dispatcher (P0.3 §7 T2) without calling
// os.Exit, by mirroring main's switch in a pure helper.
func TestDispatch(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
	}{
		{name: "version", args: []string{"version"}, wantCode: 0, wantOut: version},
		{name: "version short", args: []string{"-v"}, wantCode: 0, wantOut: version},
		{name: "help", args: []string{"help"}, wantCode: 0, wantOut: "ensemble"},
		{name: "no args", args: nil, wantCode: 2, wantOut: "usage"},
		{name: "unknown", args: []string{"bogus"}, wantCode: 2, wantOut: "unknown command"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			code := dispatch(tt.args, &out)
			if code != tt.wantCode {
				t.Errorf("dispatch(%v) code = %d, want %d", tt.args, code, tt.wantCode)
			}
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("dispatch(%v) out = %q, want to contain %q", tt.args, out.String(), tt.wantOut)
			}
		})
	}
}
