package clock

// This file is copied near-verbatim from
// gitlab.rand0m.me/ruben/go/media/internal/clock/clock.go (spine D6). It carries
// the packet codec, computeSample, and the Estimator. The on-wire 40-byte 'MPVC'
// packet is the frozen README §6.4 contract (protocolEpoch=1) and is shared with
// no other plane — do NOT alter the layout (see P3.1 §3 / risk R).
//
// All timestamps are nanoseconds from a per-process monotonic epoch (robust to
// wall-clock steps). The offset relates the two processes' monotonic clocks:
// master_mono ≈ follower_mono + offset.

import (
	"encoding/binary"
	"errors"
	"time"
)

const (
	magic   uint32 = 0x4D505643 // "MPVC"
	version uint8  = 1

	kindRequest uint8 = 1
	kindReply   uint8 = 2

	// PacketSize is the fixed wire size of a clock packet (README §6.4).
	PacketSize = 4 + 1 + 1 + 2 + 8 + 8 + 8 + 8 // = 40
)

var procEpoch = time.Now()

// nowMono returns nanoseconds since this process's monotonic epoch.
func nowMono() int64 { return time.Since(procEpoch).Nanoseconds() }

// NowMono returns nanoseconds since this process's monotonic epoch. It is the
// timebase used on the wire and by the sync controller to pair local
// measurements with the master clock.
func NowMono() int64 { return nowMono() }

// packet is the on-wire clock message (fixed 40 bytes, big-endian).
type packet struct {
	seq  uint64
	t1   int64 // follower send (echoed by master)
	t2   int64 // master receive
	t3   int64 // master send
	kind uint8
}

func (p packet) marshal() []byte {
	b := make([]byte, PacketSize)
	binary.BigEndian.PutUint32(b[0:], magic)
	b[4] = version
	b[5] = p.kind
	// b[6:8] padding
	binary.BigEndian.PutUint64(b[8:], p.seq)
	binary.BigEndian.PutUint64(b[16:], uint64(p.t1))
	binary.BigEndian.PutUint64(b[24:], uint64(p.t2))
	binary.BigEndian.PutUint64(b[32:], uint64(p.t3))
	return b
}

var errBadPacket = errors.New("clock: malformed packet")

func unmarshal(b []byte) (packet, error) {
	if len(b) < PacketSize {
		return packet{}, errBadPacket
	}
	if binary.BigEndian.Uint32(b[0:]) != magic || b[4] != version {
		return packet{}, errBadPacket
	}
	return packet{
		kind: b[5],
		seq:  binary.BigEndian.Uint64(b[8:]),
		t1:   int64(binary.BigEndian.Uint64(b[16:])),
		t2:   int64(binary.BigEndian.Uint64(b[24:])),
		t3:   int64(binary.BigEndian.Uint64(b[32:])),
	}, nil
}

// Sample is one completed four-timestamp exchange.
type Sample struct {
	Offset time.Duration // master_mono - follower_mono
	Delay  time.Duration // round-trip delay (>= 0)
}

// computeSample derives offset and delay from the four timestamps (A.1).
//
//	offset = ((t2 - t1) + (t3 - t4)) / 2
//	delay  = (t4 - t1) - (t3 - t2)
func computeSample(t1, t2, t3, t4 int64) Sample {
	offset := ((t2 - t1) + (t3 - t4)) / 2
	delay := (t4 - t1) - (t3 - t2)
	if delay < 0 {
		delay = 0
	}
	return Sample{Offset: time.Duration(offset), Delay: time.Duration(delay)}
}

// Estimator filters a stream of samples into a smoothed clock offset. It keeps a
// sliding window, selects the minimum-delay sample (NTP minimum filter — high
// delay means queuing jitter, i.e. a low-quality sample), and EWMAs the offset of
// those best samples for stability.
type Estimator struct {
	window []Sample
	size   int
	alpha  float64

	offset  float64 // EWMA of best-sample offsets, nanoseconds
	have    bool
	samples int
}

// NewEstimator returns an Estimator with the given window size and EWMA alpha
// (0..1; smaller = smoother/slower). Out-of-range args fall back to the mpvsync
// defaults (window 8, alpha 0.15); ensemble drives A.12's (8,0.15) wired /
// (16,0.10) WiFi explicitly via Follower's WithEstimator.
func NewEstimator(window int, alpha float64) *Estimator {
	if window < 1 {
		window = 8
	}
	if alpha <= 0 || alpha > 1 {
		alpha = 0.15
	}
	return &Estimator{size: window, alpha: alpha}
}

// Add incorporates a new sample and returns the updated smoothed offset.
func (e *Estimator) Add(s Sample) time.Duration {
	e.samples++
	e.window = append(e.window, s)
	if len(e.window) > e.size {
		e.window = e.window[len(e.window)-e.size:]
	}

	// Minimum-delay filter: the lowest-delay sample in the window is the most
	// trustworthy estimate of the true offset.
	best := e.window[0]
	for _, w := range e.window[1:] {
		if w.Delay < best.Delay {
			best = w
		}
	}

	bo := float64(best.Offset)
	if !e.have {
		e.offset = bo
		e.have = true
	} else {
		// EWMA slews the applied offset toward the best estimate (never steps).
		e.offset += e.alpha * (bo - e.offset)
	}
	return time.Duration(int64(e.offset))
}

// Offset returns the current smoothed offset (master_mono - follower_mono) and
// whether at least one sample has been processed.
func (e *Estimator) Offset() (time.Duration, bool) {
	return time.Duration(int64(e.offset)), e.have
}

// MinDelay returns the smallest round-trip delay currently in the window, a
// proxy for sync quality. ok is false until a sample has been seen.
func (e *Estimator) MinDelay() (time.Duration, bool) {
	if len(e.window) == 0 {
		return 0, false
	}
	min := e.window[0].Delay
	for _, w := range e.window[1:] {
		if w.Delay < min {
			min = w.Delay
		}
	}
	return min, true
}

// Samples returns the total number of samples processed.
func (e *Estimator) Samples() int { return e.samples }
