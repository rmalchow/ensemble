package source

// loopReader wraps a frameSource (optionally already wrapped by a resampler) and
// turns its finite stream into an infinite, gapless one. On EOF it repositions to
// the start via seekStart and continues, so Read NEVER returns io.EOF while
// looping (05 §5.3). It fills the caller's whole dst across the loop boundary —
// leftover frames straddling the seam are carried by continuing to fill the same
// dst slice — so the chunker upstream never sees a runt/short frame (05 §5.3:
// "partial final chunks are padded with the loop's leading frames").
//
// For a non-seekable input (HTTP body) seekStart returns errSeekUnsupported; the
// loop then re-opens the input via reopen() to restart the stream.

import (
	"errors"
	"fmt"
	"io"
)

// errSeekUnsupported is returned by a frameSource.seekStart that cannot reposition
// in place (non-seekable HTTP body); loopReader falls back to reopen().
var errSeekUnsupported = errors.New("source: seek to start unsupported")

// loopReader is the public Reader: it loops a frameSource at the canonical rate.
type loopReader struct {
	fs     frameSource
	reopen func() (frameSource, error) // rebuild the source (HTTP re-request); may be nil
	rateHz int
	chans  int
	closed bool
}

// newLoopReader assembles the looping Reader over fs (native==canonical or already
// resampled). reopen restarts a non-seekable source; pass nil for seekable inputs.
func newLoopReader(fs frameSource, rateHz, chans int, reopen func() (frameSource, error)) *loopReader {
	return &loopReader{fs: fs, reopen: reopen, rateHz: rateHz, chans: chans}
}

func (l *loopReader) Rate() int     { return l.rateHz }
func (l *loopReader) Channels() int { return l.chans }

// Read fills dst with whole interleaved frames, looping at EOF. It returns n (a
// multiple of Channels()) and only a non-nil, non-EOF error on a hard decode/IO
// failure or when the stream is empty and cannot be restarted.
func (l *loopReader) Read(dst []float32) (frames int, err error) {
	if l.closed {
		return 0, errors.New("source: read after close")
	}
	want := len(dst) - len(dst)%l.chans
	// emptyLoops counts consecutive restarts that produced no frames, guarding a
	// zero-length / silent media file from spinning forever.
	emptyLoops := 0
	for frames < want {
		n, rerr := l.fs.read(dst[frames:want])
		frames += n
		if rerr == nil {
			emptyLoops = 0
			continue
		}
		if rerr != io.EOF {
			return frames, rerr
		}
		// Underlying source hit true EOF.
		if n == 0 {
			emptyLoops++
			if emptyLoops > 1 {
				if frames > 0 {
					return frames, nil
				}
				return 0, errors.New("source: empty media, cannot loop")
			}
		} else {
			emptyLoops = 0
		}
		// Rewind and keep filling dst so the boundary is gapless and the caller
		// still gets whole frames (no short slice at the loop seam).
		if err := l.restart(); err != nil {
			if frames > 0 {
				return frames, nil
			}
			return 0, err
		}
	}
	return frames, nil
}

// restart repositions the source to frame 0, re-opening it if seek is unsupported.
func (l *loopReader) restart() error {
	err := l.fs.seekStart()
	if err == nil {
		return nil
	}
	if !errors.Is(err, errSeekUnsupported) {
		return err
	}
	if l.reopen == nil {
		return fmt.Errorf("source: cannot loop non-seekable stream: %w", err)
	}
	fresh, oerr := l.reopen()
	if oerr != nil {
		return fmt.Errorf("source: loop re-open: %w", oerr)
	}
	_ = l.fs.close()
	l.fs = fresh
	return nil
}

func (l *loopReader) Close() error {
	if l.closed {
		return nil
	}
	l.closed = true
	return l.fs.close()
}
