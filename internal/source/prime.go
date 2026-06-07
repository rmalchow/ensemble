package source

import (
	"time"

	"ensemble/internal/stream"
)

// primePaceUDP is the inter-frame delay for a UDP prime burst: ~5 ms/frame,
// i.e. ~4× realtime so the burst outruns the live stream without flooding (D24).
const primePaceUDP = 5 * time.Millisecond

// prime bursts the selected ring frames to a (re)joining subscriber, then
// keeps catching up via the ring until it reaches the live edge, and only
// then re-admits the subscriber to live fan-out (clears sub.priming). This
// guarantees the subscriber sees seqs from the oldest primed frame onward
// with no interleaved newer frame — the client reorder window and the sink
// anchor on the burst, never on a racing live frame. The burst runs at >
// realtime (UDP ~4×, TCP back-to-back), so catch-up always terminates.
// Runs in its own goroutine (wg-tracked). No FEC during a prime — the live
// FEC cadence is independent. The Primes stat is counted at initiation
// (onSubscribe), not here.
func (s *Server) prime(sub *subscriber, frames []ringSlot, gen uint32) {
	defer s.wg.Done()
	donePriming := func() {
		s.mu.Lock()
		sub.priming = false
		s.mu.Unlock()
	}
	for {
		var ok bool
		if sub.tr == stream.TransportTCP {
			ok = s.primeTCP(sub, frames, gen)
		} else {
			ok = s.primeUDP(sub, frames, gen)
		}
		if !ok { // server closing or conn dead — give up, re-admit
			donePriming()
			return
		}
		last := frames[len(frames)-1].seq
		s.mu.Lock()
		if !s.active || s.gen != gen {
			// Session ended or superseded mid-prime; the new generation's
			// frames reach the sub via live fan-out (and RECONFIG handling).
			sub.priming = false
			s.mu.Unlock()
			return
		}
		frames = s.ring.framesAfter(last)
		if len(frames) == 0 {
			sub.priming = false // caught up: live from the next ReleaseFrame
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
	}
}

// primeUDP paces TypeAudio datagrams to a UDP subscriber, one per ~5 ms, each
// carrying its original Seq/PTS/Gen. Returns false if the server is closing.
func (s *Server) primeUDP(sub *subscriber, frames []ringSlot, gen uint32) bool {
	t := time.NewTicker(primePaceUDP)
	defer t.Stop()
	buf := make([]byte, 0, stream.HeaderSize+stream.FrameBytes)
	for _, f := range frames {
		select {
		case <-s.done:
			return false
		case <-t.C:
		}
		h := stream.Header{
			Magic:      stream.Magic,
			Type:       stream.TypeAudio,
			Gen:        gen,
			Seq:        f.seq,
			PTS:        f.pts,
			PayloadLen: uint16(len(f.payload)),
		}
		pkt := h.AppendFrame(buf[:0], f.payload)
		s.writeUDP(pkt, sub.addr)
	}
	return true
}

// primeTCP writes the selected frames back-to-back (length-prefixed) on the
// subscriber's conn under its wmu (TCP flow control paces it, D24). Returns
// false if the server is closing or the conn died.
func (s *Server) primeTCP(sub *subscriber, frames []ringSlot, gen uint32) bool {
	sub.wmu.Lock()
	defer sub.wmu.Unlock()
	if sub.conn == nil || sub.dead {
		return false
	}
	buf := make([]byte, 0, stream.HeaderSize+stream.FrameBytes)
	for _, f := range frames {
		select {
		case <-s.done:
			return false
		default:
		}
		h := stream.Header{
			Magic:      stream.Magic,
			Type:       stream.TypeAudio,
			Gen:        gen,
			Seq:        f.seq,
			PTS:        f.pts,
			PayloadLen: uint16(len(f.payload)),
		}
		pkt := h.AppendFrame(buf[:0], f.payload)
		_ = sub.conn.SetWriteDeadline(time.Now().Add(tcpWriteTimeout))
		if err := writeTCPFrame(sub.conn, pkt); err != nil {
			sub.dead = true
			_ = sub.conn.Close()
			return false
		}
	}
	return true
}
