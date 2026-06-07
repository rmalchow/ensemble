package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"ensemble/internal/api"
	"ensemble/internal/contracts"
	"ensemble/internal/group"
	"ensemble/internal/stream"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseOptionsDefaults(t *testing.T) {
	opt, err := parseOptions(nil, env(nil))
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if opt.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", opt.LogLevel)
	}
	if opt.Output != "" {
		t.Errorf("Output = %q, want empty (auto)", opt.Output)
	}
	if opt.Host != "" {
		t.Errorf("Host = %q, want empty", opt.Host)
	}
}

func TestParseOptionsOutputNull(t *testing.T) {
	opt, err := parseOptions(nil, env(map[string]string{"ENSEMBLE_OUTPUT": "null"}))
	if err != nil {
		t.Fatal(err)
	}
	if opt.Output != "null" {
		t.Errorf("Output = %q, want null", opt.Output)
	}
}

func TestParseOptionsLogLevel(t *testing.T) {
	opt, _ := parseOptions(nil, env(map[string]string{"ENSEMBLE_LOG": "debug"}))
	if opt.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", opt.LogLevel)
	}
}

func TestParseOptionsHostFlag(t *testing.T) {
	for _, args := range [][]string{
		{"--host", "127.0.0.1"},
		{"--host=127.0.0.1"},
	} {
		opt, err := parseOptions(args, env(nil))
		if err != nil {
			t.Fatalf("args %v: %v", args, err)
		}
		if opt.Host != "127.0.0.1" {
			t.Errorf("args %v: Host = %q, want 127.0.0.1", args, opt.Host)
		}
		// --host must be stripped from the args forwarded to config.Load.
		for _, a := range opt.cfgArgs {
			if a == "--host" || a == "127.0.0.1" || a == "--host=127.0.0.1" {
				t.Errorf("args %v: --host leaked into cfgArgs: %v", args, opt.cfgArgs)
			}
		}
	}
}

func TestParseOptionsHostEnv(t *testing.T) {
	opt, _ := parseOptions(nil, env(map[string]string{"ENSEMBLE_HOST": "127.0.0.1"}))
	if opt.Host != "127.0.0.1" {
		t.Errorf("Host = %q, want 127.0.0.1", opt.Host)
	}
}

func TestParseOptionsForwardsConfigFlags(t *testing.T) {
	opt, err := parseOptions([]string{"--http-port", "18080", "--host", "127.0.0.1", "--name", "n1"}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"--http-port": true, "18080": true, "--name": true, "n1": true}
	got := map[string]bool{}
	for _, a := range opt.cfgArgs {
		got[a] = true
	}
	for w := range want {
		if !got[w] {
			t.Errorf("cfgArgs missing %q: %v", w, opt.cfgArgs)
		}
	}
}

func TestParseOptionsBadPort(t *testing.T) {
	if _, err := parseOptions([]string{"--http-port", "notanumber"}, env(nil)); err == nil {
		t.Fatal("expected parse error for non-numeric --http-port")
	}
}

func TestParseOptionsHostMissingArg(t *testing.T) {
	if _, err := parseOptions([]string{"--host"}, env(nil)); err == nil {
		t.Fatal("expected error for --host without value")
	}
}

func TestForcedNull(t *testing.T) {
	if !forcedNull("null") {
		t.Error("forcedNull(null) = false")
	}
	if forcedNull("auto") || forcedNull("") || forcedNull("alsa") {
		t.Error("forcedNull true for a non-null spec")
	}
}

func TestCapabilitiesNullForcesNoPlayback(t *testing.T) {
	caps := capabilities(options{Output: "null"})
	if caps.Playback {
		t.Error("Playback = true with forced null")
	}
	if !contains(caps.Sources, "file") || !contains(caps.Sources, "http") {
		t.Errorf("Sources missing file/http: %v", caps.Sources)
	}
	if !contains(caps.Codecs, "pcm") {
		t.Errorf("Codecs missing pcm: %v", caps.Codecs)
	}
	if !contains(caps.Formats, "wav") {
		t.Errorf("Formats missing wav: %v", caps.Formats)
	}
}

func TestCapabilitiesBackendsAlwaysHaveNullAndFile(t *testing.T) {
	caps := capabilities(options{Output: "null"})
	if !contains(caps.Backends, "null") || !contains(caps.Backends, "file") {
		t.Errorf("Backends missing null/file: %v", caps.Backends)
	}
}

func TestHostCIDR(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1": "127.0.0.1/32",
		"::1":       "::1/128",
		"":          "",
		"0.0.0.0":   "",
		"::":        "",
		"garbage":   "",
	}
	for in, want := range cases {
		if got := hostCIDR(in); got != want {
			t.Errorf("hostCIDR(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShutdownStackLIFO(t *testing.T) {
	var order []string
	s := &shutdownStack{}
	s.push("a", func(context.Context) error { order = append(order, "a"); return nil })
	s.push("b", func(context.Context) error { order = append(order, "b"); return errors.New("boom") })
	s.push("c", func(context.Context) error { order = append(order, "c"); return nil })

	err := s.unwind(context.Background(), newLogger("error"))
	if err == nil || err.Error() != "boom" {
		t.Errorf("unwind returned %v, want boom", err)
	}
	want := []string{"c", "b", "a"}
	if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Errorf("order = %v, want %v (all run, reverse)", order, want)
	}
}

func TestProbeGossipPortReleases(t *testing.T) {
	port, released, err := probeGossipPort("127.0.0.1", 17946, 64)
	if err != nil {
		t.Fatalf("probeGossipPort: %v", err)
	}
	if !released {
		t.Fatal("probeGossipPort did not report release")
	}
	// Both TCP and UDP must be immediately re-bindable (proves probe-release, D8).
	tcp, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		t.Fatalf("TCP not re-bindable on %d: %v", port, err)
	}
	tcp.Close()
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		t.Fatalf("UDP not re-bindable on %d: %v", port, err)
	}
	udp.Close()
}

func TestMapErrTranslatesToAPISentinels(t *testing.T) {
	cases := []struct {
		in   error
		want error
	}{
		{group.ErrNotMaster, api.ErrNotMaster},
		{group.ErrTargetUnknown, api.ErrUnknownNode},
		{group.ErrTargetDead, api.ErrNotAlive},
		{group.ErrTargetFollower, api.ErrTargetNotMaster},
		{group.ErrNoOpus, api.ErrNoCodec},
		{group.ErrBadSettings, api.ErrNoCodec},
	}
	for _, c := range cases {
		got := mapErr(c.in)
		if !errors.Is(got, c.want) {
			t.Errorf("mapErr(%v): errors.Is(_, %v) = false", c.in, c.want)
		}
		// Original message must be preserved.
		if got.Error() != c.in.Error() {
			t.Errorf("mapErr(%v).Error() = %q, want %q", c.in, got.Error(), c.in.Error())
		}
	}
	if mapErr(nil) != nil {
		t.Error("mapErr(nil) != nil")
	}
}

func TestMapErrPreservesOpusNodeNames(t *testing.T) {
	// Simulate the engine's wrapped opus error (Play wraps ErrNoOpus with names).
	wrapped := fmt.Errorf("%w: alice, bob", group.ErrNoOpus)
	got := mapErr(wrapped)
	if !errors.Is(got, api.ErrNoCodec) {
		t.Fatal("wrapped opus error not mapped to ErrNoCodec")
	}
	if got.Error() != wrapped.Error() {
		t.Errorf("message = %q, want %q", got.Error(), wrapped.Error())
	}
}

// fakeSink records the gens it was Reset to and the frames pushed, for the
// deliver-callback test.
type fakeSink struct {
	resets []uint32
	pushes []uint32
}

func (f *fakeSink) Push(gen uint32, seq uint64, pts int64, payload []byte) {
	f.pushes = append(f.pushes, gen)
}
func (f *fakeSink) Reset(gen uint32)           { f.resets = append(f.resets, gen) }
func (f *fakeSink) Disarm()                    {}
func (f *fakeSink) Stats() contracts.SinkStats { return contracts.SinkStats{} }
func (f *fakeSink) SetGain(float64)            {}
func (f *fakeSink) SetDelayOffset(int64)       {}
func (f *fakeSink) Close() error               { return nil }

func TestNewDeliverPCMResetsOnGenChange(t *testing.T) {
	fs := &fakeSink{}
	d := newDeliver(fs, newDisableState(nil), newLogger("error"))
	pcm := make([]byte, stream.FrameBytes)

	d(stream.Header{Gen: 7, Seq: 0, PTS: 0}, pcm)
	d(stream.Header{Gen: 7, Seq: 1, PTS: 0}, pcm)
	d(stream.Header{Gen: 9, Seq: 0, PTS: 0}, pcm) // gen change → re-arm

	if len(fs.resets) != 2 || fs.resets[0] != 7 || fs.resets[1] != 9 {
		t.Errorf("resets = %v, want [7 9]", fs.resets)
	}
	if len(fs.pushes) != 3 {
		t.Errorf("pushes = %d, want 3", len(fs.pushes))
	}
	for _, g := range fs.pushes {
		if g != 7 && g != 9 {
			t.Errorf("unexpected push gen %d", g)
		}
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
