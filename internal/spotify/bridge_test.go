package spotify

import (
	"log/slog"
	"testing"
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

// A non-metadata event (playing) drives OnPlay but does not disturb metadata.
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
