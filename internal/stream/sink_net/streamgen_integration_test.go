package sink_net_test

// This file is an EXTERNAL test package (sink_net_test) because it drives a real
// origin.Origin into a real sink_net.Receiver, and origin imports sink_net (for
// the Transport type, P7.1) — an in-package test importing origin would cycle.
// It reaches the receiver's push log via the exported CaptureReceiver shim
// (export_test.go).

import (
	"context"
	"net"
	"testing"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/codec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/fec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/origin"
	sinknet "gitlab.rand0m.me/ruben/go/ensemble/internal/stream/sink_net"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/streamgen"
)

const (
	tFrames   = 480
	tChannels = 2
)

// rampSource is a deterministic source.Reader: each frame is a ramp keyed on the
// absolute frame index so the receiver can tell pre-seek from post-seek audio. It
// loops at total but never resets pos, modelling looped content.
type rampSource struct {
	channels int
	total    int64
	pos      int64
}

func (s *rampSource) Rate() int     { return 48000 }
func (s *rampSource) Channels() int { return s.channels }
func (s *rampSource) Close() error  { return nil }

func (s *rampSource) Read(dst []float32) (int, error) {
	n := len(dst) - len(dst)%s.channels
	for i := 0; i < n; i += s.channels {
		f := s.pos % s.total
		for c := 0; c < s.channels; c++ {
			dst[i+c] = float32((f*int64(s.channels)+int64(c))%256) / 32768
		}
		s.pos++
	}
	return n, nil
}

// fixedTimeline is a minimal origin.Timeline returning a fixed anchor.
type fixedTimeline struct {
	sample  int64
	playing bool
}

func (t fixedTimeline) NowSample() (int64, bool, bool) { return t.sample, t.playing, true }

func canonCfg() sinknet.Config {
	return sinknet.Config{Rate: 48000, Channels: tChannels, FramesPerChunk: tFrames, WindowPackets: 32}
}

// TestIntegrationSeekReanchor wires a REAL Origin → loopback UDP → REAL Receiver +
// streamgen.Gate + ring, plays a few chunks, then Bumps (seek) on the origin and
// asserts the receiver flushes & re-anchors at the seek sampleIndex with no
// pre-seek samples surviving onto the post-seek anchor. PCM codec + None FEC.
func TestIntegrationSeekReanchor(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	dst := conn.LocalAddr().(*net.UDPAddr)

	rc, err := codec.New(codec.PCM)
	if err != nil {
		t.Fatal(err)
	}
	r := sinknet.NewCaptureReceiver(rc, fec.NewNone(), sinknet.AllowingSet("127.0.0.1"), canonCfg())

	rctx, rcancel := context.WithCancel(context.Background())
	rdone := make(chan struct{})
	go func() { _ = r.Run(rctx, conn); close(rdone) }()
	defer func() { rcancel(); <-rdone }()

	oc, err := codec.New(codec.PCM)
	if err != nil {
		t.Fatal(err)
	}
	src := &rampSource{channels: tChannels, total: 48000}
	o := origin.New(fixedTimeline{sample: 0, playing: true}, oc, fec.NewNone(), src, origin.Config{
		Rate: 48000, Channels: tChannels, FramesPerChunk: tFrames, StreamGen: 1,
	})
	if err := o.AddListener("rcv", dst); err != nil {
		t.Fatal(err)
	}
	octx, ocancel := context.WithCancel(context.Background())
	odone := make(chan struct{})
	go func() { _ = o.Run(octx); close(odone) }()
	defer func() { ocancel(); <-odone }()

	waitPushes(t, r, 3)
	if r.StreamGen() != 1 {
		t.Fatalf("receiver gen=%d want 1", r.StreamGen())
	}

	const seek = 480000
	if g := o.Bump(streamgen.ReasonSeek, seek, true); g != 2 {
		t.Fatalf("Bump(seek) gen=%d want 2", g)
	}

	deadline := time.After(3 * time.Second)
	for r.StreamGen() != 2 {
		select {
		case <-deadline:
			t.Fatalf("receiver did not adopt gen 2 (still %d)", r.StreamGen())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	var firstSeekIdx int64 = -1
	firstSeekPos := -1
	deadline = time.After(3 * time.Second)
	for firstSeekIdx < 0 {
		for i := 0; i < r.Pushes(); i++ {
			if idx, _ := r.PushAtIdx(i); idx >= seek {
				firstSeekIdx = idx
				firstSeekPos = i
				break
			}
		}
		if firstSeekIdx >= 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no post-seek push observed")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if firstSeekIdx != seek {
		t.Errorf("first post-seek push idx=%d want %d (crisp re-anchor)", firstSeekIdx, seek)
	}
	// The re-anchor invariant is ORDER-based: once the seek anchor has pushed,
	// no pre-seek sample may follow it. (How many pre-seek chunks ship BEFORE
	// the bump is timing-dependent — the origin keeps pacing 10 ms chunks while
	// the test observes — so a count/window assertion is inherently flaky.)
	for i := firstSeekPos + 1; i < r.Pushes(); i++ {
		idx, _ := r.PushAtIdx(i)
		if idx < seek {
			t.Errorf("stale pre-seek push at idx=%d AFTER the seek anchor", idx)
		}
	}
}

// TestIntegrationLateJoinAligned wires a REAL Origin and a late-joining REAL
// Receiver, asserting the joiner's first pushed sample is chunk-aligned to the
// live timeline rather than rewound to 0.
func TestIntegrationLateJoinAligned(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	dst := conn.LocalAddr().(*net.UDPAddr)

	rc, _ := codec.New(codec.PCM)
	r := sinknet.NewCaptureReceiver(rc, fec.NewNone(), sinknet.AllowingSet("127.0.0.1"), canonCfg())

	rctx, rcancel := context.WithCancel(context.Background())
	rdone := make(chan struct{})
	go func() { _ = r.Run(rctx, conn); close(rdone) }()
	defer func() { rcancel(); <-rdone }()

	oc, _ := codec.New(codec.PCM)
	src := &rampSource{channels: tChannels, total: 48000}
	o := origin.New(fixedTimeline{sample: 0, playing: true}, oc, fec.NewNone(), src, origin.Config{
		Rate: 48000, Channels: tChannels, FramesPerChunk: tFrames, StreamGen: 5,
	})
	octx, ocancel := context.WithCancel(context.Background())
	odone := make(chan struct{})
	go func() { _ = o.Run(octx); close(odone) }()
	defer func() { ocancel(); <-odone }()

	time.Sleep(40 * time.Millisecond)
	if err := o.AddListener("late", dst); err != nil {
		t.Fatal(err)
	}

	waitPushes(t, r, 2)
	if r.StreamGen() != 5 {
		t.Errorf("late joiner gen=%d want 5", r.StreamGen())
	}
	idx0, pcm0 := r.PushAtIdx(0)
	if idx0%int64(tFrames) != 0 {
		t.Errorf("late joiner first push idx=%d not chunk-aligned", idx0)
	}
	if len(pcm0) != tFrames*tChannels {
		t.Errorf("late joiner first chunk len=%d want %d", len(pcm0), tFrames*tChannels)
	}
}

func waitPushes(t *testing.T, r *sinknet.CaptureReceiver, n int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for r.Pushes() < n {
		select {
		case <-deadline:
			t.Fatalf("timed out: %d pushes, want %d", r.Pushes(), n)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}
