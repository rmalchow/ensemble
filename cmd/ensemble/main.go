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
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"ensemble/internal/api"
	"ensemble/internal/audio"
	"ensemble/internal/clock"
	"ensemble/internal/cluster"
	"ensemble/internal/config"
	"ensemble/internal/contracts"
	"ensemble/internal/discovery"
	"ensemble/internal/group"
	"ensemble/internal/id"
	"ensemble/internal/netx"
	"ensemble/internal/sink"
	"ensemble/internal/source"
	"ensemble/internal/stream"
	"ensemble/web"
)

// options is the fully-resolved configuration after flags+env. Ports/dirs are
// resolved by config.Load (A); options only carries the K-owned knobs (--host,
// ENSEMBLE_OUTPUT, ENSEMBLE_LOG) plus the raw flag args forwarded to config.Load.
type options struct {
	Host     string   // --host bind address; "" => all interfaces, "127.0.0.1" in dev/e2e
	Output   string   // ENSEMBLE_OUTPUT (env only, D2): "" => auto | null | file:<p> | name
	LogLevel string   // ENSEMBLE_LOG (debug|info|warn|error), default info
	cfgArgs  []string // flag args forwarded to config.Load (--host stripped)
}

func main() {
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

// parseOptions extracts the K-owned knobs (--host, ENSEMBLE_OUTPUT, ENSEMBLE_LOG)
// and forwards the remaining flag args to config.Load (A owns flag>env>default for
// ports/dirs/name/join). It parses with a permissive FlagSet so unknown flags
// (the config ones) are passed through untouched.
func parseOptions(args []string, env func(string) string) (options, error) {
	opt := options{
		Output:   env("ENSEMBLE_OUTPUT"),
		LogLevel: env("ENSEMBLE_LOG"),
	}
	if opt.LogLevel == "" {
		opt.LogLevel = "info"
	}

	// Pull --host (and --host=v) out of args; everything else goes to config.Load.
	var host string
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
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
		default:
			rest = append(rest, a)
		}
	}
	if host == "" {
		host = env("ENSEMBLE_HOST")
	}
	opt.Host = host
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
	fs.Int("gossip-port", 0, "")
	fs.String("data", "", "")
	fs.String("media", "", "")
	fs.String("name", "", "")
	fs.String("join", "", "")
	return fs.Parse(args)
}

// run builds the component graph, starts it, blocks until ctx is cancelled, then
// unwinds the shutdown stack in reverse. Returns the first fatal/shutdown error.
func run(ctx context.Context, opt options) (rerr error) {
	log := newLogger(opt.LogLevel).With("comp", "main")

	// 1. config / node.json (A). Fatal on error — never mint a fresh id over a
	//    corrupt file (§4).
	cfg, err := config.Load(config.Options{Args: opt.cfgArgs})
	if err != nil {
		return fmt.Errorf("config: %w", err)
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

	// 2. Port binds — bind-or-increment; capture the ACTUAL bound port each (§2).
	streamTCP, streamUDP, streamPort, err := netx.BindTCPUDP(opt.Host, cfg.StreamPort, netx.DefaultTries)
	if err != nil {
		return fmt.Errorf("bind stream port (base %d): %w", cfg.StreamPort, err)
	}
	stack.push("stream listeners", func(context.Context) error {
		_ = streamTCP.Close()
		return nil
	})

	srcTCP, srcUDP, sourcePort, err := netx.BindTCPUDP(opt.Host, cfg.SourcePort, netx.DefaultTries)
	if err != nil {
		return fmt.Errorf("bind source port (base %d): %w", cfg.SourcePort, err)
	}
	stack.push("source sockets", func(context.Context) error {
		_ = srcTCP.Close()
		_ = srcUDP.Close()
		return nil
	})

	httpLn, httpPort, err := netx.BindTCP(opt.Host, cfg.HTTPPort, netx.DefaultTries)
	if err != nil {
		return fmt.Errorf("bind http port (base %d): %w", cfg.HTTPPort, err)
	}
	stack.push("http listener", func(context.Context) error {
		_ = httpLn.Close()
		return nil
	})

	gossipPort, err := probeGossipPort(opt.Host, cfg.GossipPort, netx.DefaultTries)
	if err != nil {
		return fmt.Errorf("probe gossip port (base %d): %w", cfg.GossipPort, err)
	}

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

	// 5. UDP mux over STREAM_PORT (not yet Run).
	mux := stream.NewMux(streamUDP, log)

	// 6. Cluster (memberlist on the probed gossip port; impls StateStore). The
	//    discovery Peers channel is consumed by the cluster's own join loop.
	disc := discovery.New(discovery.Config{
		ID:         cfg.NodeID,
		GossipPort: gossipPort,
		HTTPPort:   httpPort,
		StreamPort: streamPort,
		SourcePort: sourcePort,
		Log:        log,
	})
	cl, err := cluster.New(cluster.Config{
		Self:          cfg.NodeID,
		Name:          cfg.NodeName,
		Volume:        cfg.Volume,
		OutputDelayMs: cfg.OutputDelayMs,
		Caps:          caps,
		Addrs:         addrs,
		HTTPPort:      httpPort,
		StreamPort:    streamPort,
		SourcePort:    sourcePort,
		GossipPort:    gossipPort,
		BindAddr:      opt.Host,
		Peers:         disc.Peers(),
		Logger:        log,
	})
	if err != nil {
		return fmt.Errorf("cluster: %w", err)
	}

	// 7. Clock server (passive) + follower (contracts.Clock).
	clockSrv := clock.NewServer(mux, log)
	clockSrv.Start() // registers 0x10 on the mux (idempotent, mux not yet running)
	clockFol := clock.NewFollower(mux, log)

	// 8. Sink backend + playout. ENSEMBLE_OUTPUT=null forces null (§8.5/D27).
	backend, backendName, err := sink.Open(opt.Output, log)
	if err != nil {
		return fmt.Errorf("sink backend %q: %w", opt.Output, err)
	}
	theSink := sink.New(sink.Config{
		Backend:       backend,
		Clock:         clockFol,
		BufferMs:      contracts.DefaultBufferMs,
		Volume:        cfg.Volume,
		OutputDelayMs: cfg.OutputDelayMs,
		Log:           log,
	})

	// 9. Source server (master-side; idle until a session runs) on SOURCE_PORT.
	srcSrv := source.NewServer(source.Config{
		Self: cfg.NodeID,
		UDP:  srcUDP,
		TCP:  srcTCP,
		Log:  log,
	})

	// 10. Subscriber client (member-side). The deliver closure decodes opus when
	//     the payload is a compressed packet (not a full PCM frame), re-arms the
	//     sink on a generation change, and pushes canonical PCM to the sink.
	subClient := stream.NewClient(stream.ClientConfig{
		Mux:     mux,
		Deliver: newDeliver(theSink, log),
		Log:     log,
	})

	// 11. Media + opus factories (D's audio package), bound to MediaDir.
	media := mediaFactory{mediaDir: cfg.MediaDir}
	var opusFac group.OpusFactory
	if audio.OpusAvailable() {
		opusFac = opusFactory{}
	}

	// 12. Group engine (H). Follow client comes from I (D16) bound to the cluster.
	follow := api.NewFollowClient(cl)
	engine := group.New(group.Params{
		Cluster:  cl,
		Media:    media,
		Opus:     opusFac,
		Source:   srcSrv,
		Sub:      subClient,
		Sink:     theSink,
		Clock:    clockFol,
		ClockCtl: clockFol,
		Follow:   follow,
		Caps:     caps,
		Log:      log,
	})

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
		Stats:   statusStats(theSink, clockFol, engine),
		Sink:    func() api.SinkControl { return theSink },
		Ports: api.PortsResp{
			HTTP:   httpPort,
			Stream: streamPort,
			Source: sourcePort,
			Gossip: gossipPort,
		},
		Listener: httpLn,
		DistFS:   distFS,
		Log:      log,
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

	disc.Run()
	stack.push("discovery", func(context.Context) error { return disc.Close() })

	apiErr := make(chan error, 1)
	go func() { apiErr <- apiSrv.Start() }()
	stack.push("api", func(sc context.Context) error { return apiSrv.Shutdown(sc) })

	log.Info("ready",
		"id", cfg.NodeID.String(), "name", cfg.NodeName,
		"http", httpPort, "stream", streamPort, "source", sourcePort, "gossip", gossipPort,
		"output", backendName, "playback", caps.Playback,
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
func probeGossipPort(host string, base, tries int) (int, error) {
	tcp, udp, port, err := netx.BindTCPUDP(host, base, tries)
	if err != nil {
		return 0, err
	}
	_ = tcp.Close()
	_ = udp.Close()
	return port, nil
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

// forcedNull reports whether ENSEMBLE_OUTPUT explicitly selects the null backend.
func forcedNull(output string) bool {
	return strings.TrimSpace(output) == "null"
}

// ---- adapters (K-owned glue between sibling packages) -----------------------

// newDeliver builds the member-side stream→sink callback (wiring gap #3): it
// decodes opus packets (a payload shorter than a full canonical PCM frame, i.e.
// not pcm), re-arms the sink on a generation change so a live settings/RECONFIG
// gen bump keeps playing, and pushes canonical PCM to the sink. A single decoder
// is reused across the subscription (one goroutine: the mux read loop, serialized
// by the stream client).
func newDeliver(sk contracts.Sink, log *slog.Logger) stream.DeliverFunc {
	var dec *audio.OpusDecoder
	var curGen uint32
	haveGen := false
	dl := log.With("comp", "deliver")

	return func(h stream.Header, payload []byte) {
		// A generation change means a new session / settings RECONFIG: re-arm the
		// sink under the new gen, else every frame is dropped as stale-gen.
		if !haveGen || h.Gen != curGen {
			sk.Reset(h.Gen)
			curGen = h.Gen
			haveGen = true
		}

		pcm := payload
		if len(payload) != stream.FrameBytes {
			// Compressed (opus) payload. Lazily build the decoder.
			if dec == nil {
				d, err := audio.NewOpusDecoder()
				if err != nil {
					dl.Warn("opus decoder unavailable, dropping frame", "err", err)
					return
				}
				dec = d
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

// mediaFactory adapts audio.Open (ctx + mediaDir bound) to group.MediaFactory.
type mediaFactory struct{ mediaDir string }

func (m mediaFactory) Open(uri string) (group.MediaSource, error) {
	src, err := audio.Open(context.Background(), uri, m.mediaDir)
	if err != nil {
		return nil, err
	}
	return src, nil
}

// opusFactory adapts audio.NewOpusEncoder to group.OpusFactory.
type opusFactory struct{}

func (opusFactory) NewEncoder() (group.OpusEncoder, error) {
	return audio.NewOpusEncoder()
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

func (g *groupAdapter) MakeMaster(ctx context.Context, node id.ID) error {
	return mapErr(g.e.MakeMaster(ctx, node))
}

func (g *groupAdapter) NameGroup(ctx context.Context, group id.ID, name string) error {
	// The engine owns no group-name write; it is a plain LWW cluster record any
	// node may set (§4). Route straight to the cluster store.
	g.cl.SetGroupName(group, name)
	return nil
}

func (g *groupAdapter) Play(ctx context.Context, uri string) error {
	return mapErr(g.e.Play(uri))
}

func (g *groupAdapter) Stop(ctx context.Context) error {
	return mapErr(g.e.Stop())
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
	case errors.Is(err, group.ErrNotMaster):
		return translated{api.ErrNotMaster, err}
	case errors.Is(err, group.ErrTargetUnknown):
		return translated{api.ErrUnknownNode, err}
	case errors.Is(err, group.ErrTargetDead):
		return translated{api.ErrNotAlive, err}
	case errors.Is(err, group.ErrTargetFollower):
		return translated{api.ErrTargetNotMaster, err}
	case errors.Is(err, group.ErrSelfFollow):
		return translated{api.ErrUnknownNode, err}
	case errors.Is(err, group.ErrNoOpus):
		return translated{api.ErrNoCodec, err}
	case errors.Is(err, group.ErrBadSettings):
		return translated{api.ErrNoCodec, err}
	case errors.Is(err, group.ErrNotSynced):
		return translated{api.ErrNotMaster, err}
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
