package group

import (
	"io"
	"sync"

	"ensemble/internal/contracts"
	"ensemble/internal/stream"
)

// queueSource plays an ordered list of media URIs as ONE continuous session
// (gapless): it opens the current item, and when that inner source hits io.EOF it
// transparently closes it and opens the next, returning frames the whole way —
// so the session generation never changes, members never re-subscribe, and the
// source server streams without a break. The outer io.EOF (which ends the
// session, §8.6) is returned only when the queue is exhausted.
//
// It is the source for FILE-source playback (the queue is a file-source concept);
// http/input/spotify still play as single, non-queued sources.
//
// Concurrency: ReadFrame runs on the engine's release goroutine; the mutators
// (Append/PlayNow/Next/RemoveUpcoming) and Now() are called from API + heartbeat
// goroutines. All state is guarded by mu. onChange (the track-boundary re-publish
// hook) is invoked WITHOUT mu held so the engine's re-publish — which calls back
// into Now() — cannot self-deadlock.
type queueSource struct {
	mu    sync.Mutex
	items []contracts.QueueItem // full playlist (current + upcoming)
	cur   int                   // index of the current item; -1 before the first open
	inner MediaSource           // open source for items[cur], or nil when (re)open is pending

	pendingAdvance bool // Next()/skip: drop current, move to cur+1
	pendingReopen  bool // PlayNow(): current slot replaced, reopen it in place

	framesInCur int64 // frames emitted from the current item (per-track position)
	rev         int64 // monotonic change counter (bumped on any queue mutation / track advance)

	open     func(uri string) (MediaSource, error)
	onChange func() // engine hook: re-publish the replicated record on a track change
}

// newQueueSource builds a queue source seeded with items (at least one). open is
// the per-item source factory; onChange is fired (off-lock) whenever the playing
// track changes so the engine can re-publish promptly.
func newQueueSource(items []contracts.QueueItem, open func(string) (MediaSource, error), onChange func()) *queueSource {
	return &queueSource{
		items:    cloneItems(items),
		cur:      -1, // first ReadFrame advances to 0
		open:     open,
		onChange: onChange,
	}
}

// prime eagerly opens the first item so a bad first file surfaces as an error at
// Play time (rather than silently EOF-ing). The opened inner is reused by the
// first ReadFrame. items must be non-empty.
func (q *queueSource) prime() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.cur = 0
	src, err := q.open(q.items[0].URI)
	if err != nil {
		q.cur = -1
		return err
	}
	q.inner = src
	q.framesInCur = 0
	return nil
}

// ReadFrame fills dst with the next canonical frame, rolling from one queued item
// to the next across inner EOFs. Returns io.EOF only when the queue is exhausted.
func (q *queueSource) ReadFrame(dst []byte) error {
	q.mu.Lock()
	changed := false
	for {
		if q.pendingAdvance {
			q.pendingAdvance = false
			q.closeInnerLocked()
			q.cur++
		}
		if q.pendingReopen {
			q.pendingReopen = false
			q.closeInnerLocked()
		}
		if q.inner == nil {
			if !q.openCurrentLocked() {
				q.mu.Unlock()
				return io.EOF // queue exhausted
			}
			changed = true
			q.rev++ // track advanced: the upcoming list shifted
		}

		err := q.inner.ReadFrame(dst)
		if err == io.EOF {
			q.closeInnerLocked()
			q.cur++
			continue // open the next item
		}
		if err != nil {
			q.mu.Unlock()
			return err
		}
		q.framesInCur++
		q.mu.Unlock()
		if changed && q.onChange != nil {
			q.onChange()
		}
		return nil
	}
}

// openCurrentLocked opens items[cur], skipping any item that fails to open (a
// deleted/corrupt file is dropped rather than ending the whole queue). Returns
// false when cur runs past the end (exhausted). Caller holds mu.
func (q *queueSource) openCurrentLocked() bool {
	for q.cur < len(q.items) {
		if q.cur < 0 {
			q.cur = 0
		}
		src, err := q.open(q.items[q.cur].URI)
		if err == nil {
			q.inner = src
			q.framesInCur = 0
			return true
		}
		q.cur++ // unplayable item: skip to the next
	}
	return false
}

func (q *queueSource) closeInnerLocked() {
	if q.inner != nil {
		_ = q.inner.Close()
		q.inner = nil
	}
}

// Live reports pull pacing (a file queue is EOF-terminated, never live).
func (q *queueSource) Live() bool { return false }

// Close releases the open inner source.
func (q *queueSource) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closeInnerLocked()
	return nil
}

// Now reports the currently-playing item (uri + metadata, preferring the live
// inner source's tags), the per-track position in seconds, and a copy of the
// UPCOMING items. Satisfies QueueProgress (consumed by session.playbackRecord).
func (q *queueSource) Now() (uri string, meta *contracts.TrackMetadata, positionSec float64, upcoming []contracts.QueueItem) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.cur >= 0 && q.cur < len(q.items) {
		it := q.items[q.cur]
		uri = it.URI
		meta = it.Metadata
		if ms, ok := q.inner.(MetadataSource); ok {
			if md, has := ms.Metadata(); has {
				m := md
				meta = &m
			}
		}
	}
	positionSec = float64(q.framesInCur*stream.FrameNanos) / 1e9
	from := q.cur + 1
	if from < 0 {
		from = 0
	}
	if from < len(q.items) {
		upcoming = cloneItems(q.items[from:])
	}
	return
}

// Metadata satisfies MetadataSource with the current track's tags.
func (q *queueSource) Metadata() (contracts.TrackMetadata, bool) {
	_, meta, _, _ := q.Now()
	if meta == nil {
		return contracts.TrackMetadata{}, false
	}
	return *meta, true
}

// Append adds items to the END of the queue (the [+] button).
func (q *queueSource) Append(items []contracts.QueueItem) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, cloneItems(items)...)
	q.rev++
}

// PlayNow replaces the currently-playing track with item (the old one is dropped,
// NOT requeued) and reopens in place — a gapless front-switch. Upcoming items are
// preserved. When nothing is currently playing (cur out of range) it appends and
// skips to the new item.
func (q *queueSource) PlayNow(item contracts.QueueItem) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.cur < 0 || q.cur >= len(q.items) {
		q.items = append(q.items, item)
		q.pendingAdvance = true
		q.rev++
		return
	}
	q.items[q.cur] = item
	q.pendingReopen = true
	q.rev++
}

// PlayUpcoming promotes the upcoming item at index (0 == the next track) to play
// now: the current track is dropped (NOT requeued), the promoted item takes the
// current slot and reopens in place (a gapless front-switch), and it is removed
// from its upcoming position. uriGuard, when non-empty, must match the promoted
// item's URI — a guard against index races with concurrent snapshot updates.
// No-op on a stale index/guard or when nothing is currently playing.
func (q *queueSource) PlayUpcoming(index int, uriGuard string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.cur < 0 || q.cur >= len(q.items) {
		return
	}
	abs := q.cur + 1 + index
	if abs <= q.cur || abs >= len(q.items) {
		return
	}
	if uriGuard != "" && q.items[abs].URI != uriGuard {
		return
	}
	item := q.items[abs]
	// abs > cur, so removing it leaves cur's index intact.
	q.items = append(q.items[:abs], q.items[abs+1:]...)
	q.items[q.cur] = item
	q.pendingReopen = true
	q.rev++
}

// Next skips to the next upcoming item (the Next button). When none remain the
// next ReadFrame returns io.EOF and the session ends.
func (q *queueSource) Next() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pendingAdvance = true
	q.rev++
}

// RemoveUpcoming removes the upcoming item at index (0 == the next track). uriGuard,
// when non-empty, must match that item's URI — a guard against index races with
// concurrent snapshot updates. No-op on a stale index/guard.
func (q *queueSource) RemoveUpcoming(index int, uriGuard string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	abs := q.cur + 1 + index
	if abs <= q.cur || abs >= len(q.items) {
		return
	}
	if uriGuard != "" && q.items[abs].URI != uriGuard {
		return
	}
	q.items = append(q.items[:abs], q.items[abs+1:]...)
	q.rev++
}

// QueueRev returns the monotonic change counter (QueueProgress). The UI re-pulls
// the queue contents whenever this moves; the items are never gossiped.
func (q *queueSource) QueueRev() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.rev
}

// hasUpcoming reports whether any items remain after the current one.
func (q *queueSource) hasUpcoming() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.cur+1 < len(q.items)
}

// cloneItems returns a deep-ish copy of items (fresh slice; metadata pointers are
// shared, which is safe since records are treated as immutable).
func cloneItems(items []contracts.QueueItem) []contracts.QueueItem {
	if len(items) == 0 {
		return nil
	}
	return append([]contracts.QueueItem(nil), items...)
}
