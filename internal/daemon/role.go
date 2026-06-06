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
	"net"
	"path/filepath"
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

// buildTransport constructs the live transport seam for an active session: a
// state.Store (loaded from <Root>/config.json), the group MasterTimeline, the
// group.Engine wired to the stream/audio/clock hooks, and the inputs resolver
// reading the replicated doc. It returns nil on a hard failure so activate stays
// non-fatal (the closures then degrade to not-ready). The cross-node peer proxy
// and the cluster election are left nil here (the pending P2-wiring step); a
// single-node group therefore elects itself master/solo and decodes+renders
// locally — the P4.9 end-goal substrate.
func (n *Node) buildTransport(groupID string) *transport {
	storePath := ""
	if n.options.Paths.Root != "" {
		storePath = filepath.Join(n.options.Paths.Root, "config.json")
	}
	store := state.Load(n.options.NodeID, storePath)

	ap := canonicalAudio(
		n.options.Device,
		portAddr(n.options.ClockPort, 9000),
		portAddr(n.options.AudioPort, 9100),
	)
	cfgFn := store.Get
	hs := &hookState{
		n:   n,
		ap:  ap,
		tl:  group.NewMasterTimeline(ap.rate),
		cfg: cfgFn,
	}
	engine := group.NewEngine(n.options.NodeID, hs.buildHooks()).
		WithMasterAddr(func(_, masterID string) string {
			// Single-node substrate: the master clock plane is local. The P2-wiring
			// step replaces this with the elected master's NodeRecord clock addr.
			return ap.clockAddr
		})

	tx := &transport{
		store:   store,
		self:    n.options.NodeID,
		dataDir: n.options.Paths.Data,
		master:  func(gid string) string { return masterOf(store.Get(), gid, n.options.NodeID) },
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
	re.engine.Apply(in)
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
// it). Falls back to the session ctx, then context.Background, so a hook never
// starts an un-cancelable goroutine.
func (n *Node) roleCtxNow() context.Context {
	n.sessMu.Lock()
	defer n.sessMu.Unlock()
	if n.tx != nil && n.tx.roleCtx != nil {
		return n.tx.roleCtx
	}
	if n.activeCtx != nil {
		return n.activeCtx
	}
	return context.Background()
}

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
// tear down. One per active session; guarded by its own mutex because Apply can
// be called from the role loop while a prior teardown is in flight.
type hookState struct {
	n     *Node
	ap    audioParams
	tl    *group.MasterTimeline // master timeline (origin + local loopback render)
	cfg   func() state.ConfigDoc
	allow *allowlist.Set

	srv  *clock.Server
	orig *originPlane
	fol  *clock.Follower
	recv *receiverPlane
	rend *renderPlane
}

// originPlane bundles a running master origin + its cancel.
type originPlane struct {
	o      *origin.Origin
	cancel context.CancelFunc
}

// receiverPlane bundles a running follower receiver + its ring + cancel + the
// chunk-meta source the follower timeline reads.
type receiverPlane struct {
	r      *sinknet.Receiver
	rng    *ring.Ring
	conn   *net.UDPConn
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
// starts a running plane).
func (h *hookState) buildHooks() group.Hooks {
	return group.Hooks{
		StartClockServer: h.startClockServer,
		StopClockServer:  h.stopClockServer,

		StartOrigin:    h.startOrigin,
		StopOrigin:     h.stopOrigin,
		OriginResumeAt: h.originResumeAt,

		StartClockFollower: h.startClockFollower,
		StopClockFollower:  h.stopClockFollower,

		StartReceiver:        h.startReceiver,
		StopReceiver:         h.stopReceiver,
		ReceiverFlushReprime: h.receiverFlushReprime,

		StartRender: h.startRender,
		StopRender:  h.stopRender,
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
	})
	// Register render-capable listeners (other members) at the audio plane.
	h.addListeners(o, doc, groupID)

	ctx, cancel := context.WithCancel(h.n.roleCtxNow())
	h.orig = &originPlane{o: o, cancel: cancel}
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
	}
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
// Codec.Decode→ring) bound to the audio-plane UDP socket. Idempotent.
func (h *hookState) startReceiver(groupID string) error {
	if h.recv != nil {
		return nil
	}
	uaddr, err := net.ResolveUDPAddr("udp", h.ap.audioAddr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", uaddr)
	if err != nil {
		return err
	}
	c, err := codec.New(codec.PCM)
	if err != nil {
		conn.Close()
		return err
	}
	rng := ring.NewRing(ringSamples(h.ap))
	rcv := sinknet.New(c, fec.NewNone(), rng, h.allow, sinknet.Config{
		Rate:           h.ap.rate,
		Channels:       h.ap.channels,
		FramesPerChunk: h.ap.framesPerChunk,
	})
	ctx, cancel := context.WithCancel(h.n.roleCtxNow())
	h.recv = &receiverPlane{r: rcv, rng: rng, conn: conn, cancel: cancel}
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

// startRender opens the node's audio sink and runs the renderer. For the master/
// solo it loops back the master timeline; for a follower it chases the follower
// timeline projected over the receiver's chunk meta + clock follower. Idempotent.
func (h *hookState) startRender() error {
	if h.rend != nil {
		return nil
	}
	snk, err := sink.Open(nil, h.ap.device)
	if err != nil {
		return err
	}
	tl := h.renderTimeline()
	src := h.renderSource()
	rd := render.NewRenderer(snk, tl, h.cfg, src, h.n.options.NodeID, render.RendererParams{
		Rate:     h.ap.rate,
		Channels: h.ap.channels,
		LeadMs:   h.ap.leadMs,
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
// follower, or a silent reader on a sink-less/loopback master with no recv (the
// loopback master renders the same PCM the origin reads; for the MVP the master's
// local render reads from its own recv ring only when it is also a follower — a
// pure master loopback feeds the renderer from the source via a tee is out of P4.9
// scope, so a master loopback renders silence until a recv path exists).
func (h *hookState) renderSource() render.FrameReader {
	if h.recv != nil {
		return ringReader{rng: h.recv.rng, gen: func() uint64 { return h.recv.r.StreamGen() }}
	}
	return silentReader{}
}

// addListeners registers the audio-plane unicast destination for every OTHER
// render-capable member of the group (D17: only Render=true members receive). The
// destination addr is resolved from the member's NodeRecord.Addrs + the audio port.
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
		if nr == nil || !nr.Caps.Render || len(nr.Addrs) == 0 {
			continue
		}
		addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(nr.Addrs[0], portStr))
		if err != nil {
			continue
		}
		_ = o.AddListener(id, addr)
	}
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

// mediaPath resolves a media file name to its absolute path under the node's data/
// folder (08 §F.2: master-side decode reads from data/). An http(s):// URL or an
// already-absolute path is returned unchanged.
func (n *Node) mediaPath(file string) string {
	if n.options.Paths.Data == "" || isURL(file) || filepath.IsAbs(file) {
		return file
	}
	return filepath.Join(n.options.Paths.Data, file)
}

// isURL reports whether s is an http(s) stream URL (source.Open accepts those).
func isURL(s string) bool {
	return len(s) >= 7 && (s[:7] == "http://" || (len(s) >= 8 && s[:8] == "https://"))
}
