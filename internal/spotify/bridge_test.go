package spotify

import (
	"log/slog"
	"testing"
	"time"
)

// A metadata event populates Latest() with the full track info (title/artist/
// album/cover/duration) and fires OnMetadata.
func TestHandleEventMetadata(t *testing.T) {
	fired := 0
	b := &Bridge{log: slog.Default()}
	b.cfg.OnMetadata = func() { fired++ }

	if _, ok := b.Latest(); ok {
		t.Fatal("Latest() should be empty before any metadata event")
	}

	ev := `{"type":"metadata","data":{
		"uri":"spotify:track:x","name":"Song","artist_names":["A","B"],
		"album_name":"Album","album_cover_url":"http://art","duration":204000}}`
	b.handleEvent([]byte(ev))

	md, ok := b.Latest()
	if !ok {
		t.Fatal("Latest() not set after metadata event")
	}
	if md.Title != "Song" || md.Artist != "A" || md.Album != "Album" ||
		md.ArtURL != "http://art" || md.DurationSec != 204 {
		t.Fatalf("metadata mismatch: %+v", md)
	}
	if fired != 1 {
		t.Fatalf("OnMetadata fired %d times, want 1", fired)
	}
}

// go-librespot 0.7.3 carries the track info on the "playing" event — capture it.
func TestHandleEventPlayingCarriesMetadata(t *testing.T) {
	fired := 0
	b := &Bridge{log: slog.Default()}
	b.cfg.OnMetadata = func() { fired++ }
	b.handleEvent([]byte(`{"type":"playing","data":{"uri":"spotify:track:x","name":"Song","artist_names":["A"]}}`))
	md, ok := b.Latest()
	if !ok || md.Title != "Song" || md.Artist != "A" {
		t.Fatalf("playing event with a name should set metadata: %+v ok=%v", md, ok)
	}
	if fired != 1 {
		t.Fatalf("OnMetadata fired %d times, want 1", fired)
	}
}

// A name-less event (e.g. a bare "playing") drives its callback but sets no metadata.
func TestHandleEventPlayingNoMetadata(t *testing.T) {
	played := 0
	b := &Bridge{log: slog.Default()}
	b.cfg.OnPlay = func() { played++ }
	b.handleEvent([]byte(`{"type":"playing","data":{"uri":"spotify:track:x"}}`))
	if played != 1 {
		t.Fatalf("OnPlay fired %d times, want 1", played)
	}
	if _, ok := b.Latest(); ok {
		t.Fatal("a playing event should not set metadata")
	}
}

// Position() is unknown until an event carries one — the session then falls back
// to wall-clock. A bare playing event (no position field) must not anchor at 0.
func TestPositionUnknownUntilCarried(t *testing.T) {
	b := &Bridge{log: slog.Default()}
	if _, ok := b.Position(); ok {
		t.Fatal("Position() should be unknown before any position-carrying event")
	}
	b.handleEvent([]byte(`{"type":"playing","data":{"uri":"spotify:track:x"}}`))
	if _, ok := b.Position(); ok {
		t.Fatal("a position-less playing event must not anchor Position()")
	}
}

// A metadata event anchors position and, while playing, it free-runs at 1x.
func TestPositionFreeRunsWhilePlaying(t *testing.T) {
	b := &Bridge{log: slog.Default()}
	// metadata carries position=0; mark playing so it advances.
	b.handleEvent([]byte(`{"type":"metadata","data":{"name":"S","position":0,"duration":200000}}`))
	b.handleEvent([]byte(`{"type":"playing","data":{"uri":"spotify:track:x"}}`))

	// Rewind the anchor 2s into the past and read: should be ~2s.
	b.mu.Lock()
	b.posAt = b.posAt.Add(-2 * time.Second)
	b.mu.Unlock()
	sec, ok := b.Position()
	if !ok || sec < 1.9 || sec > 2.2 {
		t.Fatalf("free-running position = %.3f ok=%v, want ~2.0", sec, ok)
	}
}

// A seek event re-anchors to the reported position — this is the fix: a phone-side
// seek to the start drops the position back instead of continuing to climb.
func TestPositionSeekReanchors(t *testing.T) {
	b := &Bridge{log: slog.Default()}
	b.handleEvent([]byte(`{"type":"metadata","data":{"name":"S","position":150000,"duration":200000}}`))
	b.handleEvent([]byte(`{"type":"playing","data":{"uri":"spotify:track:x"}}`))
	if sec, ok := b.Position(); !ok || sec < 149 || sec > 152 {
		t.Fatalf("pre-seek position = %.3f, want ~150", sec)
	}
	// Seek back to the start.
	b.handleEvent([]byte(`{"type":"seek","data":{"uri":"spotify:track:x","position":0,"duration":200000}}`))
	if sec, ok := b.Position(); !ok || sec > 1 {
		t.Fatalf("post-seek position = %.3f ok=%v, want ~0", sec, ok)
	}
}

// While paused the anchored position is frozen — it does not advance with time.
func TestPositionFrozenWhilePaused(t *testing.T) {
	b := &Bridge{log: slog.Default()}
	b.handleEvent([]byte(`{"type":"metadata","data":{"name":"S","position":30000,"duration":200000}}`))
	b.handleEvent([]byte(`{"type":"playing","data":{"uri":"spotify:track:x"}}`))
	b.handleEvent([]byte(`{"type":"paused","data":{"uri":"spotify:track:x"}}`))

	// Even with the anchor well in the past, a paused clock holds its value.
	b.mu.Lock()
	b.posAt = b.posAt.Add(-10 * time.Second)
	b.mu.Unlock()
	sec, ok := b.Position()
	if !ok || sec < 29 || sec > 31 {
		t.Fatalf("paused position = %.3f ok=%v, want frozen ~30", sec, ok)
	}
}
