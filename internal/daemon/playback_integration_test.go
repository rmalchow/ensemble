package daemon

// playback_integration_test.go is the REALTIME two-node milestone gate: an
// in-process loopback session proving a second node actually HEARS the first,
// in sync — genesis A, adopt B (the control-plane e2e path), real gossip
// membership + per-group election, A plays the 1 kHz sine fixture, and B's
// injected capturing sink receives the decoded, render-paced tone (allowlisted
// UDP audio + clock planes end-to-end). Then A leaves and B re-elects itself
// master (failover, A.5 / doc 01 §6d).

import (
	"crypto/tls"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	audio "gitlab.rand0m.me/ruben/go/ensemble/internal/audio/sink"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/config"
)

// captureSink is an injected AudioSink that records every rendered sample and
// paces Write at real time (the backpressure a real device provides — without
// it the renderer would drain the jitter ring far faster than the origin fills
// it and spin on underruns).
type captureSink struct {
	mu       sync.Mutex
	rate     int
	channels int
	samples  []float32
}

func (c *captureSink) Start(rate, channels int) error {
	c.mu.Lock()
	c.rate, c.channels = rate, channels
	c.mu.Unlock()
	return nil
}

func (c *captureSink) Write(frames []float32) (int, error) {
	c.mu.Lock()
	c.samples = append(c.samples, frames...)
	rate, channels := c.rate, c.channels
	c.mu.Unlock()
	if rate > 0 && channels > 0 {
		time.Sleep(time.Duration(float64(len(frames)) / float64(rate*channels) * float64(time.Second)))
	}
	return len(frames), nil
}

func (c *captureSink) Delay() (int, bool) { return 0, false }
func (c *captureSink) Close() error       { return nil }

func (c *captureSink) snapshot() []float32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]float32, len(c.samples))
	copy(out, c.samples)
	return out
}

// loudSegment returns the longest contiguous run of 10 ms windows whose left-
// channel RMS exceeds thresh, as a flat left-channel sample slice. (A pure
// peak-threshold run cannot work for a tone: a 1 kHz sine at 48 kHz dips below
// any amplitude threshold every half-period.)
func loudSegment(samples []float32, channels int, thresh float64) []float32 {
	const winFrames = 480 // 10 ms @ 48k
	left := make([]float32, 0, len(samples)/channels)
	for i := 0; i+channels <= len(samples); i += channels {
		left = append(left, samples[i])
	}
	nwin := len(left) / winFrames
	loud := make([]bool, nwin)
	for w := 0; w < nwin; w++ {
		var sum float64
		for _, v := range left[w*winFrames : (w+1)*winFrames] {
			sum += float64(v) * float64(v)
		}
		loud[w] = math.Sqrt(sum/winFrames) > thresh
	}
	best, bestLen, start, n := 0, 0, 0, 0
	for w := 0; w < nwin; w++ {
		if loud[w] {
			if n == 0 {
				start = w
			}
			n++
			if n > bestLen {
				best, bestLen = start, n
			}
		} else {
			n = 0
		}
	}
	return left[best*winFrames : (best+bestLen)*winFrames]
}

// estimateToneHz estimates a mono tone's frequency by zero-crossing count.
func estimateToneHz(left []float32, rate int) float64 {
	if len(left) < 4 {
		return 0
	}
	crossings := 0
	prev := left[0]
	for _, v := range left[1:] {
		if (prev < 0 && v >= 0) || (prev >= 0 && v < 0) {
			crossings++
		}
		prev = v
	}
	return float64(crossings) / 2 / (float64(len(left)-1) / float64(rate))
}

// waitFor polls cond until it is true or the deadline passes.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// freeUDPPort reserves a UDP port by binding :0 and releasing it.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", ":0")
	if err != nil {
		t.Fatalf("free udp port: %v", err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()
	return port
}

// freeGossipPort reserves a port usable for memberlist (TCP and UDP on the same
// number).
func freeGossipPort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 64; i++ {
		ln, err := net.Listen("tcp", ":0")
		if err != nil {
			continue
		}
		port := ln.Addr().(*net.TCPAddr).Port
		pc, uerr := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
		ln.Close()
		if uerr != nil {
			continue
		}
		pc.Close()
		return port
	}
	t.Fatal("no free gossip port")
	return 0
}

func TestTwoNodeSyncedPlayback(t *testing.T) {
	if testing.Short() {
		t.Skip("two-node realtime loopback session (slow)")
	}
	dirA, dirB := t.TempDir(), t.TempDir()
	pathsA, err := config.OpenDataDir(dirA)
	if err != nil {
		t.Fatalf("open data dir A: %v", err)
	}
	pathsB, err := config.OpenDataDir(dirB)
	if err != nil {
		t.Fatalf("open data dir B: %v", err)
	}

	// The media: the in-repo 1 kHz sine fixture, copied into A's data/ folder
	// (master-side decode reads from data/, 08 §F.2).
	tone, err := os.ReadFile(filepath.Join("..", "stream", "source", "testdata", "sine_48000.mp3"))
	if err != nil {
		t.Fatalf("read sine fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pathsA.Data, "sine.mp3"), tone, 0o644); err != nil {
		t.Fatalf("write sine.mp3: %v", err)
	}

	const (
		idA = "00000000000000000000000000000000"
		idB = "11111111111111111111111111111111"
	)
	sinkA, sinkB := &captureSink{}, &captureSink{}
	gossipA, gossipB := freeGossipPort(t), freeGossipPort(t)

	// === Node A: genesis founder. ===
	nodeA := New(Options{
		Paths: pathsA, NodeID: idA, Name: "A",
		ClockPort: freeUDPPort(t), AudioPort: freeUDPPort(t), BindPort: gossipA,
		OpenSink: func() (audio.AudioSink, error) { return sinkA, nil },
	})
	t.Cleanup(nodeA.deactivate)
	baseA, clientA, stopA := serveNode(t, nodeA)
	defer stopA()

	resp, err := clientA.Post(baseA+"/api/v1/setup", "application/json",
		strings.NewReader(`{"clusterName":"home","adminPassword":"correct horse battery staple","nodeName":"A"}`))
	if err != nil {
		t.Fatalf("POST /setup: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /setup status = %d, want 200", resp.StatusCode)
	}

	// === Node B: adopted over the A.9 bootstrap handshake, gossip-seeded at A. ===
	nodeB := New(Options{
		Paths: pathsB, NodeID: idB, Name: "B",
		ClockPort: freeUDPPort(t), AudioPort: freeUDPPort(t), BindPort: gossipB,
		Seeds:    []string{fmt.Sprintf("127.0.0.1:%d", gossipA)},
		OpenSink: func() (audio.AudioSink, error) { return sinkB, nil },
	})
	t.Cleanup(nodeB.deactivate)
	baseB, _, stopB := serveNode(t, nodeB)
	defer stopB()

	fp := bootstrapFingerprint(t, baseB)
	bClient := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	if err := nodeA.adoptUsing(baseB, bClient, "127.0.0.1", fp, "0000", idB, "B", "", false); err != nil {
		t.Fatalf("adopt B: %v", err)
	}

	// === Convergence: gossip replicates the doc; the election resolves A master
	// (lowest stable id, A.5), B follower. ===
	waitFor(t, 30*time.Second, "B's ConfigDoc to converge (default group, 2 members)", func() bool {
		g := groupRecord(nodeB.store.Get(), "default")
		return g != nil && len(g.MemberNodeIDs) == 2
	})
	waitFor(t, 30*time.Second, "A to settle as master", func() bool {
		st := nodeA.status()
		return st.Role == "master" && st.MasterID == idA
	})
	waitFor(t, 30*time.Second, "B to settle as follower of A", func() bool {
		st := nodeB.status()
		return st.Role == "follower" && st.MasterID == idA
	})

	// === Play the tone on A (one-shot select+play, 08 §F.3). ===
	if _, err := nodeA.play("default", "sine.mp3", true, "", nodeA.store.Get().Version); err != nil {
		t.Fatalf("play on A: %v", err)
	}

	// B's receiver gets chunks for A's LIVE stream generation, marked playing.
	waitFor(t, 30*time.Second, "B's receiver to see playing chunks of A's streamGen", func() bool {
		hsA, hsB := nodeA.hooksFor(), nodeB.hooksFor()
		if hsA == nil || hsB == nil {
			return false
		}
		hsA.mu.Lock()
		var aGen uint64
		if hsA.orig != nil {
			aGen = hsA.orig.o.StreamGen()
		}
		origUp := hsA.orig != nil
		hsA.mu.Unlock()
		hsB.mu.Lock()
		recv := hsB.recv
		hsB.mu.Unlock()
		if !origUp || recv == nil {
			return false
		}
		_, _, bGen, playing, ok := recv.r.LatestChunkMeta()
		return ok && playing && bGen == aGen
	})

	// B RENDERS the tone: its capturing sink receives a sustained loud segment
	// (≥250 ms of contiguous 10 ms RMS-loud windows) whose zero-crossing
	// frequency matches the 1 kHz source within tolerance — i.e. the decoded
	// samples arrive in order at the right rate, not as noise/garbage.
	waitFor(t, 30*time.Second, "B's sink to receive a sustained tone segment", func() bool {
		return len(loudSegment(sinkB.snapshot(), 2, 0.1)) >= 48*250 // 250 ms mono @48k
	})
	seg := loudSegment(sinkB.snapshot(), 2, 0.1)
	freq := estimateToneHz(seg, 48000)
	t.Logf("B captured %d samples; longest tone segment %d frames; tone ≈ %.0f Hz",
		len(sinkB.snapshot()), len(seg), freq)
	if freq < 750 || freq > 1250 {
		t.Errorf("B's rendered tone ≈ %.0f Hz, want ~1000 Hz (sample-misaligned or corrupted stream)", freq)
	}

	// === Failover: A leaves (graceful membership departure + session teardown);
	// B re-elects ITSELF master for the group and flips role (doc 01 §6d). ===
	stopA()
	nodeA.deactivate()
	waitFor(t, 45*time.Second, "B to re-elect itself master after A leaves", func() bool {
		st := nodeB.status()
		return st.Role == "master" && st.MasterID == idB
	})
}

// TestSoloPlaybackRendersAudio pins the single-node case: a lone genesis node
// that plays a file must HEAR it itself (the origin's local PCM tee feeds the
// master's renderer — historically this path rendered silence while only
// followers got audio).
func TestSoloPlaybackRendersAudio(t *testing.T) {
	if testing.Short() {
		t.Skip("realtime loopback session (slow)")
	}
	dir := t.TempDir()
	paths, err := config.OpenDataDir(dir)
	if err != nil {
		t.Fatalf("open data dir: %v", err)
	}
	tone, err := os.ReadFile(filepath.Join("..", "stream", "source", "testdata", "sine_48000.mp3"))
	if err != nil {
		t.Fatalf("read sine fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(paths.Data, "sine.mp3"), tone, 0o644); err != nil {
		t.Fatalf("write sine.mp3: %v", err)
	}

	snk := &captureSink{}
	node := New(Options{
		Paths: paths, NodeID: "00000000000000000000000000000000", Name: "A",
		ClockPort: freeUDPPort(t), AudioPort: freeUDPPort(t),
		OpenSink: func() (audio.AudioSink, error) { return snk, nil },
	})
	t.Cleanup(node.deactivate)
	base, client, stop := serveNode(t, node)
	defer stop()

	resp, err := client.Post(base+"/api/v1/setup", "application/json",
		strings.NewReader(`{"clusterName":"home","adminPassword":"correct horse battery staple","nodeName":"A"}`))
	if err != nil {
		t.Fatalf("POST /setup: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup status = %d", resp.StatusCode)
	}

	waitFor(t, 20*time.Second, "node to settle as solo master", func() bool {
		return node.status().Role == "master"
	})
	if _, err := node.play("default", "sine.mp3", true, "", node.store.Get().Version); err != nil {
		t.Fatalf("play: %v", err)
	}

	waitFor(t, 30*time.Second, "the node's OWN sink to receive a sustained tone", func() bool {
		return len(loudSegment(snk.snapshot(), 2, 0.1)) >= 48*250 // 250 ms mono @48k
	})
	seg := loudSegment(snk.snapshot(), 2, 0.1)
	freq := estimateToneHz(seg, 48000)
	t.Logf("solo captured %d samples; tone segment %d frames; tone ≈ %.0f Hz",
		len(snk.snapshot()), len(seg), freq)
	if freq < 750 || freq > 1250 {
		t.Errorf("solo rendered tone ≈ %.0f Hz, want ~1000 Hz", freq)
	}
}
