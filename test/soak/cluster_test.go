//go:build soak

package soak

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/allowlist"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/audio/ring"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/group"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/codec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/fec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/origin"
	sinknet "gitlab.rand0m.me/ruben/go/ensemble/internal/stream/sink_net"
)

func mustNetip(s string) netip.Addr { return netip.MustParseAddr(s) }
func itoa(i int) string             { return "f" + strconv.Itoa(i) }

const (
	soakRate     = 48000
	soakChannels = 2
	soakFrames   = 480
)

// syntheticSource is a deterministic, allocation-free PCM source (a ramp keyed on
// the absolute frame index) so the harness asserts bit-exact alignment without a
// real mp3 (doc P7.1 §3). It loops at total but never resets pos.
type syntheticSource struct {
	pos   int64
	total int64
}

func (s *syntheticSource) Rate() int     { return soakRate }
func (s *syntheticSource) Channels() int { return soakChannels }
func (s *syntheticSource) Close() error  { return nil }

func (s *syntheticSource) Read(dst []float32) (int, error) {
	n := len(dst) - len(dst)%soakChannels
	for i := 0; i < n; i += soakChannels {
		f := s.pos % s.total
		for c := 0; c < soakChannels; c++ {
			dst[i+c] = float32((f*soakChannels+int64(c))%256) / 32768
		}
		s.pos++
	}
	return n, nil
}

// follower is one in-process follower node: a real receiver over a real loopback
// UDP socket fed by a lossy relay, projected through a real FollowerTimeline.
type follower struct {
	id    string
	recv  *sinknet.Receiver
	conn  *net.UDPConn
	relay *lossyRelay
	tl    *group.FollowerTimeline
}

// zeroClock is a follower clock source with a fixed zero offset: in-process nodes
// share clock.NowMono, so the clock plane is exact here. The real clock-sync plane
// (offset estimation, EWMA, min-delay filter) is validated by the clock package's
// own tests and the P3 integration; this harness exercises the stream/timeline/
// failover/allowlist machinery (doc P7.1 §5.7).
type zeroClock struct{}

func (zeroClock) Offset() (time.Duration, bool)   { return 0, true }
func (zeroClock) MinDelay() (time.Duration, bool) { return 0, true }

// recvChunkMeta adapts the receiver's flat accessor to group.ChunkMetaSource.
type recvChunkMeta struct{ r *sinknet.Receiver }

func (a recvChunkMeta) LatestChunkMeta() (group.ChunkMeta, bool) {
	si, mm, gen, playing, ok := a.r.LatestChunkMeta()
	if !ok {
		return group.ChunkMeta{}, false
	}
	return group.ChunkMeta{SampleIndex: si, MasterMono: mm, StreamGen: gen, Playing: playing}, true
}

// harness is an in-process cluster: one master (timeline + origin) and N followers
// (receiver + follower timeline) over loopback, each behind a lossy relay.
type harness struct {
	t         *testing.T
	mtl       *group.MasterTimeline
	origin    *origin.Origin
	followers []*follower
	allow     *allowlist.Set
	cancel    context.CancelFunc
	ctx       context.Context
	gen       uint64
}

// newHarness builds the master timeline + origin and n followers, wires the relays,
// and starts everything. streamGen seeds the origin's generation.
func newHarness(t *testing.T, n int, streamGen uint64) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	// Everything is on 127.0.0.1, so the allowlist admits loopback.
	allow := allowlist.New()
	allow.Update(state.ConfigDoc{}, []allowlist.MemberAddr{{Addr: mustNetip("127.0.0.1")}})

	mtl := group.NewMasterTimeline(soakRate)
	mtl.Play(0)

	c, err := codec.New(codec.PCM)
	if err != nil {
		t.Fatal(err)
	}
	src := &syntheticSource{total: soakRate} // 1 s loop
	o := origin.New(mtl, c, fec.NewNone(), src, origin.Config{
		Rate: soakRate, Channels: soakChannels, FramesPerChunk: soakFrames, StreamGen: streamGen,
	})

	h := &harness{t: t, mtl: mtl, origin: o, allow: allow, ctx: ctx, cancel: cancel, gen: streamGen}

	for i := 0; i < n; i++ {
		h.addFollower(i)
	}

	go func() { _ = o.Run(ctx) }()
	return h
}

func (h *harness) addFollower(i int) {
	t := h.t
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	dst := conn.LocalAddr().(*net.UDPAddr)
	relay, err := newLossyRelay(dst, int64(1000+i))
	if err != nil {
		t.Fatal(err)
	}

	rc, _ := codec.New(codec.PCM)
	rng := ring.NewRing(soakRate * soakChannels)
	recv := sinknet.New(rc, fec.NewNone(), rng, h.allow, sinknet.Config{
		Rate: soakRate, Channels: soakChannels, FramesPerChunk: soakFrames, LeadMs: 300,
	})
	ftl := group.NewFollowerTimeline(recvChunkMeta{recv}, zeroClock{}, soakRate)
	ftl.SetStreamGen(h.gen)

	f := &follower{id: itoa(i), recv: recv, conn: conn, relay: relay, tl: ftl}
	h.followers = append(h.followers, f)

	go relay.run(h.ctx)
	go func() { _ = recv.Run(h.ctx, conn) }()
	// The origin sends to the relay ingress for this follower.
	if err := h.origin.AddListener(f.id, relay.ingressAddr()); err != nil {
		t.Fatal(err)
	}
}

// bumpGen drives a master-change/seek style generation bump on the origin and
// updates the followers' timeline gate to the new gen (mirrors what the engine
// does on a master change). Returns the new gen.
func (h *harness) bumpGen(atSample int64) uint64 {
	g := h.origin.ResumeAt(atSample, true)
	h.gen = g
	for _, f := range h.followers {
		f.tl.SetStreamGen(g)
		f.recv.FlushAndReprime()
	}
	return g
}

func (h *harness) stop() { h.cancel(); settle() }

// sampleSync samples the worst follower sync error over dur, returning the worst
// across all followers. It waits for followers to lock first.
func (h *harness) sampleSync(dur time.Duration) time.Duration {
	h.waitLocked(3 * time.Second)
	samplers := make([]*sampler, len(h.followers))
	for i := range samplers {
		samplers[i] = newSampler(soakRate)
	}
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		ms, _, _ := h.mtl.NowSample()
		for i, f := range h.followers {
			fs, _, ok := f.tl.NowSample()
			samplers[i].observe(ms, fs, ok)
		}
		time.Sleep(time.Millisecond)
	}
	var worst time.Duration
	for _, s := range samplers {
		if s.worst > worst {
			worst = s.worst
		}
	}
	return worst
}

// waitLocked blocks until every follower's timeline reports ok (locked + primed)
// or the deadline elapses.
func (h *harness) waitLocked(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		all := true
		for _, f := range h.followers {
			if _, _, ok := f.tl.NowSample(); !ok {
				all = false
				break
			}
		}
		if all {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// boundedSync is the in-process timeline-vs-timeline sync bound for the compressed
// soak. The follower projects from the most-recent chunk's header anchor, whose
// MasterMono is stamped at produce time (up to one chunk period ahead of the true
// playout instant the master timeline advances on), so the timeline-vs-timeline
// comparison quantizes to ~one chunk (10 ms, A.12 FramesPerChunk=480) plus a
// little scheduling slack. The LITERAL sub-ms acceptance (A.13 P7) is the hardware
// Pi+DAC run with the real clock-sync plane (doc P7.1 §5.7 / §9.4); this proxy
// asserts the error stays BOUNDED (no drift, no permanent desync) across faults.
const boundedSync = 15 * time.Millisecond

// TestSoakSteadyStateSync: an N-follower group with no faults holds bounded
// inter-node sync at steady state — no drift (A.13 P4/P7; sub-ms is the hardware
// gate, this is the bounded-stability proxy).
func TestSoakSteadyStateSync(t *testing.T) {
	h := newHarness(t, 3, 1)
	defer h.stop()

	worst := h.sampleSync(2 * time.Second)
	if worst > boundedSync {
		t.Errorf("steady-state worst sync error=%v, want <= %v (bounded, no drift)", worst, boundedSync)
	}
	t.Logf("steady-state worst sync error across 3 followers: %v (hardware gate: sub-ms)", worst)
}

// TestSoakGenChangeResync: a streamGen bump (failover/seek proxy) causes the
// followers to flush + re-prime and re-lock, returning to BOUNDED sync afterwards
// with no permanent desync (§5.1). The post-fault error must not exceed the
// pre-fault error by more than the bound (no cumulative drift).
func TestSoakGenChangeResync(t *testing.T) {
	h := newHarness(t, 3, 1)
	defer h.stop()

	pre := h.sampleSync(500 * time.Millisecond)
	if pre > boundedSync {
		t.Fatalf("pre-bump sync=%v, want <= %v", pre, boundedSync)
	}

	ms, _, _ := h.mtl.NowSample()
	h.bumpGen(ms) // continuity bump at the current position (R11)

	worst := h.sampleSync(2 * time.Second)
	if worst > boundedSync {
		t.Errorf("post-gen-change worst sync error=%v, want <= %v (re-lock, no desync)", worst, boundedSync)
	}
	t.Logf("gen-change resync: pre=%v post=%v", pre, worst)
}
