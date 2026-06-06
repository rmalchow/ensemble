package daemon

// role.go fills the realtime-plane construction the P0.3 role loop left as
// TODO(P3/P4): it wires the per-group role engine (group.Engine, which owns the
// generation-fenced master/follower reconcile, A.14.4) to the concrete stream/
// audio/clock subsystems via the group.Hooks function-value seam. The role loop
// (daemon.go) drives it; this file constructs it and binds the hooks.
//
// Design: group.Engine.Apply(Inputs) IS the role reconciler — it diffs the
// resolved Decision and calls only the hooks needed to converge, fenced by
// StreamGen/Generation so a superseded master cannot emit (doc 01 §4.2). P4.9
// supplies the hooks:
//
//   master / solo : StartClockServer(clock.Listen) + StartOrigin(stream.Origin
//                   over source.Open→codec→fec, paced unicast) + OriginResumeAt
//                   (Timeline.Seed + origin.ResumeAt on promotion, 04 §4.4.4) +
//                   StartRender iff Caps.Render (local loopback, D17).
//   follower      : StartClockFollower(clock.Follower.Run) + StartReceiver
//                   (stream.Receiver recv→FEC.Recover→decode→ring) +
//                   ReceiverFlushReprime (R11) + StartRender (FollowerTimeline→
//                   audio.Renderer). A Caps.Render=false member gets ClockFollower
//                   only (control-only posture, 04 §4.2.4) — the engine's Decision
//                   already encodes that, so the hooks need no special-casing.
//
// runMaster/runFollower are the P0.3-named entry points the role fence calls; they
// delegate to engine.Apply with the right Inputs. They are nil-safe: with no engine
// wired (the P0.3 skeleton / unit tests using a bare Node) they only flip the
// status role, so TestApplyRoleFence still exercises the pure fence.

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/allowlist"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/audio/render"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/audio/ring"
	sink "gitlab.rand0m.me/ruben/go/ensemble/internal/audio/sink"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/clock"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/group"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/codec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/fec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/origin"
	sinknet "gitlab.rand0m.me/ruben/go/ensemble/internal/stream/sink_net"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/source"
)

// roleEngine bundles the live group.Engine plus the inputs-resolver the role loop
// feeds it. It is set on the transport (engineApply) by activate(); a nil
// roleEngine means "no realtime plane wired" so runMaster/runFollower no-op.
type roleEngine struct {
	engine *group.Engine
	// inputs resolves the current group.Inputs from the replicated doc + election +
	// clock health. cmd closes over state.Store + cluster; tests inject a fake.
	inputs func(self, master string, gen uint64) group.Inputs
}

// buildTransport constructs the live transport seam for an active session over
// the daemon's ONE persistent store (n.store — so genesis, adoption and gossip
// merges flow straight into the engine, doc 07 §5): the group MasterTimeline,
// the group.Engine wired to the stream/audio/clock hooks, the inputs resolver
// reading the replicated doc, and the multi-node plane (election-resolved master
// addressing + the allowlist gating the clock server and audio receiver). cp may
// be nil only in unit tests; activate always supplies a plane (which itself
// degrades to the solo doc-elected substrate without a gossip port).
func (n *Node) buildTransport(groupID string, cp *clusterPlane) *transport {
	store := n.store

	clockAddr := portAddr(n.options.ClockPort, defaultClockPort)
	audioAddr := portAddr(n.options.AudioPort, defaultAudioPort)
	if cp != nil {
		// The plane resolved the ACTUAL free ports for this session (several
		// instances may share a host); bind what it advertises.
		clockAddr = ":" + itoa(cp.clockPort)
		audioAddr = ":" + itoa(cp.audioPort)
	}
	ap := canonicalAudio(n.options.Device, clockAddr, audioAddr)
	hs := &hookState{
		n:        n,
		ap:       ap,
		cp:       cp,
		tl:       group.NewMasterTimeline(ap.rate),
		cfg:      store.Get,
		loopRing: ring.NewRing(ringSamples(ap)),
	}
	if cp != nil {
		hs.allow = cp.allow
	}
	masterAddr := func(_, _ string) string { return ap.clockAddr } // solo: local clock plane
	if cp != nil {
		masterAddr = cp.clockAddrOf // elected master's live clock endpoint
	}
	engine := group.NewEngine(n.options.NodeID, hs.buildHooks()).WithMasterAddr(masterAddr)

	live := func(string) bool { return false }
	if cp != nil {
		live = func(nodeID string) bool {
			if cp.mem == nil {
				return false
			}
			for _, m := range cp.mem.Members() {
				if m.Meta.NodeID == nodeID {
					return true
				}
			}
			return false
		}
	}

	var peer peerProxy
	if cp != nil {
		// Cross-node control proxy (mTLS, node-cert authenticated): the Media
		// screen's master-scoped listing + the §F.2 existence check work when the
		// group master is a PEER (peer_proxy.go).
		peer = &httpPeer{n: n, cp: cp}
	}

	tx := &transport{
		store:   store,
		self:    n.options.NodeID,
		dataDir: n.options.Paths.Data,
		hooks:   hs,
		live:    live,
		peer:    peer,
		master: func(gid string) string {
			if cp != nil && cp.mem != nil {
				if m := cp.elections.Master(gid); m != "" {
					return m
				}
			}
			return masterOf(store.Get(), gid, n.options.NodeID)
		},
		roleEngine: &roleEngine{
			engine: engine,
			inputs: func(self, master string, gen uint64) group.Inputs {
				return resolveInputs(store.Get(), groupID, self, master, gen)
			},
		},
	}
	return tx
}

// portAddr renders a UDP listen address from a port, falling back to def when 0.
func portAddr(port, def int) string {
	if port <= 0 {
		port = def
	}
	return ":" + itoa(port)
}

// itoa is a tiny strconv.Itoa to avoid the import churn in this wiring file.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}

// masterOf resolves the elected master for a group from the replicated doc. The
// single-node substrate elects the lexicographically-lowest member id (A.5
// tiebreak); a one-member group therefore elects that member (solo). Returns ""
// if self is not a member (no role to play). The P2-wiring step replaces this with
// the live cluster.GroupElections outcome.
func masterOf(doc state.ConfigDoc, groupID, self string) string {
	g := groupRecord(doc, groupID)
	if g == nil {
		return ""
	}
	member := false
	low := ""
	for _, id := range g.MemberNodeIDs {
		if id == self {
			member = true
		}
		if low == "" || id < low {
			low = id
		}
	}
	if !member {
		return ""
	}
	if low == "" {
		// Group exists but has no members listed; self drives it (solo bootstrap).
		return self
	}
	return low
}

// resolveInputs assembles the group.Inputs for the engine from the replicated doc.
// MasterID is the resolved election outcome; ClockOK is true on the master (it is
// the reference) and provisionally true on a follower (the orphan gate refines it
// via SetClockHealth from the clock follower sample hook).
func resolveInputs(doc state.ConfigDoc, groupID, self, master string, gen uint64) group.Inputs {
	g := groupRecord(doc, groupID)
	in := group.Inputs{
		SelfID:     self,
		GroupID:    groupID,
		MasterID:   master,
		Generation: gen,
		ClockOK:    true,
		MinDelayOK: true,
	}
	if g != nil {
		in.Members = membersOf(doc, g.MemberNodeIDs)
		in.Playing = g.Playing
		in.Profile = profileFor(g.Profile)
	}
	in.MyCaps = capsOf(doc, self)
	return in
}

// membersOf returns the NodeRecords for the given member ids (the engine counts
// "other members" to distinguish solo from master).
func membersOf(doc state.ConfigDoc, ids []string) []state.NodeRecord {
	out := make([]state.NodeRecord, 0, len(ids))
	for _, id := range ids {
		if nr := nodeRecord(doc, id); nr != nil {
			out = append(out, *nr)
		} else {
			out = append(out, state.NodeRecord{ID: id})
		}
	}
	return out
}

// capsOf returns self's effective capabilities from the doc. A self not yet in the
// doc (single-node bootstrap before its NodeRecord lands) is treated as
// render-capable so a solo node decodes+renders locally.
func capsOf(doc state.ConfigDoc, self string) state.Capabilities {
	if nr := nodeRecord(doc, self); nr != nil {
		return nr.Caps
	}
	return state.Capabilities{Render: true}
}

// profileFor projects a state.TransportProfile into the group.Profile the engine
// compares for StreamGen renegotiation. group.Profile mirrors the state fields.
func profileFor(p state.TransportProfile) group.Profile {
	return group.Profile{
		Codec:          p.Codec,
		FEC:            p.FEC,
		Rate:           p.Rate,
		FramesPerChunk: p.FramesPerChunk,
	}
}

// runMaster is the master/solo entry the role fence calls under rctx (A.14.4). It
// resolves the inputs (electedMaster==self) and applies them to the group engine,
// which starts clock server + origin (+ local render iff Caps.Render). Nil-safe.
func (n *Node) runMaster(rctx context.Context, gen uint64) {
	n.applyRoleEngine(rctx, n.options.NodeID, gen)
}

// runFollower is the follower entry the role fence calls under rctx (A.14.4). It
// resolves the inputs (electedMaster==masterAddr's node) and applies them: clock
// follower + receiver + render (or clock-only for a sink-less member). Nil-safe.
func (n *Node) runFollower(rctx context.Context, gen uint64, master string) {
	n.applyRoleEngine(rctx, master, gen)
}

// applyRoleEngine resolves inputs and drives group.Engine.Apply. The engine's own
// reconcile is generation/streamGen fenced; the daemon role fence (roleState) has
// already cancelled the prior role ctx, so the hooks started here run under rctx.
// After the reconcile it syncs the doc-driven transport state the engine's
// Decision cannot express (media selection / Playing flips — syncTransport).
func (n *Node) applyRoleEngine(rctx context.Context, master string, gen uint64) {
	re := n.engineFor()
	if re == nil || re.engine == nil {
		return // skeleton / unit test: no realtime plane wired
	}
	in := re.inputs(n.options.NodeID, master, gen)
	// The hooks close over rctx (the fenced role ctx) so a role change tears the
	// started goroutines down. We thread it via the engine's hook calls; the engine
	// itself is ctx-agnostic, so the closures captured at build time read the live
	// rctx from the transport (set just below) — see buildHooks.
	n.setRoleCtx(rctx)
	d := re.engine.Apply(in)
	if hs := n.hooksFor(); hs != nil {
		hs.syncTransport(d, in)
	}
}

// hooksFor returns the live hookState under sessMu (nil before activate).
func (n *Node) hooksFor() *hookState {
	n.sessMu.Lock()
	defer n.sessMu.Unlock()
	if n.tx == nil {
		return nil
	}
	return n.tx.hooks
}

// engineFor returns the live roleEngine under sessMu (nil before activate).
func (n *Node) engineFor() *roleEngine {
	n.sessMu.Lock()
	defer n.sessMu.Unlock()
	if n.tx == nil {
		return nil
	}
	return n.tx.roleEngine
}

// setRoleCtx publishes the current fenced role ctx for the hook closures to read.
func (n *Node) setRoleCtx(ctx context.Context) {
	n.sessMu.Lock()
	if n.tx != nil {
		n.tx.roleCtx = ctx
	}
	n.sessMu.Unlock()
}

// roleCtxNow reads the current fenced role ctx (the hooks start goroutines under
// it). Falls back to the session ctx; with NO live session it returns a
// pre-cancelled ctx so a hook racing deactivate can never start an
// un-cancelable goroutine.
func (n *Node) roleCtxNow() context.Context {
	n.sessMu.Lock()
	defer n.sessMu.Unlock()
	if n.tx != nil && n.tx.roleCtx != nil {
		return n.tx.roleCtx
	}
	if n.activeCtx != nil {
		return n.activeCtx
	}
	return closedCtx
}

// closedCtx is a pre-cancelled context: the no-session fallback for roleCtxNow.
var closedCtx = func() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}()

// --- subsystem-construction config (A.12 canonical, threaded through) -------

// audioParams is the per-node audio config the hooks pass to the planes (A.12).
// device is the operator --device override; the rest are canonical constants the
// upstream pieces also default to, threaded here so the wiring is explicit.
type audioParams struct {
	rate           int
	channels       int
	framesPerChunk int
	leadMs         int
	device         string
	clockAddr      string // ":9000" — clock plane (A.12 Ports)
	audioAddr      string // ":9100" — audio plane
}

// canonicalAudio returns the A.12 canonical audio parameters with the operator
// device + port overrides applied.
func canonicalAudio(device, clockAddr, audioAddr string) audioParams {
	return audioParams{
		rate:           48000,
		channels:       2,
		framesPerChunk: 480,
		leadMs:         300,
		device:         device,
		clockAddr:      clockAddr,
		audioAddr:      audioAddr,
	}
}

// --- group.Hooks construction ------------------------------------------------

// hookState holds the per-session live subsystem handles the hooks create and
// tear down. One per active session. The engine serializes hook calls under its
// own mutex (Apply/Shutdown); mu additionally guards the handles against the
// loop's syncTransport racing a deactivate-driven engine.Shutdown.
type hookState struct {
	n     *Node
	ap    audioParams
	cp    *clusterPlane // live member endpoint resolution; nil in unit tests
	tl    *group.MasterTimeline // master timeline (origin + local loopback render)
	cfg   func() state.ConfigDoc
	allow *allowlist.Set

	mu   sync.Mutex
	srv  *clock.Server
	orig *originPlane
	fol  *clock.Follower
	recv *receiverPlane
	rend *renderPlane

	// loopRing buffers the origin's PCM tee for the master's OWN render (the
	// solo/master local playback path — without it a lone node plays silence
	// while its followers hear audio). Allocated once per session and Reset on
	// every origin (re)start so the running renderer's reader stays valid.
	loopRing *ring.Ring

	// lastTick is the renderer's most recent control-tick snapshot (sync /
	// want/played/error/underruns), kept for the -v status line. Guarded by mu.
	lastTick render.RenderTick

	// lastMedia/lastPlaying track the doc-driven transport state already applied
	// to the running origin, so syncTransport (re)starts/repoints it exactly when
	// the replicated selection or Playing changes (08 §F.2-§F.4).
	lastMedia   string
	lastPlaying bool

	// lastDevice is the audio-output device the running renderer's sink was
	// opened with; a doc-level device change (per-node persisted config, §D.3)
	// re-opens the sink via a render restart in syncTransport.
	lastDevice string
}

// deviceFor resolves THIS node's audio-output device: the persisted per-node
// NodeRecord.Device wins (set from the node detail screen, gossiped), falling
// back to the node-local --device flag, then the backend default ("").
func (h *hookState) deviceFor() string {
	if nr := nodeRecord(h.cfg(), h.n.options.NodeID); nr != nil && nr.Device != "" {
		return nr.Device
	}
	return h.ap.device
}

// originPlane bundles a running master origin + its cancel.
type originPlane struct {
	o      *origin.Origin
	cancel context.CancelFunc
}

// receiverPlane bundles a running follower receiver + its ring + cancel + the
// chunk-meta source the follower timeline reads. closer closes the bound socket
// (a *net.UDPConn on the UDP path, a *net.TCPListener on the TCP fallback path).
type receiverPlane struct {
	r      *sinknet.Receiver
	rng    *ring.Ring
	closer io.Closer
	cancel context.CancelFunc
}

// renderPlane bundles a running renderer + cancel.
type renderPlane struct {
	rd     *render.Renderer
	cancel context.CancelFunc
}

// buildHooks constructs the group.Hooks bound to this node's subsystem builders.
// Every hook starts its goroutine under the LIVE fenced role ctx (n.roleCtxNow),
// so a role change (which cancels that ctx via roleState) unwinds the plane. The
// hooks are idempotent w.r.t. the engine's reconcile (the engine never double-
// starts a running plane). Each entry takes h.mu so an engine reconcile and the
// loop's syncTransport never interleave on the subsystem handles.
func (h *hookState) buildHooks() group.Hooks {
	locked := func(fn func()) func() {
		return func() { h.mu.Lock(); defer h.mu.Unlock(); fn() }
	}
	return group.Hooks{
		StartClockServer: func(g string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.startClockServer(g)
		},
		StopClockServer: locked(h.stopClockServer),

		StartOrigin: func(g string, sg uint64) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.startOrigin(g, sg)
		},
		StopOrigin: locked(h.stopOrigin),
		OriginResumeAt: func(si int64, playing bool) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.originResumeAt(si, playing)
		},

		StartClockFollower: func(g, addr string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.startClockFollower(g, addr)
		},
		StopClockFollower: locked(h.stopClockFollower),

		StartReceiver: func(g string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.startReceiver(g)
		},
		StopReceiver:         locked(h.stopReceiver),
		ReceiverFlushReprime: locked(h.receiverFlushReprime),

		StartRender: func() error {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.startRender()
		},
		StopRender: locked(h.stopRender),
	}
}

// syncTransport reconciles the doc-driven transport state the engine's Decision
// does not encode: media selection and the Playing flag (08 §F.2-§F.4). The
// engine starts the origin when the role demands it, but media may be selected
// only later (play{file}); a running origin must re-point on a media change and
// pause/resume on a Playing flip (Timeline.Seed + origin.ResumeAt, 04 §4.4.4).
// It also refreshes the origin's listener set so a follower that appeared after
// origin start still receives (AddListener is idempotent by id). Called by
// applyRoleEngine after every engine.Apply — i.e. on every loop signal.
func (h *hookState) syncTransport(d group.Decision, in group.Inputs) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Persisted per-node device change (§D.3) applies to ANY rendering role
	// (master loopback AND follower): the sink is opened once at render start,
	// so a doc-level device switch re-opens it via a render restart.
	if h.rend != nil {
		if dev := h.deviceFor(); dev != h.lastDevice {
			h.stopRender()
			_ = h.startRender()
		}
	}
	if !d.RunOrigin {
		return
	}
	doc := h.cfg()
	media := mediaFor(doc, in.GroupID)
	switch {
	case h.orig == nil && media != "":
		// Media selected after the role start: bring the origin up now. The
		// already-running renderer reads the persistent loopRing; the streamGen
		// flip (0 → live) triggers its built-in reseek, so no restart is needed.
		if h.startOrigin(in.GroupID, d.StreamGen) == nil && h.orig != nil {
			h.originResumeAt(in.SampleIndex, in.Playing)
		}
	case h.orig != nil && media != "" && media != h.lastMedia:
		// Selection changed: re-point the origin at the new source.
		h.stopOrigin()
		if h.startOrigin(in.GroupID, d.StreamGen) == nil && h.orig != nil {
			h.originResumeAt(in.SampleIndex, in.Playing)
		}
	case h.orig != nil && in.Playing != h.lastPlaying:
		// Play/stop flip: reseed the timeline + bump the stream generation so
		// receivers flush+reprime (R11/D22).
		h.originResumeAt(in.SampleIndex, in.Playing)
	}
	if h.orig != nil {
		h.addListeners(h.orig.o, doc, in.GroupID)
	}
}

// startClockServer binds the UDP clock server gated by the source-IP allowlist
// (P2.4). Idempotent: a running server is left in place.
func (h *hookState) startClockServer(_ string) error {
	if h.srv != nil {
		return nil
	}
	var srv *clock.Server
	var err error
	if h.allow != nil {
		srv, err = clock.ListenGated(h.ap.clockAddr, h.allow.AllowedAddr)
	} else {
		srv, err = clock.Listen(h.ap.clockAddr)
	}
	if err != nil {
		return err
	}
	h.srv = srv
	return nil
}

func (h *hookState) stopClockServer() {
	if h.srv != nil {
		_ = h.srv.Close()
		h.srv = nil
	}
}

// startOrigin builds and runs the master-side stream origin over the selected
// media (source.Open, looping) at the canonical profile, sending one paced unicast
// stream per render-capable listener. Idempotent. The PCM/none baseline is wired
// (P5 swaps in opus/xor via the negotiated profile).
func (h *hookState) startOrigin(groupID string, streamGen uint64) error {
	if h.orig != nil {
		return nil
	}
	doc := h.cfg()
	media := mediaFor(doc, groupID)
	if media == "" {
		return nil // nothing selected yet; origin starts when play selects media
	}
	src, err := source.Open(h.n.mediaPath(media), h.ap.rate, h.ap.channels)
	if err != nil {
		return err
	}
	c, err := codec.New(codec.PCM)
	if err != nil {
		src.Close()
		return err
	}
	f := fec.NewNone()
	o := origin.New(h.tl, c, f, src, origin.Config{
		Rate:           h.ap.rate,
		Channels:       h.ap.channels,
		FramesPerChunk: h.ap.framesPerChunk,
		Lead:           time.Duration(h.ap.leadMs) * time.Millisecond,
		StreamGen:      streamGen,
		Transport:      transportFor(doc, groupID), // UDP default; TCP forces fec.None (05 §5.9)
	})
	// Local render tee: the master hears its own stream through the loop ring
	// (no UDP self-loop). Reset drops any prior generation's samples.
	h.loopRing.Reset()
	o.WithLoopback(func(pcm []float32) { h.loopRing.Write(pcm) })
	// Register render-capable listeners (other members) at the audio plane.
	h.addListeners(o, doc, groupID)

	ctx, cancel := context.WithCancel(h.n.roleCtxNow())
	h.orig = &originPlane{o: o, cancel: cancel}
	h.lastMedia = media
	go func() {
		defer src.Close()
		_ = o.Run(ctx)
	}()
	return nil
}

func (h *hookState) stopOrigin() {
	if h.orig != nil {
		h.orig.cancel()
		h.orig = nil
	}
}

// originResumeAt seeds the master timeline for failover continuity (Timeline.Seed,
// 04 §4.4.4) and re-points a running origin (origin.ResumeAt, A.14.4). Called by
// the engine on (re)start with the replicated sampleIndex + Playing.
func (h *hookState) originResumeAt(sampleIndex int64, playing bool) {
	if h.tl != nil {
		h.tl.Seed(sampleIndex, playing)
	}
	if h.orig != nil {
		h.orig.o.ResumeAt(sampleIndex, playing)
		// The bump re-keys the stream; drop the prior generation's local PCM so
		// the master's own render re-primes cleanly (the R11 analog).
		h.loopRing.Reset()
	}
	h.lastPlaying = playing
}

// startClockFollower runs the clock follower against the master's clock-plane
// addr (resolved by the engine's WithMasterAddr). Idempotent. The estimator window/
// alpha are P3.1 defaults; P4.9 threads only the address.
func (h *hookState) startClockFollower(_, masterAddr string) error {
	if h.fol != nil {
		return nil
	}
	var fol *clock.Follower
	fol = clock.NewFollower(clock.WithSampleHook(func(_ clock.Sample, _ time.Duration) {
		// Feed the engine's orphan gate with the latest clock health (04 §4.2.3).
		if md, ok := fol.MinDelay(); ok {
			h.engineSetClockHealth(md, true)
		}
	}))
	h.fol = fol
	ctx := h.n.roleCtxNow()
	go func() { _ = fol.Run(ctx, masterAddr) }()
	return nil
}

func (h *hookState) stopClockFollower() {
	// The follower stops when its role ctx is cancelled by the role fence; we just
	// drop the handle so a re-point builds a fresh one.
	h.fol = nil
}

// startReceiver builds the follower-side receiver (recv→allowlist→FEC.Recover→
// Codec.Decode→ring) bound to the audio plane, dispatching the UDP listen or the
// TCP-fallback listen per the group's negotiated transport (05 §5.9). Idempotent.
func (h *hookState) startReceiver(groupID string) error {
	if h.recv != nil {
		return nil
	}
	transport := transportFor(h.cfg(), groupID)

	c, err := codec.New(codec.PCM)
	if err != nil {
		return err
	}
	rng := ring.NewRing(ringSamples(h.ap))
	rcv := sinknet.New(c, fec.NewNone(), rng, h.allow, sinknet.Config{
		Rate:           h.ap.rate,
		Channels:       h.ap.channels,
		FramesPerChunk: h.ap.framesPerChunk,
		Transport:      transport,
	})
	ctx, cancel := context.WithCancel(h.n.roleCtxNow())

	if transport == sinknet.TransportTCP {
		taddr, err := net.ResolveTCPAddr("tcp", h.ap.audioAddr)
		if err != nil {
			cancel()
			return err
		}
		ln, err := net.ListenTCP("tcp", taddr)
		if err != nil {
			cancel()
			return err
		}
		h.recv = &receiverPlane{r: rcv, rng: rng, closer: ln, cancel: cancel}
		go func() {
			defer ln.Close()
			_ = rcv.RunTCP(ctx, ln)
		}()
		return nil
	}

	uaddr, err := net.ResolveUDPAddr("udp", h.ap.audioAddr)
	if err != nil {
		cancel()
		return err
	}
	conn, err := net.ListenUDP("udp", uaddr)
	if err != nil {
		cancel()
		return err
	}
	h.recv = &receiverPlane{r: rcv, rng: rng, closer: conn, cancel: cancel}
	go func() {
		defer conn.Close()
		_ = rcv.Run(ctx, conn)
	}()
	return nil
}

func (h *hookState) stopReceiver() {
	if h.recv != nil {
		h.recv.cancel()
		h.recv = nil
	}
}

// receiverFlushReprime drops the receiver's buffered chunks so the follower
// re-primes ~one buffer on (re)start (R11 / A.14.4).
func (h *hookState) receiverFlushReprime() {
	if h.recv != nil {
		h.recv.r.FlushAndReprime()
	}
}

// startRender opens the node's audio sink (Options.OpenSink override first —
// tests inject a capturing fake) and runs the renderer. For the master/solo it
// loops back the master timeline; for a follower it chases the follower
// timeline projected over the receiver's chunk meta + clock follower. Idempotent.
func (h *hookState) startRender() error {
	if h.rend != nil {
		return nil
	}
	device := h.deviceFor()
	open := func() (sink.AudioSink, error) {
		s, backend, err := sink.OpenNamed(nil, device)
		if err != nil {
			logf(h.n.options.Log, "render: no usable audio sink (device=%q): %v", device, err)
			return nil, err
		}
		logf(h.n.options.Log, "render: sink=%s device=%q", backend, device)
		return s, nil
	}
	if h.n.options.OpenSink != nil {
		open = h.n.options.OpenSink // injected fake (tests)
	}
	snk, err := open()
	if err != nil {
		return err
	}
	h.lastDevice = device
	tl := h.renderTimeline()
	src := h.renderSource()
	rd := render.NewRenderer(snk, tl, h.cfg, src, h.n.options.NodeID, render.RendererParams{
		Rate:     h.ap.rate,
		Channels: h.ap.channels,
		LeadMs:   h.ap.leadMs,
	})
	// Keep the latest control-tick snapshot for the -v status line.
	rd.SetOnTick(func(ti render.RenderTick) {
		h.mu.Lock()
		h.lastTick = ti
		h.mu.Unlock()
	})
	ctx, cancel := context.WithCancel(h.n.roleCtxNow())
	h.rend = &renderPlane{rd: rd, cancel: cancel}
	go func() { _ = rd.Run(ctx) }()
	return nil
}

func (h *hookState) stopRender() {
	if h.rend != nil {
		h.rend.cancel()
		h.rend = nil
	}
}

// renderTimeline returns the group.Timeline the renderer chases: the master
// timeline when this node origins (loopback), else a follower projection over the
// receiver chunk meta + clock follower.
func (h *hookState) renderTimeline() group.Timeline {
	if h.orig != nil || h.recv == nil {
		return h.tl // master/solo loopback
	}
	clk := group.OrphanClock()
	if h.fol != nil {
		clk = group.FollowerClock(h.fol)
	}
	return group.NewFollowerTimeline(chunkMetaAdapter{h.recv.r}, clk, h.ap.rate)
}

// renderSource returns the renderer's FrameReader: the receiver ring on a
// follower; on a master/solo the origin's local PCM tee (loopRing) — so a lone
// node actually HEARS what it plays — with a silent reader only when neither
// path exists (no origin and no receiver: nothing to render yet).
func (h *hookState) renderSource() render.FrameReader {
	if h.recv != nil {
		return ringReader{rng: h.recv.rng, gen: func() uint64 { return h.recv.r.StreamGen() }}
	}
	return ringReader{rng: h.loopRing, gen: func() uint64 {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.orig != nil {
			return h.orig.o.StreamGen()
		}
		return 0
	}}
}

// addListeners registers the audio-plane unicast destination for every OTHER
// render-capable member of the group (D17: only Render=true members receive).
// The destination is resolved from the live gossip Meta (correct per-node audio
// port) when the plane runs, falling back to NodeRecord.Addrs + the local audio
// port. AddListener is idempotent by id, so re-running this on every
// syncTransport refresh is safe and picks up late-joining followers.
func (h *hookState) addListeners(o *origin.Origin, doc state.ConfigDoc, groupID string) {
	g := groupRecord(doc, groupID)
	if g == nil {
		return
	}
	_, portStr, _ := net.SplitHostPort(h.ap.audioAddr)
	for _, id := range g.MemberNodeIDs {
		if id == h.n.options.NodeID {
			continue
		}
		nr := nodeRecord(doc, id)
		if nr == nil || !nr.Caps.Render {
			continue
		}
		hostPort := ""
		if h.cp != nil {
			if a, ok := h.cp.audioAddrOf(id); ok {
				hostPort = a
			}
		}
		if hostPort == "" {
			if len(nr.Addrs) == 0 {
				continue
			}
			hostPort = net.JoinHostPort(nr.Addrs[0], portStr)
		}
		addr, err := net.ResolveUDPAddr("udp", hostPort)
		if err != nil {
			continue
		}
		_ = o.AddListener(id, addr)
	}
}

// statusLine renders the -v playback/stream/clock status snapshot the role loop
// logs periodically: role + replicated transport state, origin generation /
// listener count / local loop-ring fill, receiver chunk meta + ring fill, the
// renderer's latest control tick (sync, want/played, drift error+ratio,
// underruns) and the clock follower's offset/min-delay.
func (h *hookState) statusLine(role string, doc state.ConfigDoc, groupID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()

	var b strings.Builder
	fmt.Fprintf(&b, "status: role=%s", role)
	if g := groupRecord(doc, groupID); g != nil {
		fmt.Fprintf(&b, " playing=%t media=%q", g.Playing, g.Media.File)
	}
	if h.orig != nil {
		fmt.Fprintf(&b, " origin{gen=%d listeners=%d loopring=%d}",
			h.orig.o.StreamGen(), h.orig.o.Listeners(), h.loopRing.Len())
	}
	if h.recv != nil {
		si, _, gen, playing, ok := h.recv.r.LatestChunkMeta()
		fmt.Fprintf(&b, " recv{gen=%d sample=%d playing=%t meta=%t ring=%d}",
			gen, si, playing, ok, h.recv.rng.Len())
	}
	if h.rend != nil {
		ti := h.lastTick
		fmt.Fprintf(&b, " render{sync=%t want=%d played=%d err=%d ppm=%+.0f underruns=%d}",
			ti.HaveSync, ti.WantSample, ti.PlayedSample, ti.ErrorSamples, ti.RatioPPM, ti.Underruns)
	}
	if h.fol != nil {
		if off, ok := h.fol.Offset(); ok {
			fmt.Fprintf(&b, " clock{offset=%s", off)
			if md, mok := h.fol.MinDelay(); mok {
				fmt.Fprintf(&b, " mindelay=%s", md)
			}
			b.WriteString("}")
		} else {
			b.WriteString(" clock{unsynced}")
		}
	}
	return b.String()
}

// engineSetClockHealth feeds the orphan gate from the clock follower sample hook.
func (h *hookState) engineSetClockHealth(minDelay time.Duration, ok bool) {
	if re := h.n.engineFor(); re != nil && re.engine != nil {
		re.engine.SetClockHealth(minDelay, ok)
	}
}

// --- small wiring helpers ----------------------------------------------------

// ringSamples sizes the receiver jitter ring to ~2× LeadMs of interleaved audio.
func ringSamples(ap audioParams) int {
	lead := ap.leadMs
	if lead <= 0 {
		lead = 300
	}
	return 2 * ap.rate * ap.channels * lead / 1000
}

// chunkMetaAdapter adapts the receiver's flat LatestChunkMeta accessor to the
// group.ChunkMetaSource interface the FollowerTimeline consumes (sink_net must not
// import group, 01 §2 — P4.9 owns the adapter).
type chunkMetaAdapter struct{ r *sinknet.Receiver }

func (a chunkMetaAdapter) LatestChunkMeta() (group.ChunkMeta, bool) {
	si, mm, gen, playing, ok := a.r.LatestChunkMeta()
	if !ok {
		return group.ChunkMeta{}, false
	}
	return group.ChunkMeta{SampleIndex: si, MasterMono: mm, StreamGen: gen, Playing: playing}, true
}

// ringReader adapts a *ring.Ring to render.FrameReader (the follower decode path
// writes the ring; the renderer drains it). StreamGen is read from a closure over
// the receiver so a media/seek discontinuity triggers a reseek (doc 06 §6.3).
type ringReader struct {
	rng *ring.Ring
	gen func() uint64
}

func (r ringReader) Read(p []float32) (int, error) { return r.rng.Read(p), nil }
func (r ringReader) StreamGen() uint64             { return r.gen() }

// silentReader is the FrameReader for a master loopback with no receive path: it
// yields no frames so the renderer feeds silence (the renderer's underrun path).
type silentReader struct{}

func (silentReader) Read(p []float32) (int, error) { return 0, nil }
func (silentReader) StreamGen() uint64             { return 0 }

// mediaFor / groupRecord / nodeRecord read the replicated doc for the origin/
// listener wiring.
func mediaFor(doc state.ConfigDoc, groupID string) string {
	if g := groupRecord(doc, groupID); g != nil {
		return g.Media.File
	}
	return ""
}

// transportFor resolves the group's negotiated audio transport (05 §5.9 / 07
// §2.4): GroupRecord.Profile.Transport "tcp" selects the reliable fallback,
// anything else (including "" and "udp") the UDP default.
func transportFor(doc state.ConfigDoc, groupID string) sinknet.Transport {
	if g := groupRecord(doc, groupID); g != nil {
		return sinknet.ParseTransport(g.Profile.Transport)
	}
	return sinknet.TransportUDP
}

func groupRecord(doc state.ConfigDoc, groupID string) *state.GroupRecord {
	for i := range doc.Groups {
		if doc.Groups[i].ID == groupID {
			return &doc.Groups[i]
		}
	}
	return nil
}

func nodeRecord(doc state.ConfigDoc, id string) *state.NodeRecord {
	for i := range doc.Nodes {
		if doc.Nodes[i].ID == id {
			return &doc.Nodes[i]
		}
	}
	return nil
}

// mediaPath resolves a media file name (possibly a nested data/-relative path)
// to its absolute path under the node's data/ folder (08 §F.2: master-side
// decode reads from data/). The relative path is traversal-sanitized so a doc
// entry can never escape data/. An http(s):// URL or an already-absolute path
// is returned unchanged.
func (n *Node) mediaPath(file string) string {
	if n.options.Paths.Data == "" || isURL(file) || filepath.IsAbs(file) {
		return file
	}
	return filepath.Join(n.options.Paths.Data, filepath.FromSlash(cleanRelPath(file)))
}

// isURL reports whether s is an http(s) stream URL (source.Open accepts those).
func isURL(s string) bool {
	return len(s) >= 7 && (s[:7] == "http://" || (len(s) >= 8 && s[:8] == "https://"))
}
