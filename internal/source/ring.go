package source

import "ensemble/internal/stream"

// ringBuffer is a fixed-capacity ring of recently released frames — the source
// for burst priming (§8.2/D24). Sized to hold max(2*bufferMs, 1000) ms of 20 ms
// frames; oldest frames overwrite. Guarded by Server.mu.
type ringBuffer struct {
	frames  []ringSlot
	head    int   // index of the next write
	count   int   // valid slots (<= cap)
	bufMs   int   // current bufferMs (prime deadline = pts + bufMs)
	lastPTS int64 // pts of the newest pushed frame (live edge)
	hasLast bool
}

type ringSlot struct {
	seq     uint64
	pts     int64
	payload []byte // owned copy
}

// ringCapacityFrames computes the slot count for bufferMs of 20 ms frames,
// floored at 1000 ms (D24): max(2*bufferMs, 1000) / 20.
func ringCapacityFrames(bufferMs int) int {
	ms := 2 * bufferMs
	if ms < 1000 {
		ms = 1000
	}
	n := ms / stream.FrameDuration
	if n < 1 {
		n = 1
	}
	return n
}

// resize (re)allocates the ring to hold max(2*bufferMs, 1000) ms of frames and
// clears it. Called from StartSession.
func (b *ringBuffer) resize(bufferMs int) {
	b.bufMs = bufferMs
	b.frames = make([]ringSlot, ringCapacityFrames(bufferMs))
	b.head = 0
	b.count = 0
	b.lastPTS = 0
	b.hasLast = false
}

// push appends a released frame (copying payload), overwriting the oldest slot
// when full.
func (b *ringBuffer) push(seq uint64, pts int64, payload []byte) {
	if len(b.frames) == 0 {
		return
	}
	cp := make([]byte, len(payload))
	copy(cp, payload)
	b.frames[b.head] = ringSlot{seq: seq, pts: pts, payload: cp}
	b.head = (b.head + 1) % len(b.frames)
	if b.count < len(b.frames) {
		b.count++
	}
	b.lastPTS = pts
	b.hasLast = true
}

// prime returns the frames to burst to a (re)joining subscriber, oldest->newest:
// every ring frame whose playout deadline (pts+bufferMs) is still in the future
// relative to the live edge — i.e. only the most recent bufferMs of audio
// (D24). Clock-free: the cutoff is lastPTS - bufferMs.
func (b *ringBuffer) prime() []ringSlot {
	if b.count == 0 || !b.hasLast {
		return nil
	}
	cutoff := b.lastPTS - int64(b.bufMs)*1_000_000
	out := make([]ringSlot, 0, b.count)
	// Walk oldest -> newest.
	start := (b.head - b.count + len(b.frames)) % len(b.frames)
	for i := 0; i < b.count; i++ {
		s := b.frames[(start+i)%len(b.frames)]
		if s.pts >= cutoff {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// framesAfter returns the ring frames with seq > after, oldest->newest — the
// prime catch-up delta: frames released while a burst was in flight. Slot
// payloads are immutable once pushed, so callers may alias them.
func (b *ringBuffer) framesAfter(after uint64) []ringSlot {
	if b.count == 0 {
		return nil
	}
	var out []ringSlot
	start := (b.head - b.count + len(b.frames)) % len(b.frames)
	for i := 0; i < b.count; i++ {
		s := b.frames[(start+i)%len(b.frames)]
		if s.seq > after {
			out = append(out, s)
		}
	}
	return out
}

// clear empties the ring (StopSession).
func (b *ringBuffer) clear() {
	b.head = 0
	b.count = 0
	b.lastPTS = 0
	b.hasLast = false
	for i := range b.frames {
		b.frames[i] = ringSlot{}
	}
}
