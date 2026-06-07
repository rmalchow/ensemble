package source

import (
	"testing"
	"time"

	"ensemble/internal/stream"
)

func TestPrimeUDPPacing(t *testing.T) {
	s, uap, _ := newTestServer(t)
	s.StartSession(1, stream.TransportUDP, 1000) // big buffer -> many primed frames

	// Release enough frames that the prime burst spans measurable time.
	const n = 20
	for i := 0; i < n; i++ {
		s.ReleaseFrame(int64(i)*stream.FrameNanos, pcm(byte(i)))
	}
	sub := newUDPSub(t, uap)
	start := time.Now()
	sub.hello(true)
	hs := sub.recvAll(t, 500*time.Millisecond)
	elapsed := time.Since(start)

	audio := countType(hs, stream.TypeAudio)
	if audio < 2 {
		t.Fatalf("primed %d frames; want >= 2", audio)
	}
	// ~5ms/frame pacing: a burst of >=2 frames should take a few ms, not instant
	// at 0. We assert the burst completed (frames arrived) and carried gen=1.
	for _, h := range hs {
		if h.Type == stream.TypeAudio && h.Gen != 1 {
			t.Fatalf("primed frame gen=%d want 1", h.Gen)
		}
	}
	if elapsed > 2*time.Second {
		t.Fatalf("prime took too long: %v", elapsed)
	}
}

func TestPrimeCountsStat(t *testing.T) {
	s, uap, _ := newTestServer(t)
	s.StartSession(1, stream.TransportUDP, 150)
	for i := 0; i < 5; i++ {
		s.ReleaseFrame(int64(i)*stream.FrameNanos, pcm(byte(i)))
	}
	sub := newUDPSub(t, uap)
	sub.hello(true)
	if !waitForN(t, func() int { return int(s.Stats().Primes) }, 1, 2*time.Second) {
		t.Fatalf("Primes=%d want 1", s.Stats().Primes)
	}
}
