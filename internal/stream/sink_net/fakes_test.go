package sink_net

import (
	"net/netip"
	"sync"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/codec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/fec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/wire"
)

// capturePush records every PushAt so reorder/dedupe/conceal/late-drop can be
// asserted without a real ring. It replaces the ringPusher in tests. Mutex-guarded
// because the loopback test reads it while Receiver.Run pushes from another
// goroutine (production Run is single-goroutine; only this test fake is shared).
type capturePush struct {
	mu  sync.Mutex
	idx []int64
	pcm [][]float32
}

func (c *capturePush) PushAt(sampleIndex int64, pcm []float32) {
	cp := make([]float32, len(pcm))
	copy(cp, pcm)
	c.mu.Lock()
	c.idx = append(c.idx, sampleIndex)
	c.pcm = append(c.pcm, cp)
	c.mu.Unlock()
}

// len returns the number of pushes recorded.
func (c *capturePush) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.idx)
}

// at returns the sampleIndex and pcm of the i-th push (caller ensures i < len).
func (c *capturePush) at(i int) (int64, []float32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.idx[i], c.pcm[i]
}

// allowAll returns true for everything; used via the receiver's real *allowlist.Set
// is awkward (it needs Update), so tests inject a gate through a small shim. The
// real allowlist gate is exercised in TestAllowlistGate with a denying Set.

// buildPacket marshals one source ESND packet at the canonical PCM profile. value
// seeds the PCM so a round-trip is verifiable.
func buildPacket(gen, seq uint64, sampleIndex int64, framesPerChunk, channels int, value float32) []byte {
	c := codec.NewPCM(channels)
	pcm := make([]float32, framesPerChunk*channels)
	for i := range pcm {
		pcm[i] = value
	}
	payload, _ := c.Encode(pcm)
	hdr := wire.Header{
		Flags:       wire.FlagKeyframe,
		CodecID:     wire.CodecPCM,
		FECID:       wire.FECNone,
		StreamGen:   gen,
		Seq:         seq,
		SampleIndex: sampleIndex,
		MasterMono:  int64(seq) * 10_000_000, // 10 ms cadence
		Rate100:     480,
	}
	buf, _ := wire.Marshal(hdr, payload)
	return buf
}

// loopbackAddr is an allowlisted source for tests that bypass the allowlist via the
// receiver's handle (which still calls AllowedAddr).
var loopbackAddr = netip.MustParseAddr("127.0.0.1")

// idCodec / idFEC are not needed: tests use the real codec.PCM + fec.None so the
// contracts stay honest (test plan §7).
var _ = fec.None
