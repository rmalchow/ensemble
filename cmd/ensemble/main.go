// Command ensemble is the single self-organizing multiroom audio binary.
// Every node runs this. main parses flags+env, binds the four ports, probes
// host capabilities, builds the component graph bottom-up (S→A→B/C→F/G→E→H→I),
// runs it, and tears it down in reverse on SIGINT/SIGTERM (piece K).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"ensemble/internal/api"
	"ensemble/internal/audio"
	"ensemble/internal/clock"
	"ensemble/internal/cluster"
	"ensemble/internal/config"
	"ensemble/internal/contracts"
	"ensemble/internal/discovery"
	"ensemble/internal/dl"
	"ensemble/internal/group"
	"ensemble/internal/id"
	"ensemble/internal/netx"
	"ensemble/internal/playback"
	"ensemble/internal/sink"
	"ensemble/internal/source"
	"ensemble/internal/spotify"
	"ensemble/internal/stream"
	"ensemble/web"
)

// options is the fully-resolved configuration after flags+env. Ports/dirs are
// resolved by config.Load (A); options only carries the K-owned knobs (--host,
// ENSEMBLE_OUTPUT, ENSEMBLE_LOG) plus the raw flag args forwarded to config.Load.
type options struct {
	Host     string   // --host bind address; "" => all interfaces, "127.0.0.1" in dev/e2e
	Output   string   // --output / ENSEMBLE_OUTPUT (D2): "" => auto | null | file:<p> | name
	LogLevel string   // ENSEMBLE_LOG (debug|info|warn|error), default info
	cfgArgs  []string // flag args forwarded to config.Load (--host stripped)
}

// version is stamped by the build (scripts/build.sh / CI: -X main.version=…).
var version = "dev"

func main() {
	{ // --version, tolerating the `run` alias prefix.
		a := os.Args[1:]
		if len(a) > 0 && a[0] == "run" {
			a = a[1:]
		}
		if len(a) == 1 && (a[0] == "--version" || a[0] == "-version") {
			fmt.Println("ensemble", version)
			return
		}
	}
	opt, err := parseOptions(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ensemble:", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, opt); err != nil {
		fmt.Fprintln(os.Stderr, "ensemble:", err)
		os.Exit(1)
	}
}

// parseOptions extracts the K-owned knobs (--host, --output, ENSEMBLE_LOG)
// and forwards the remaining flag args to config.Load (A owns flag>env>default for
// ports/dirs/name/join). It parses with a permissive FlagSet so unknown flags
// (the config ones) are passed through untouched. --host and --output follow the
// usual flag>env>default precedence (flag overrides the ENSEMBLE_* fallback).
func parseOptions(args []string, env func(string) string) (options, error) {
	opt := options{
		LogLevel: env("ENSEMBLE_LOG"),
	}
	if opt.LogLevel == "" {
		opt.LogLevel = "info"
	}

	// Pull --host / --output (and the =v forms) out of args; everything else
	// goes to config.Load. "" means the flag was absent → fall back to env below.
	var host, output string
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case i == 0 && a == "run":
			// v1-CLI muscle-memory alias: `ensemble run …` == `ensemble …`.
			// Without this, Go's flag parser would stop at the positional and
			// silently ignore EVERY following flag (--data included!).
		case a == "-v" || a == "--verbose":
			opt.LogLevel = "debug"
		case a == "--host" || a == "-host":
			if i+1 >= len(args) {
				return opt, errors.New("flag needs an argument: --host")
			}
			host = args[i+1]
			i++
		case strings.HasPrefix(a, "--host="):
			host = strings.TrimPrefix(a, "--host=")
		case strings.HasPrefix(a, "-host="):
			host = strings.TrimPrefix(a, "-host=")
		case a == "--output" || a == "-output":
			if i+1 >= len(args) {
				return opt, errors.New("flag needs an argument: --output")
			}
			output = args[i+1]
			i++
		case strings.HasPrefix(a, "--output="):
			output = strings.TrimPrefix(a, "--output=")
		case strings.HasPrefix(a, "-output="):
			output = strings.TrimPrefix(a, "-output=")
		default:
			rest = append(rest, a)
		}
	}
	if host == "" {
		host = env("ENSEMBLE_HOST")
	}
	if output == "" {
		output = env("ENSEMBLE_OUTPUT")
	}
	opt.Host = host
	opt.Output = output
	opt.cfgArgs = rest

	// Validate the config flags up front (no panic on a bad port etc.) without
	// committing to side effects: a dry parse via the same FlagSet config uses.
	if err := validateConfigFlags(rest); err != nil {
		return opt, err
	}
	return opt, nil
}

// validateConfigFlags runs the same flag definitions config.Load uses so a bad
// flag/port is a clean parse error here, not a panic deeper in.
func validateConfigFlags(args []string) error {
	fs := flag.NewFlagSet("ensemble", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Int("http-port", 0, "")
	fs.Int("stream-port", 0, "")
	fs.Int("source-port", 0, "")
	fs.Int("control-port", 0, "")
	fs.Int("gossip-port", 0, "")
	fs.String("role", "", "")
	fs.String("data", "", "")
	fs.String("media", "", "")
	fs.String("name", "", "")
	fs.String("join", "", "")
	fs.Bool("no-mdns", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Go's flag parser STOPS at the first positional; anything left over means
	// part of the command line was about to be silently ignored. Refuse loudly.
	if extra := fs.Args(); len(extra) > 0 {
		return fmt.Errorf("unexpected argument %q (flags only; flags after a positional would be ignored)", extra[0])
	}
	return nil
}

// run builds the component graph, starts it, blocks until ctx is cancelled, then
// unwinds the shutdown stack in reverse. Returns the first fatal/shutdown error.
func run(ctx context.Context, opt options) (rerr error) {
	// base carries no comp attr: components attach their own comp=… exactly
	// once. main's own lines use log (comp=main).
	base := newLogger(opt.LogLevel)
	log := base.With("comp", "main")

	// 1. config / node.json (A). Fatal on error — never mint a fresh id over a
	//    corrupt file (§4).
	cfg, err := config.Load(config.Options{Args: opt.cfgArgs})
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	log.Info("starting",
		"version", version, "id", cfg.NodeID.String(), "name", cfg.NodeName,
		"role", cfg.Role.String(),
		"output", outputLabel(opt.Output), "media", cfg.MediaDir, "logLevel", opt.LogLevel,
	)

	// role=playback (no master): a non-gossiping, receive-only node (D49/D50/D61).
	// Minimal bring-up — no gossip, source server, group engine, or HTTP API; it is
	// discovered and driven by a master over the control plane.
	if cfg.Role.Playback && !cfg.Role.Master {
		return runPlayback(ctx, opt, cfg, base)
	}

	// A LIFO of teardown closures; unwound in reverse on shutdown (§3.3).
	stack := &shutdownStack{}
	// On an EARLY (pre-ready) failure, unwind whatever we acquired so far.
	defer func() {
		if rerr != nil {
			sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = stack.unwind(sc, log)
			cancel()
		}
	}()

	// 2. Port binds — pinned (flag/env) ports bind EXACTLY or fail; unset ports
	//    bind-or-increment (§2). Capture the ACTUAL bound port each.
	streamTCP, streamUDP, streamPort, err := netx.BindTCPUDP(opt.Host, cfg.StreamPort, bindTries(cfg.ExplicitPorts.Stream))
	if err != nil {
		return portBindErr("stream", cfg.StreamPort, cfg.ExplicitPorts.Stream, err)
	}
	stack.push("stream listeners", func(context.Context) error {
		_ = streamTCP.Close()
		return nil
	})

	srcTCP, srcUDP, sourcePort, err := netx.BindTCPUDP(opt.Host, cfg.SourcePort, bindTries(cfg.ExplicitPorts.Source))
	if err != nil {
		return portBindErr("source", cfg.SourcePort, cfg.ExplicitPorts.Source, err)
	}
	stack.push("source sockets", func(context.Context) error {
		_ = srcTCP.Close()
		_ = srcUDP.Close()
		return nil
	})

	httpLn, httpPort, err := netx.BindTCP(opt.Host, cfg.HTTPPort, bindTries(cfg.ExplicitPorts.HTTP))
	if err != nil {
		return portBindErr("http", cfg.HTTPPort, cfg.ExplicitPorts.HTTP, err)
	}
	stack.push("http listener", func(context.Context) error {
		_ = httpLn.Close()
		return nil
	})

	gossipPort, gossipReleased, err := probeGossipPort(opt.Host, cfg.GossipPort, bindTries(cfg.ExplicitPorts.Gossip))
	if err != nil {
		return portBindErr("gossip", cfg.GossipPort, cfg.ExplicitPorts.Gossip, err)
	}

	// PORTS (§2): one consistent line per actually-bound port at startup.
	host := opt.Host
	if host == "" {
		host = "0.0.0.0"
	}
	log.Info("port bound", "service", "http", "proto", "tcp", "host", host, "port", httpPort)
	log.Info("port bound", "service", "stream", "proto", "tcp", "host", host, "port", streamPort)
	log.Info("port bound", "service", "stream", "proto", "udp", "host", host, "port", streamPort)
	log.Info("port bound", "service", "source", "proto", "tcp", "host", host, "port", sourcePort)
	log.Info("port bound", "service", "source", "proto", "udp", "host", host, "port", sourcePort)
	log.Info("port bound", "service", "gossip", "proto", "tcp+udp", "host", host, "port", gossipPort, "probeReleased", gossipReleased)

	// 3. Addresses (§3.1). When bound to a SPECIFIC --host, only that address is
	//    actually reachable, so advertise exactly it (critical on loopback dev/e2e:
	//    advertising unbound interface CIDRs would make peers — and our own clock
	//    self-dial — pick an address nothing listens on). On the wildcard bind,
	//    advertise the node's real interface CIDRs (§3.1).
	var addrs []string
	if h := hostCIDR(opt.Host); h != "" {
		addrs = []string{h}
	} else {
		addrs = netx.InterfaceCIDRs()
	}

	// 4. Capabilities (D3/D32): $PATH probe + dlopen probes + static lists.
	caps := capabilities(opt)

	// role=master (no playback piece): a pure controller/source. It still gossips,
	// sources audio to other rooms, serves the API, and drives remote playback
	// nodes — but never plays locally. Realized as the null sink + playback:false
	// (D3): the playback piece is off, the master piece runs unchanged.
	output := opt.Output
	if !cfg.Role.Playback {
		output = "null"
		caps.Playback = false
		log.Info("role=master: playback piece disabled (null sink, playback:false)")
	}

	// 4b. Output-device enumeration (D37, §8.5): parse /proc/asound/pcm when the
	//     alsa backend is loadable. Empty on hosts without ALSA/libasound.
	outputDevices := sink.ListOutputDevices()
	log.Info("output devices", "devices", deviceIDs(outputDevices))

	// 4c. Capture-device enumeration: PipeWire sources or ALSA capture PCMs,
	//     offered as the device for `input:` (line-in) playback.
	inputDevices := audio.ListInputDevices()
	log.Info("input devices", "count", len(inputDevices))

	// 5. UDP mux over STREAM_PORT (not yet Run).
	mux := stream.NewMux(streamUDP, base)

	// 6. Cluster (memberlist on the probed gossip port; impls StateStore). The
	//    discovery Peers channel is consumed by the cluster's own join loop.
	var disc *discovery.Discovery
	if cfg.NoMDNS {
		log.Info("mDNS discovery disabled (--no-mdns); gossip relies on --join seeds")
	} else {
		disc = discovery.New(discovery.Config{
			ID: cfg.NodeID,
			// Role drives the advert (D49/D50): a master advertises its four ports;
			// a playback-only node advertises control + caps. A combined node
			// advertises as master (local playout is driven in-process, D61). The
			// playback-only advert's ControlPort/Caps are wired with that bring-up.
			Master:     cfg.Role.Master,
			Playback:   cfg.Role.Playback,
			GossipPort: gossipPort,
			HTTPPort:   httpPort,
			StreamPort: streamPort,
			SourcePort: sourcePort,
			Log:        base,
		})
	}
	var peers <-chan discovery.Peer
	if disc != nil {
		peers = disc.Peers()
	}
	cl, err := cluster.New(cluster.Config{
		Self:             cfg.NodeID,
		Name:             cfg.NodeName,
		Volume:           cfg.Volume,
		OutputDelayMs:    cfg.OutputDelayMs,
		OutputDevice:     cfg.OutputDevice,
		OutputDevices:    outputDevices,
		InputDevices:     inputDevices,
		Caps:             caps,
		Disabled:         cfg.Disabled,
		SpotifyEndpoints: cfg.SpotifyEndpoints, // D57: seed presets from node.json so the snapshot has them on boot
		InitialFollowing: cfg.Following,        // D45: rejoin previous group on return
		Addrs:            addrs,
		HTTPPort:         httpPort,
		StreamPort:       streamPort,
		SourcePort:       sourcePort,
		GossipPort:       gossipPort,
		BindAddr:         opt.Host,
		Peers:            peers,
		StatePath:        filepath.Join(cfg.DataDir, "cluster.json"),
		Logger:           base,
	})
	if err != nil {
		return fmt.Errorf("cluster: %w", err)
	}

	// 7. Clock server (passive) + follower (contracts.Clock).
	clockSrv := clock.NewServer(mux, base)
	clockSrv.Start() // registers 0x10 on the mux (idempotent, mux not yet running)
	clockFol := clock.NewFollower(mux, base)

	// 8. Sink backend + playout. ENSEMBLE_OUTPUT=null forces null (§8.5/D27).
	//    Otherwise the output is the resilient failover chain: it tries every real
	//    output (the configured device first) until one works, rotates on failure,
	//    and rests with backoff after repeated whole-chain failures (D37).
	backend, backendName, err := sink.OpenResilient(output, cfg.OutputDevice, base)
	if err != nil {
		return fmt.Errorf("sink backend %q: %w", opt.Output, err)
	}
	cl.SetOutputBackend(backendName) // report the CHOSEN backend so the UI can show it
	if ar, ok := backend.(sink.ActiveReporter); ok {
		// Surface the live output kind as the chain opens / rotates outputs.
		ar.OnActive(func(kind string) { cl.SetOutputBackend(kind) })
	}
	theSink := sink.New(sink.Config{
		Backend:       backend,
		Clock:         clockFol,
		BufferMs:      contracts.DefaultBufferMs,
		Volume:        cfg.Volume,
		OutputDelayMs: cfg.OutputDelayMs,
		Log:           base,
	})

	// 9. Source server (master-side; idle until a session runs) on SOURCE_PORT.
	srcSrv := source.NewServer(source.Config{
		Self: cfg.NodeID,
		UDP:  srcUDP,
		TCP:  srcTCP,
		Log:  base,
		// D60: a playback node's STATUS refreshes its liveness, so an actively-driven
		// node stays alive even if its mDNS re-announce lapses.
		OnStatus: cl.TouchPlaybackNode,
	})

	// 10. Subscriber client (member-side). The deliver closure decodes opus when
	//     the payload is a compressed packet (not a full PCM frame), re-arms the
	//     sink on a generation change, and pushes canonical PCM to the sink.
	// Operator-disabled features (D40): a live, atomic set the local media/opus
	// factories + deliver path consult so disabling opus/input refuses locally too
	// (effective caps already gate new sessions cluster-wide).
	disabled := newDisableState(cfg.Disabled)

	subClient := stream.NewClient(stream.ClientConfig{
		Mux:     mux,
		Deliver: newDeliver(theSink, disabled, base),
		Log:     base,
	})

	// 11. Media + opus factories (D's audio package), bound to MediaDir.
	media := mediaFactory{mediaDir: cfg.MediaDir, disabled: disabled}
	var opusFac group.OpusFactory
	if audio.OpusAvailable() {
		opusFac = opusFactory{log: log, disabled: disabled}
	} else {
		log.Debug("opus unavailable (libopus not loadable)")
	}

	// 12. Group engine (H).
	engine := group.New(group.Params{
		Cluster:  cl,
		Media:    media,
		Opus:     opusFac,
		Source:   srcSrv,
		Sub:      subClient,
		Sink:     theSink,
		Clock:    clockFol,
		ClockCtl: clockFol,
		Caps:     caps,
		Log:      base,
		// D45: persist every engine-driven follow change to node.json so this
		// node rejoins its previous group after a temporary disappearance.
		PersistFollowing: func(target id.ID) {
			if err := cfg.SetFollowing(target); err != nil {
				log.Warn("persist following failed", "target", target.String(), "err", err)
			}
		},
	})

	// 12b. Spotify bridge manager (D57): owns the default Connect device plus one
	//      go-librespot bridge per configured preset. Constructed now (no processes)
	//      so the API can reconcile/rename it; started during ACTIVATE below. nil
	//      when no go-librespot binary is present.
	var spotMgr *spotify.Manager
	if bin := audio.FindSpotifyBinary(); bin != "" {
		spotMgr = spotify.NewManager(bin, cfg.DataDir, cfg.NodeName, engine, cl, base)
	}
	var spotifyCtl api.Spotify
	if spotMgr != nil {
		spotifyCtl = spotMgr // typed-nil guard: a nil *Manager must stay a nil interface
	}

	// 13. API server (I) on the bound HTTP listener. Group is wired via an adapter
	//     that forwards ctx and maps group errors to api sentinels.
	distFS, err := fs.Sub(web.DistFS, "dist")
	if err != nil {
		distFS = nil
	}
	apiSrv := api.New(api.Config{
		Cluster: cl,
		Group:   &groupAdapter{e: engine, cl: cl},
		Media:   api.NewMediaLister(cfg.MediaDir),
		NodeCfg: cfg,
		Spotify: spotifyCtl,
		Stats:   statusStats(theSink, clockFol, engine),
		PlaybackStatuses: func() []api.PlaybackStat {
			m := srcSrv.Statuses()
			out := make([]api.PlaybackStat, 0, len(m))
			for nid, ps := range m {
				st := ps.Status
				out = append(out, api.PlaybackStat{
					NodeID:          nid.String(),
					Synced:          st.Synced,
					Playing:         st.Playing,
					OffsetNs:        st.OffsetNs,
					RTTNs:           st.RTTNs,
					RatePPM:         float64(st.RatePPMx1000) / 1000.0,
					PhaseErrNs:      st.PhaseErrNs,
					DeviceDelayNs:   st.DeviceDelayNs,
					Buffered:        int(st.Buffered),
					Played:          st.Played,
					Silence:         st.Silence,
					Late:            st.Late,
					Calibrated:      st.Calibrated,
					SamplesInjected: st.SamplesInjected,
					SamplesDropped:  st.SamplesDropped,
					AgeMs:           time.Since(ps.LastSeen).Milliseconds(),
				})
			}
			return out
		},
		Sink:              func() api.SinkControl { return theSink },
		ApplyOutputDevice: applyOutputDevice(backendName, theSink, base),
		ApplyDisabled:     applyDisabled(disabled, output, cfg.OutputDevice, theSink, base),
		Ports: api.PortsResp{
			HTTP:   httpPort,
			Stream: streamPort,
			Source: sourcePort,
			Gossip: gossipPort,
		},
		Listener: httpLn,
		DistFS:   distFS,
		Log:      base,
	})

	// 14. SEED + ACTIVATE. Closers are pushed in acquisition order so the LIFO
	//     unwind (§3.3) tears down in the intended reverse: disc → api → engine →
	//     subscriber → source → sink → clock → cluster → mux. engine before
	//     cluster so the master writes idle before we Leave.
	mux.Run()
	stack.push("mux", func(context.Context) error { return mux.Close() })

	if err := cl.Start(); err != nil {
		return fmt.Errorf("cluster start: %w", err)
	}
	stack.push("cluster", func(sc context.Context) error { return cl.Close() })

	if len(cfg.Join) > 0 {
		if err := cl.Join(cfg.Join); err != nil {
			log.Warn("gossip seed join failed (non-fatal)", "seeds", cfg.Join, "err", err)
		}
	}

	clockFol.Start()
	stack.push("clock follower", func(context.Context) error { return clockFol.Close() })

	stack.push("sink", func(context.Context) error { return theSink.Close() })

	srcSrv.Run()
	stack.push("source server", func(context.Context) error { return srcSrv.Close() })

	// Subscriber client owns its own loops; the engine BYEs it via Close, but we
	// also close it directly to stop receive loops after the engine has stopped.
	stack.push("subscriber", func(context.Context) error { return subClient.Close() })

	go engine.Run(ctx)
	stack.push("engine", func(context.Context) error { return engine.Close() })

	// Spotify bridge manager (D57): start the default Connect device ("ensemble
	// <node>", the legacy auto-switch behavior) plus one go-librespot bridge per
	// configured preset ("ensemble <node>: <name>"). All bridges advertise at once;
	// playing to one regroups its players + preempts any other (mutually exclusive).
	if spotMgr != nil {
		spotMgr.Start(ctx, cfg.SpotifyEndpoints)
		stack.push("spotify", func(context.Context) error { return spotMgr.Close() })
	}

	// Master-side control driver (D59/D62): drives the non-gossiping playback nodes
	// assigned to the group THIS node masters, over the control plane. It sends from
	// the source UDP socket (the node's STATUS comes back to that same endpoint).
	// D65: feed the driver each playback node's calibrated device-queue depth so it
	// can equalize cross-room buffering. The stable per-room constant is the servo
	// setpoint, recovered from STATUS as DeviceDelayNs−PhaseErrNs; only fresh,
	// recently-heard rooms are included.
	pbDelays := func() map[id.ID]playback.RoomDelay {
		st := srcSrv.Statuses()
		out := make(map[id.ID]playback.RoomDelay, len(st))
		now := time.Now()
		for nid, ps := range st {
			if now.Sub(ps.LastSeen) > 3*time.Second {
				continue
			}
			out[nid] = playback.RoomDelay{
				SetpointNs: ps.Status.DeviceDelayNs - ps.Status.PhaseErrNs,
				Calibrated: ps.Status.Calibrated,
				Playing:    ps.Status.Playing,
			}
		}
		return out
	}
	pbDriver := playback.NewDriver(playback.DriverConfig{Store: cl, W: srcUDP, Log: base, Delays: pbDelays})
	pbDriver.Run(ctx)
	stack.push("playback driver", func(context.Context) error { return pbDriver.Close() })

	// Member-side 1 Hz playing stats: one INFO line/second while the local sink is
	// actively playing (idle → silent). The master side is logged by H (it owns
	// the session); here we own the sink/clock/subscriber directly.
	go memberStatsLoop(ctx, theSink, clockFol, subClient, base)

	if disc != nil {
		disc.Run()
		stack.push("discovery", func(context.Context) error { return disc.Close() })
	}

	apiErr := make(chan error, 1)
	go func() { apiErr <- apiSrv.Start() }()
	stack.push("api", func(sc context.Context) error { return apiSrv.Shutdown(sc) })

	printBanner(os.Stderr, fmt.Sprintf("ensemble %s — ready", version), [][2]string{
		{"node", fmt.Sprintf("%s  (%s)", cfg.NodeName, cfg.NodeID.String())},
		{"roles", cfg.Role.String()},
		{"bind", host},
		{"ports", fmt.Sprintf("http=%d  stream=%d  source=%d  gossip=%d  (tcp+udp; bind-or-increment)", httpPort, streamPort, sourcePort, gossipPort)},
		{"paths", fmt.Sprintf("data=%s  media=%s", cfg.DataDir, cfg.MediaDir)},
		{"output", backendName},
		{"codecs", strings.Join(caps.Codecs, ", ")},
		{"sources", strings.Join(caps.Sources, ", ")},
		{"backends", strings.Join(caps.Backends, ", ")},
		{"spotify", spotifyBannerValue(cfg.NodeName)},
	})

	log.Info("ready",
		"version", version,
		"id", cfg.NodeID.String(), "name", cfg.NodeName,
		"http", httpPort, "stream", streamPort, "source", sourcePort, "gossip", gossipPort,
		"output", backendName, "media", cfg.MediaDir, "playback", caps.Playback,
	)

	// 15. Wait for shutdown signal or a fatal API error.
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-apiErr:
		if err != nil {
			rerr = fmt.Errorf("api server: %w", err)
		}
	}

	// Reset the deferred early-unwind path: from here we unwind unconditionally
	// with a fresh, bounded context.
	sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if uerr := stack.unwind(sc, log); uerr != nil && rerr == nil {
		rerr = uerr
	}
	// Prevent the deferred early-unwind from double-running.
	stack.fns = nil
	return rerr
}

// runPlayback is the standalone PLAYBACK-PIECE bring-up (D49/D50/D61): a
// non-gossiping, receive-only node. It runs the clock follower + subscriber + sink
// behind a localPlayer, exposes a control Listener on CONTROL_PORT, and announces
// itself over mDNS as a playback node. A master discovers and drives it (ATTACH/
// SETVOL/SETDELAY). It never gossips, sources audio, runs a group engine, or serves
// an HTTP API (D56) — the master piece is entirely absent from this process.
func runPlayback(ctx context.Context, opt options, cfg *config.Config, base *slog.Logger) (rerr error) {
	log := base.With("comp", "main")

	stack := &shutdownStack{}
	defer func() {
		if rerr != nil {
			sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = stack.unwind(sc, log)
			cancel()
		}
	}()

	// Sockets: STREAM_PORT (mux: clock probes + UDP audio) and CONTROL_PORT
	// (master→playback commands). No HTTP, source, or gossip ports.
	streamTCP, streamUDP, streamPort, err := netx.BindTCPUDP(opt.Host, cfg.StreamPort, bindTries(cfg.ExplicitPorts.Stream))
	if err != nil {
		return portBindErr("stream", cfg.StreamPort, cfg.ExplicitPorts.Stream, err)
	}
	_ = streamTCP.Close() // a playback node dials the master's source for TCP; it never listens
	stack.push("stream socket", func(context.Context) error { _ = streamUDP.Close(); return nil })

	ctrlTCP, ctrlUDP, controlPort, err := netx.BindTCPUDP(opt.Host, cfg.ControlPort, bindTries(cfg.ExplicitPorts.Control))
	if err != nil {
		return portBindErr("control", cfg.ControlPort, cfg.ExplicitPorts.Control, err)
	}
	_ = ctrlTCP.Close() // control is UDP soft-state (D58)
	stack.push("control socket", func(context.Context) error { _ = ctrlUDP.Close(); return nil })

	caps := capabilities(opt)
	log.Info("port bound", "service", "stream", "proto", "udp", "port", streamPort)
	log.Info("port bound", "service", "control", "proto", "udp", "port", controlPort)

	// Sink backend + playout, fed by the subscriber through the decode/deliver path.
	// Resilient failover chain (D37): try every real output until one works.
	backend, backendName, err := sink.OpenResilient(opt.Output, cfg.OutputDevice, base)
	if err != nil {
		return fmt.Errorf("sink backend %q: %w", opt.Output, err)
	}
	mux := stream.NewMux(streamUDP, base)
	clockFol := clock.NewFollower(mux, base)
	theSink := sink.New(sink.Config{
		Backend:       backend,
		Clock:         clockFol,
		BufferMs:      contracts.DefaultBufferMs,
		Volume:        cfg.Volume,
		OutputDelayMs: cfg.OutputDelayMs,
		Log:           base,
	})
	disabled := newDisableState(cfg.Disabled)
	subClient := stream.NewClient(stream.ClientConfig{
		Mux:     mux,
		Deliver: newDeliver(theSink, disabled, base),
		Log:     base,
	})

	// The playout component (D61), driven over the wire by the control Listener.
	player := playback.NewLocal(playback.Config{
		Self:  cfg.NodeID,
		Clock: clockFol,
		Sub:   subClient,
		Sink:  theSink,
		ClockStats: func() (int64, int64, bool) {
			st := clockFol.Stats()
			return st.OffsetNs, st.RTTNs, st.Synced
		},
	})
	listener := playback.NewListener(playback.ListenerConfig{Conn: ctrlUDP, Player: player, Log: base})

	// mDNS playback advert (D50/D51): control port + capabilities, no gossip.
	var disc *discovery.Discovery
	if cfg.NoMDNS {
		log.Info("mDNS disabled (--no-mdns); playback node will not be discoverable")
	} else {
		disc = discovery.New(discovery.Config{
			ID:          cfg.NodeID,
			Master:      false,
			Playback:    true,
			Name:        cfg.NodeName,
			ControlPort: controlPort,
			Caps: discovery.Caps{
				Codecs:         caps.Codecs,
				MaxRate:        stream.SampleRate,
				CanReportQueue: backendReportsQueue(backend),
				Input:          containsStr(caps.Sources, "input"),
			},
			Log: base,
		})
	}

	// Activate (teardown unwinds in reverse).
	mux.Run()
	stack.push("mux", func(context.Context) error { return mux.Close() })
	clockFol.Start()
	stack.push("clock follower", func(context.Context) error { return clockFol.Close() })
	stack.push("sink", func(context.Context) error { return theSink.Close() })
	stack.push("subscriber", func(context.Context) error { return subClient.Close() })
	listener.Run()
	stack.push("listener", func(context.Context) error { return listener.Close() })
	if disc != nil {
		disc.Run()
		stack.push("discovery", func(context.Context) error { return disc.Close() })
	}

	pbHost := opt.Host
	if pbHost == "" {
		pbHost = "0.0.0.0"
	}
	printBanner(os.Stderr, fmt.Sprintf("ensemble %s — ready", version), [][2]string{
		{"node", fmt.Sprintf("%s  (%s)", cfg.NodeName, cfg.NodeID.String())},
		{"roles", "playback"},
		{"bind", pbHost},
		{"ports", fmt.Sprintf("control=%d  stream=%d  (tcp+udp; bind-or-increment)", controlPort, streamPort)},
		{"paths", fmt.Sprintf("data=%s", cfg.DataDir)},
		{"output", backendName},
		{"codecs", strings.Join(caps.Codecs, ", ")},
		{"backends", strings.Join(caps.Backends, ", ")},
	})

	log.Info("ready",
		"version", version, "id", cfg.NodeID.String(), "name", cfg.NodeName,
		"role", "playback", "control", controlPort, "stream", streamPort,
		"output", backendName, "codecs", caps.Codecs,
	)

	<-ctx.Done()
	log.Info("shutting down")
	sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if uerr := stack.unwind(sc, log); uerr != nil && rerr == nil {
		rerr = uerr
	}
	stack.fns = nil
	return rerr
}

// backendReportsQueue reports whether a sink backend can report its output-queue
// depth (the rate-servo drift signal, D52) — advertised as the `queue` capability.
func backendReportsQueue(b contracts.Backend) bool {
	_, ok := b.(contracts.DelayReporter)
	return ok
}

// containsStr reports whether s is in xs.
func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// hostCIDR returns a /32 (or /128) CIDR for a concrete --host bind address, or ""
// for the wildcard/empty host. Lets DialCandidates resolve a loopback/explicit
// host that InterfaceCIDRs (which skips loopback) would never report (§3.1).
func hostCIDR(host string) string {
	if host == "" || host == "0.0.0.0" || host == "::" {
		return ""
	}
	a, err := netip.ParseAddr(host)
	if err != nil || a.IsUnspecified() {
		return ""
	}
	if a.Is4() {
		return host + "/32"
	}
	return host + "/128"
}

// probeGossipPort finds a free TCP+UDP pair via netx, CLOSES both, and returns
// the bare number for memberlist to bind itself (D8). The tiny rebind race is
// accepted for v1.
func probeGossipPort(host string, base, tries int) (port int, released bool, err error) {
	tcp, udp, port, err := netx.BindTCPUDP(host, base, tries)
	if err != nil {
		return 0, false, err
	}
	_ = tcp.Close()
	_ = udp.Close()
	return port, true, nil
}

// newLogger builds a text slog handler at the requested level (default info).
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}

// capabilities assembles this node's reported capabilities from the $PATH scan
// for exec tools, the static format/scheme lists, and the internal/dl dlopen
// probes (D3/D32). ENSEMBLE_OUTPUT=null forces playback=false while the probes
// still report alsa/opus per host reality (§8.5).
func capabilities(opt options) contracts.Capabilities {
	codecs := []string{"pcm"}
	if audio.OpusAvailable() {
		codecs = append(codecs, "opus")
	}

	playback := sink.HasPlayback()
	if forcedNull(opt.Output) {
		playback = false
	}

	return contracts.Capabilities{
		Playback: playback,
		Codecs:   codecs,
		Backends: sink.BackendNames(),
		Sources:  audio.Schemes(),
		Formats:  []string{"wav", "mp3", "flac"},
	}
}

// printBanner writes a clear, bordered startup summary to w — the at-a-glance
// "what am I, where, and what can I do" headline, complementing the structured
// "ready" log. Each row is a (label, value) pair; values are never truncated
// (clarity over a perfectly aligned right edge). Always shown, regardless of
// log level — it is the headline, not a log line.
func printBanner(w io.Writer, title string, rows [][2]string) {
	const rule = "═══════════════════════════════════════════════════════════════"
	const thin = "───────────────────────────────────────────────────────────────"
	fmt.Fprintf(w, "\n%s\n  %s\n%s\n", rule, title, thin)
	for _, r := range rows {
		fmt.Fprintf(w, "  %-9s %s\n", r[0], r[1])
	}
	fmt.Fprintf(w, "%s\n\n", rule)
}

// bindTries returns the bind attempt count for a port: 1 (bind exactly or fail)
// when the operator pinned it via flag/env, else DefaultTries (bind-or-increment,
// §2). Pinning a port and finding it taken is an explicit-config error worth
// surfacing, not silently drifting to the next number.
func bindTries(explicit bool) int {
	if explicit {
		return 1
	}
	return netx.DefaultTries
}

// portBindErr wraps a bind failure with operator-facing context: a pinned port
// explains the no-increment policy; an unset port notes the exhausted base.
func portBindErr(service string, base int, explicit bool, err error) error {
	if explicit {
		return fmt.Errorf("bind %s port %d (pinned via flag/env; not auto-incremented): %w", service, base, err)
	}
	return fmt.Errorf("bind %s port (base %d): %w", service, base, err)
}

// spotifyBannerValue describes the Spotify bridge for the banner: the resolved
// go-librespot/librespot path and the Connect device name it advertises, or a
// clear "not found" when no binary is present (Spotify disabled).
func spotifyBannerValue(nodeName string) string {
	bin := audio.FindSpotifyBinary()
	if bin == "" {
		return "not found (Spotify Connect disabled)"
	}
	return fmt.Sprintf("%s  (Connect device: %q)", bin, "ensemble "+nodeName)
}

// deviceIDs renders the enumerated output-device ids for the startup line (D37).
func deviceIDs(devs []contracts.OutputDevice) []string {
	ids := make([]string, 0, len(devs))
	for _, d := range devs {
		ids = append(ids, d.ID)
	}
	return ids
}

// applyOutputDevice returns the PATCH /api/node {outputDevice} live-apply closure
// (D37, §8.5). The resilient backend honors it by re-ordering its failover chain
// to prefer the chosen device (the override may itself fail, in which case the
// chain carries on past it). For a plain backend that ignores devices it is a
// no-op (persist+replicate already happened upstream).
func applyOutputDevice(backendName string, sk *sink.Playout, log *slog.Logger) func(string) {
	return func(device string) {
		if sk.PreferOutputDevice(device) {
			log.Info("output device override", "device", device)
			return
		}
		log.Info("output device changed; backend ignores device, persist+replicate only", "backend", backendName)
	}
}

// forcedNull reports whether ENSEMBLE_OUTPUT explicitly selects the null backend.
func forcedNull(output string) bool {
	return strings.TrimSpace(output) == "null"
}

// outputLabel renders the requested ENSEMBLE_OUTPUT for the startup line ("auto"
// when unset; the backend is resolved/logged again at bind time).
func outputLabel(output string) string {
	if strings.TrimSpace(output) == "" {
		return "auto"
	}
	return output
}

// ---- adapters (K-owned glue between sibling packages) -----------------------

// newDeliver builds the member-side stream→sink callback (wiring gap #3): it
// decodes opus packets (a payload shorter than a full canonical PCM frame, i.e.
// not pcm), re-arms the sink on a generation change so a live settings/RECONFIG
// gen bump keeps playing, and pushes canonical PCM to the sink. A single decoder
// is reused across the subscription (one goroutine: the mux read loop, serialized
// by the stream client).
func newDeliver(sk contracts.Sink, disabled *disableState, log *slog.Logger) stream.DeliverFunc {
	var dec *audio.OpusDecoder
	var decUnavail bool // sticky: the decoder can't be built (e.g. libopus missing) —
	// back off after one WARN instead of re-probing + re-warning on every frame.
	var curGen uint32
	haveGen := false
	dl := log.With("comp", "deliver")

	// armedSink lets deliver consult the sink's ACTUAL armed gen (the real
	// *sink.Playout implements it; the fakeSink in tests does not). This closes
	// the late-join stale-gen bug: when the group engine (repointLocked) Resets
	// the sink to a guessed gen on a (re)subscribe, deliver's own curGen cache may
	// still equal the incoming frame gen and skip re-arming — leaving the sink
	// armed at the wrong (guessed) gen, so every frame drops as stale-gen and the
	// joiner starves. Re-arming whenever the sink is not armed at the frame's gen
	// makes deliver authoritative regardless of its cache.
	armed, _ := sk.(interface {
		ArmedGen() (uint32, bool)
	})

	return func(h stream.Header, payload []byte) {
		// A generation change means a new session / settings RECONFIG: re-arm the
		// sink under the new gen, else every frame is dropped as stale-gen. Also
		// re-arm when the sink reports it is armed at a DIFFERENT gen (or disarmed)
		// than this frame — i.e. repointLocked re-armed it out from under us on a
		// (re)subscribe (the late-join case).
		needArm := !haveGen || h.Gen != curGen
		if !needArm && armed != nil {
			if g, ok := armed.ArmedGen(); !ok || g != h.Gen {
				needArm = true
			}
		}
		if needArm {
			sk.Reset(h.Gen)
			curGen = h.Gen
			haveGen = true
		}

		pcm := payload
		if len(payload) != stream.FrameBytes {
			// Compressed (opus) payload. Locally-disabled opus (D40) refuses to
			// decode — drop the frame (the master should not have picked opus for a
			// group including us; effective caps gate it).
			if disabled.has("opus") {
				dl.Debug("opus disabled on this node, dropping frame")
				return
			}
			// Lazily build the decoder. A build failure (e.g. libopus not installed)
			// is permanent for this process — the library won't appear mid-stream — so
			// latch it, WARN once, and drop quietly thereafter rather than re-probing
			// and flooding the log at frame rate. (A node without libopus advertises
			// pcm-only, so a correctly-negotiated group never sends us opus; this is the
			// belt-and-braces soft-fail for when it does.)
			if dec == nil {
				if decUnavail {
					dl.Debug("opus decoder unavailable, dropping frame")
					return
				}
				d, err := audio.NewOpusDecoder()
				if err != nil {
					decUnavail = true
					dl.Warn("opus decoder unavailable — dropping opus frames on this node (install libopus to enable)", "err", err)
					return
				}
				dec = d
				dl.Info("opus decoder created", "rate", 48000, "channels", 2)
			}
			out, err := dec.Decode(payload)
			if err != nil {
				dl.Debug("opus decode failed, dropping frame", "err", err)
				return
			}
			pcm = out
		}
		sk.Push(h.Gen, h.Seq, h.PTS, pcm)
	}
}

// memberStatsLoop emits the member-side 1 Hz playing-stats line (comp=sink)
// while the local sink is actively playing. It ticks every second and logs only
// when playout advanced (played/silence moved) since the previous tick, so an
// idle node stays quiet. Returns when ctx is cancelled.
func memberStatsLoop(ctx context.Context, sk *sink.Playout, fol *clock.Follower, sub *stream.Client, log *slog.Logger) {
	sl := log.With("comp", "sink")
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var prevPlayed, prevSilence uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ss := sk.Stats()
		// Active iff playout produced output frames (real or silence) this second.
		if ss.Played == prevPlayed && ss.Silence == prevSilence {
			prevPlayed, prevSilence = ss.Played, ss.Silence
			continue
		}
		prevPlayed, prevSilence = ss.Played, ss.Silence

		cs := fol.Stats()
		ct := sub.Counters()
		sl.Info("playing",
			"side", "member",
			"played", ss.Played,
			"silence", ss.Silence,
			"lateDrop", ss.LateDrop,
			"buffered", ss.Buffered,
			"ratePPM", ss.RatePPM,
			"synced", ss.Synced,
			"offsetNs", cs.OffsetNs,
			"rttNs", cs.RTTNs,
			"delivered", ct.Delivered,
			"recovered", ct.Recovered,
			"lost", ct.Lost,
		)
	}
}

// mediaFactory adapts audio.Open (ctx + mediaDir bound) to group.MediaFactory.
// It refuses to open an input: source when "input" is operator-disabled (D40).
type mediaFactory struct {
	mediaDir string
	disabled *disableState
}

func (m mediaFactory) Open(uri string) (group.MediaSource, error) {
	if m.disabled.has("input") && uriScheme(uri) == "input" {
		return nil, errInputDisabled
	}
	src, err := audio.Open(context.Background(), uri, m.mediaDir)
	if err != nil {
		return nil, err
	}
	return src, nil
}

// Probe reads embedded tags for a file URI (queue metadata), bound to mediaDir.
func (m mediaFactory) Probe(uri string) (contracts.TrackMetadata, bool) {
	return audio.Probe(context.Background(), uri, m.mediaDir)
}

// uriScheme returns the lowercased scheme of a URI ("file" when none).
func uriScheme(uri string) string {
	i := strings.IndexByte(uri, ':')
	if i <= 0 {
		return "file"
	}
	return strings.ToLower(uri[:i])
}

var errInputDisabled = errors.New("audio: input capture disabled on this node")

// opusFactory adapts audio.NewOpusEncoder to group.OpusFactory. It refuses when
// "opus" is operator-disabled on this node (D40) — dl.ErrUnavailable so the group
// engine surfaces ErrNoOpus exactly as it does for a host without libopus.
type opusFactory struct {
	log      *slog.Logger
	disabled *disableState
}

func (f opusFactory) NewEncoder() (group.OpusEncoder, error) {
	if f.disabled.has("opus") {
		return nil, dl.ErrUnavailable
	}
	enc, err := audio.NewOpusEncoder()
	if err == nil && f.log != nil {
		f.log.With("comp", "audio").Info("opus encoder created", "bitrate", audio.OpusBitrate, "rate", 48000, "channels", 2)
	}
	return enc, err
}

// ---- disable state (D40) ----------------------------------------------------

// disableState is the live set of operator-disabled features on this node (D40).
// Updated atomically by PATCH /api/node {disabled}; read by the media/opus
// factories and the deliver path. Cheap reads (a pointer load + small map).
type disableState struct {
	v atomic.Pointer[map[string]bool]
}

func newDisableState(initial []string) *disableState {
	d := &disableState{}
	d.set(initial)
	return d
}

func (d *disableState) set(features []string) {
	m := make(map[string]bool, len(features))
	for _, f := range features {
		m[f] = true
	}
	d.v.Store(&m)
}

func (d *disableState) has(feature string) bool {
	m := d.v.Load()
	return m != nil && (*m)[feature]
}

// applyDisabled returns the PATCH /api/node {disabled} live-apply closure (D40).
// It updates the shared disable set and, when "playback" toggles, swaps the live
// sink backend: disabling playback swaps to the null backend; re-enabling reopens
// the configured device/backend (mirroring applyOutputDevice). opus/input need no
// swap — the factories/deliver read the set directly.
func applyDisabled(state *disableState, outputSpec, device string, sk *sink.Playout, log *slog.Logger) func([]string) {
	return func(features []string) {
		wasPlayback := state.has("playback")
		state.set(features)
		nowPlayback := state.has("playback")
		if wasPlayback == nowPlayback {
			return
		}
		if nowPlayback {
			// Newly disabled: swap to the null backend (timed discard).
			nb, _, err := sink.OpenDevice("null", device, log)
			if err != nil {
				log.Warn("playback disable: null backend open failed", "err", err)
				return
			}
			sk.SwapBackend(nb)
			log.Info("playback disabled; sink swapped to null backend")
			return
		}
		// Re-enabled: reopen the resilient failover chain (not a plain backend) so
		// the self-healing output is restored after a disable/enable cycle.
		nb, name, err := sink.OpenResilient(outputSpec, device, log)
		if err != nil {
			log.Warn("playback re-enable: backend reopen failed; keeping null", "err", err)
			return
		}
		sk.SwapBackend(nb)
		log.Info("playback re-enabled; sink swapped back", "backend", name)
	}
}

// statusStats builds the GET /api/status closure from the sink (E), clock
// follower (F), and engine (H — source stats only while a session runs, D19).
func statusStats(sk contracts.Sink, fol *clock.Follower, eng *group.Engine) func() api.StatusStats {
	return func() api.StatusStats {
		fs := fol.Stats()
		st := api.StatusStats{
			Sink: sk.Stats(),
			Clock: api.ClockStat{
				Synced:   fs.Synced,
				OffsetNs: fs.OffsetNs,
				RTTNs:    0,
			},
		}
		if src, ok := eng.SourceStats(); ok {
			st.Source = &src
		}
		return st
	}
}

// groupAdapter bridges *group.Engine to the api.Group interface (wiring gap #1/#2):
// it forwards/ignores ctx as each engine method needs, routes NameGroup to the
// cluster (the engine has no NameGroup), and translates group sentinel errors to
// the api sentinels so the handlers' errors.Is matches (preserving the original
// message, including node names in opus errors).
type groupAdapter struct {
	e  *group.Engine
	cl *cluster.Cluster
}

func (g *groupAdapter) Follow(ctx context.Context, target id.ID) error {
	return mapErr(g.e.Follow(target))
}

func (g *groupAdapter) Unfollow(ctx context.Context) error {
	return mapErr(g.e.Unfollow())
}

func (g *groupAdapter) NameGroup(ctx context.Context, group id.ID, name string) error {
	// The engine owns no group-name write; it is a plain LWW cluster record any
	// node may set (§4/§9.1). The request's `group` is the current group id (=
	// master id, D42), but the explicit name OVERRIDE map is keyed by the member-
	// set XOR (an override names a specific COMBINATION of rooms, surviving master
	// changes + re-forming). Resolve the group's CURRENT member set from the
	// snapshot, compute its XOR, and write the override there. An empty name CLEARS
	// the override (back to the derived label). A group id with no live members
	// (skew) falls back to keying by the given id verbatim.
	key := group
	for _, gv := range g.cl.Snapshot().Groups {
		if gv.ID == group {
			key = id.XOR(gv.Members...)
			break
		}
	}
	g.cl.SetGroupName(key, name)
	return nil
}

func (g *groupAdapter) Play(ctx context.Context, uri string) error {
	return mapErr(g.e.Play(uri))
}

func (g *groupAdapter) Enqueue(ctx context.Context, uris []string) error {
	return mapErr(g.e.Enqueue(uris))
}

func (g *groupAdapter) RemoveFromQueue(ctx context.Context, index int, uriGuard string) error {
	return mapErr(g.e.RemoveFromQueue(index, uriGuard))
}

func (g *groupAdapter) PlayQueuedNow(ctx context.Context, index int, uriGuard string) error {
	return mapErr(g.e.PlayQueuedNow(index, uriGuard))
}

func (g *groupAdapter) QueueList() []contracts.QueueItem {
	return g.e.QueueSnapshot()
}

func (g *groupAdapter) Seek(ctx context.Context, positionSec float64) error {
	return mapErr(g.e.Seek(positionSec))
}

func (g *groupAdapter) Next(ctx context.Context) error {
	return mapErr(g.e.Next())
}

func (g *groupAdapter) Stop(ctx context.Context) error {
	return mapErr(g.e.Stop())
}

func (g *groupAdapter) Pause(ctx context.Context) error {
	return mapErr(g.e.Pause())
}

func (g *groupAdapter) Resume(ctx context.Context) error {
	return mapErr(g.e.Resume())
}

func (g *groupAdapter) Settings() contracts.GroupSettings {
	return g.e.Settings()
}

func (g *groupAdapter) SetSettings(ctx context.Context, s contracts.GroupSettings) error {
	return mapErr(g.e.SetSettings(s))
}

// mapErr wraps a group-engine sentinel so errors.Is matches the api sentinel
// (errors.Is fails across packages otherwise) while preserving the original
// message. Unknown errors pass through (the api handler 500s them).
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, group.ErrNoOpus):
		return translated{api.ErrNoCodec, err}
	case errors.Is(err, group.ErrBadSettings):
		return translated{api.ErrNoCodec, err}
	case errors.Is(err, group.ErrNotSynced):
		return translated{api.ErrNotSynced, err}
	case errors.Is(err, group.ErrNotPlaying):
		return translated{api.ErrNotPlaying, err}
	case errors.Is(err, group.ErrNotPaused):
		return translated{api.ErrNotPaused, err}
	case errors.Is(err, group.ErrNotSeekable):
		return translated{api.ErrNotSeekable, err}
	default:
		return err
	}
}

// translated carries an api sentinel for errors.Is matching plus the original
// engine error for its message (incl. opus node names).
type translated struct {
	sentinel error
	orig     error
}

func (t translated) Error() string { return t.orig.Error() }
func (t translated) Is(target error) bool {
	return errors.Is(t.sentinel, target)
}
func (t translated) Unwrap() error { return t.orig }

// ---- shutdown stack ---------------------------------------------------------

// shutdownStack is a LIFO of named teardown closures, pushed as resources are
// acquired and unwound in reverse on shutdown (§3.3). Extracted for unit tests.
type shutdownStack struct {
	fns []namedCloser
}

type namedCloser struct {
	name string
	fn   func(context.Context) error
}

func (s *shutdownStack) push(name string, fn func(context.Context) error) {
	s.fns = append(s.fns, namedCloser{name, fn})
}

// unwind runs every closer in reverse order, logging each, and returns the first
// error encountered (continuing past it).
func (s *shutdownStack) unwind(ctx context.Context, log *slog.Logger) error {
	var first error
	for i := len(s.fns) - 1; i >= 0; i-- {
		nc := s.fns[i]
		if err := nc.fn(ctx); err != nil {
			log.Warn("shutdown step failed", "step", nc.name, "err", err)
			if first == nil {
				first = err
			}
		}
	}
	return first
}
