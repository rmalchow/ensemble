package group

import (
	"io"
	"sync"
	"sync/atomic"
	"time"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
	"ensemble/internal/stream"
)

// endReason distinguishes a natural pull-source EOF from an explicit stop.
type endReason int

const (
	endEOF  endReason = iota // natural end of a pull-paced source (§8.6)
	endStop                  // explicit Stop / replace / takeover / master loss
)

// session runs one playback of one URI on the master. Created by Play, owned by
// the Engine (e.sess). Self-contained: one goroutine + a 20 ms ticker.
//
// gen is read/written under the engine mutex (live SetSettings bumps it); the
// run goroutine loads it atomically each tick so a mid-stream gen change applies
// without a data race.
type session struct {
	gen     atomic.Uint32 // current session generation (§8.4); SetSettings may bump
	uri     string
	groupID id.ID
	codec   string
	live    bool // pacing class (§6.1): false = pull (EOF ends), true = live (Stop only)

	src MediaSource
	srv SourceServer
	enc OpusEncoder // nil for pcm

	startMaster int64 // sessionStart in master-clock ns (LocalToMaster(now)+leadMs)
	startedUnix int64 // wall-clock unix for positionSec
	transport   string
	bufferMs    int
	leadMs      int

	stop chan struct{} // closed by halt()
	done chan struct{} // closed when run() exits
	once sync.Once     // guards stop close

	onEnd func(s *session, reason endReason) // engine callback (clears status on EOF)
	now   func() time.Time
}

// run is the release loop (§8.2). One frame per 20 ms tick:
//   - read a frame via src.ReadFrame(buf). A live source never EOFs and self-
//     silences on underflow (D30), so nil always means "publish this frame".
//     A pull source returns io.EOF after its last frame → enter drain.
//   - opus: encode the PCM frame; the payload published is the opus packet.
//   - stamp pts = startMaster + seq*FrameNanos and ReleaseFrame; the server
//     stamps seq itself, so seq here is just the local frame index.
//
// On pull EOF the loop does NOT cut instantly: it keeps ticking (no more reads/
// publishes) until lead+bufferMs has elapsed so the already-released tail plays
// out, then ends EOF (§8.6). Exits on stop (endStop) or drain-complete (endEOF).
func (s *session) run() {
	defer close(s.done)

	tick := time.NewTicker(time.Duration(stream.FrameDuration) * time.Millisecond)
	defer tick.Stop()

	buf := make([]byte, stream.FrameBytes)
	var idx int64
	draining := false
	var drainUntil time.Time

	for {
		select {
		case <-s.stop:
			s.onEnd(s, endStop)
			return
		case <-tick.C:
		}

		if draining {
			if !s.now().Before(drainUntil) {
				s.onEnd(s, endEOF)
				return
			}
			continue
		}

		err := s.src.ReadFrame(buf)
		if err == io.EOF {
			// Pull source ended. Drain the already-released lead+buffer tail.
			draining = true
			tail := time.Duration(s.leadMs+s.bufferMs) * time.Millisecond
			drainUntil = s.now().Add(tail)
			continue
		}
		if err != nil {
			s.onEnd(s, endStop)
			return
		}

		payload := buf
		if s.enc != nil {
			pkt, eerr := s.enc.Encode(buf)
			if eerr != nil {
				s.onEnd(s, endStop)
				return
			}
			// Copy: Encode aliases the encoder's reused buffer (D33).
			payload = append([]byte(nil), pkt...)
		}

		pts := s.startMaster + idx*stream.FrameNanos
		s.srv.ReleaseFrame(pts, payload)
		idx++
	}
}

// halt closes stop once and waits for done. MUST be called without e.mu held
// (the run goroutine's onEnd re-takes e.mu).
func (s *session) halt() {
	s.once.Do(func() { close(s.stop) })
	<-s.done
}

// closeSrc releases the media source + encoder after the run goroutine exits.
func (s *session) closeSrc() {
	if s.src != nil {
		_ = s.src.Close()
	}
	if s.enc != nil {
		_ = s.enc.Close()
	}
}

// playbackRecord assembles the replicated playback record for this session.
func (s *session) playbackRecord(now time.Time, st contracts.SourceStats) contracts.Playback {
	pos := float64(now.Unix() - s.startedUnix)
	if pos < 0 {
		pos = 0
	}
	return contracts.Playback{
		State:       "playing",
		URI:         s.uri,
		StartedUnix: s.startedUnix,
		PositionSec: pos,
		Codec:       s.codec,
		Transport:   s.transport,
		Source:      st,
	}
}
