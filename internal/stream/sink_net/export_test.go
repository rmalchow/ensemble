package sink_net

import (
	"gitlab.rand0m.me/ruben/go/ensemble/internal/allowlist"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/audio/ring"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/codec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/fec"
)

// export_test.go exposes the minimal receiver internals the EXTERNAL
// (package sink_net_test) origin↔receiver integration tests need, without
// widening the production API. It stays in package sink_net so it can touch the
// unexported push pusher / prime state, but the symbols it exports are usable
// from sink_net_test (which must import origin and therefore cannot live in
// package sink_net — that would cycle via origin→sink_net).

// CaptureReceiver is a Receiver whose pushes are recorded into an in-memory log,
// for the integration tests that drive a real origin into a real receiver.
type CaptureReceiver struct {
	*Receiver
	cap *capturePush
}

// NewCaptureReceiver builds a receiver with a capturing pusher in place of the
// ring adapter and a prime target of one chunk (so a re-anchor is observable
// without waiting the full 300 ms lead). It mirrors the in-package
// newTestReceiver helper but is exported for sink_net_test.
func NewCaptureReceiver(c codec.Codec, f fec.FEC, allow *allowlist.Set, cfg Config) *CaptureReceiver {
	r := New(c, f, ring.NewRing(48000*2), allow, cfg)
	cap := &capturePush{}
	r.push = cap
	r.primeTarget = 1
	return &CaptureReceiver{Receiver: r, cap: cap}
}

// AllowingSet returns an allowlist.Set permitting the given IPs via the live
// member path (the realtime-plane gate, 07 §3.1), for the external integration
// tests.
func AllowingSet(ips ...string) *allowlist.Set { return allowingSet(ips...) }

// Pushes returns the number of chunks pushed so far.
func (cr *CaptureReceiver) Pushes() int { return cr.cap.len() }

// PushAtIdx returns the sampleIndex and pcm of the i-th push.
func (cr *CaptureReceiver) PushAtIdx(i int) (int64, []float32) { return cr.cap.at(i) }
