package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/config"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/daemon"
)

// runFlags is the parsed run flag set (doc 01 §5.1). The defaults come from
// config.Defaults() / Appendix A.12; the only baked-in literal here is the data
// dir, since it is the bootstrap anchor (config.yaml is looked up under it).
type runFlags struct {
	dataDir    string
	configPath string
	name       string
	webPort    int
	clockPort  int
	audioPort  int
	bindPort   int
	join       stringSlice
	noMDNS     bool
	device     string
	verbose    bool
}

// parseRunFlags defines and parses the run flag set (doc 01 §5.1) with A.12
// defaults, and returns the flags plus the set of flags the user explicitly
// passed (fs.Visit). cmd uses that set so that only the EXPLICITLY-set flags
// override config.yaml (doc 01 §5.1: "only the set flags override"). Split from
// cmdRun so tests can assert the parse + overlay without binding sockets.
func parseRunFlags(args []string) (*runFlags, map[string]bool, error) {
	d := config.Defaults()
	rf := &runFlags{}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.StringVar(&rf.dataDir, "data", config.DefaultDataDir, "data directory (config + identity + certs + media)")
	fs.StringVar(&rf.configPath, "config", "", "explicit config.yaml (default <data>/config.yaml)")
	fs.StringVar(&rf.name, "name", "", "node friendly name (overrides config / persisted identity)")
	fs.IntVar(&rf.webPort, "web-port", d.Web.Port, "control-plane HTTPS base port (retries +1 on conflict)")
	fs.IntVar(&rf.clockPort, "clock-port", d.Cluster.ClockPort, "clock-plane UDP port")
	fs.IntVar(&rf.audioPort, "audio-port", d.Cluster.AudioPort, "audio-plane UDP port")
	fs.IntVar(&rf.bindPort, "bind-port", d.Cluster.BindPort, "memberlist gossip port")
	fs.Var(&rf.join, "join", "explicit gossip seed host:port (repeatable)")
	fs.BoolVar(&rf.noMDNS, "no-mdns", false, "disable mDNS announce/browse")
	fs.StringVar(&rf.device, "device", "", "audio sink device (e.g. ALSA hw:0)")
	fs.BoolVar(&rf.verbose, "v", false, "verbose cluster/engine logs")
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return rf, set, nil
}

// buildOptions overlays, in increasing precedence, the built-in defaults (in
// cfg), the config.yaml (already merged into cfg by config.Load), and the
// EXPLICITLY-set flags (set), then resolves the friendly name and seed set, and
// returns the daemon.Options the daemon consumes (doc 01 §5.1/§5.2). It is pure
// (no I/O) so the precedence rules are table-testable.
//
//   - ports / device take the config value unless the matching flag was set;
//   - --no-mdns flips UseMDNS off (config's cluster.mdns otherwise applies);
//   - --join accumulates onto config's cluster.seeds;
//   - name precedence: --name / config.node_name → persisted id.Name → id[:8].
func buildOptions(set map[string]bool, cfg config.Config, rf *runFlags, paths config.Paths, id config.Identity, log io.Writer) daemon.Options {
	webPort := cfg.Web.Port
	if set["web-port"] {
		webPort = rf.webPort
	}
	clockPort := cfg.Cluster.ClockPort
	if set["clock-port"] {
		clockPort = rf.clockPort
	}
	audioPort := cfg.Cluster.AudioPort
	if set["audio-port"] {
		audioPort = rf.audioPort
	}
	bindPort := cfg.Cluster.BindPort
	if set["bind-port"] {
		bindPort = rf.bindPort
	}

	useMDNS := cfg.Cluster.MDNS
	if set["no-mdns"] && rf.noMDNS {
		useMDNS = false
	}

	device := cfg.Audio.Device
	if set["device"] {
		device = rf.device
	}

	// Name precedence: explicit flag/config node_name → persisted id.Name → id[:8].
	name := cfg.NodeName
	if set["name"] {
		name = rf.name
	}
	if name == "" {
		name = id.Name
	}
	if name == "" && len(id.NodeID) >= 8 {
		name = id.NodeID[:8]
	} else if name == "" {
		name = id.NodeID
	}

	// Seeds: config.cluster.seeds ∪ --join (config first, then the repeatable flag).
	seeds := append(append([]string{}, cfg.Cluster.Seeds...), []string(rf.join)...)

	// WebPort 0 disables the web UI (config web.listen: false). The daemon treats
	// 0 as "no web listener".
	if !cfg.Web.Listen {
		webPort = 0
	}

	return daemon.Options{
		Paths:     paths,
		NodeID:    id.NodeID,
		Name:      name,
		WebPort:   webPort,
		ClockPort: clockPort,
		AudioPort: audioPort,
		BindPort:  bindPort,
		Seeds:     seeds,
		UseMDNS:   useMDNS,
		Device:    device,
		Log:       log,
		Version:   version,
	}
}

// cmdRun is the run daemon: it resolves the data dir + identity (doc 01 §5.2),
// loads config.yaml, overlays the explicitly-set flags (doc 01 §5.1), builds the
// daemon.Options, and dispatches to daemon.Run, which blocks until SIGINT/SIGTERM.
func cmdRun(argv []string) error {
	rf, set, err := parseRunFlags(argv)
	if err != nil {
		return err
	}

	paths, err := config.OpenDataDir(rf.dataDir)
	if err != nil {
		return err
	}

	// --config when set is REQUIRED (a load error is fatal); unset defaults to
	// <data>/config.yaml and is optional (doc 01 §5.1 B2).
	cfgPath := rf.configPath
	required := cfgPath != ""
	if cfgPath == "" {
		cfgPath = filepath.Join(paths.Root, "config.yaml")
	}
	cfg, err := config.Load(cfgPath, required)
	if err != nil {
		return err
	}

	id, err := config.LoadOrCreateIdentity(paths)
	if err != nil {
		return err
	}

	var log io.Writer
	if rf.verbose {
		log = os.Stderr
	}

	opts := buildOptions(set, cfg, rf, paths, id, log)

	fmt.Printf("ensemble %s  node=%s name=%q\n", version, shortID(id.NodeID), opts.Name)
	fmt.Printf("  data=%s  web=:%d  clock=:%d  audio=:%d  bind=:%d  mdns=%v\n",
		paths.Root, opts.WebPort, opts.ClockPort, opts.AudioPort, opts.BindPort, opts.UseMDNS)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return daemon.Run(ctx, opts)
}

// shortID returns the first 8 chars of an id for the startup banner.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
