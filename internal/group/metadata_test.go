package group

import (
	"testing"
	"time"

	"ensemble/internal/contracts"
)

// metaSrc is a MediaSource that also exposes the optional metadata channel.
type metaSrc struct {
	md contracts.TrackMetadata
	ok bool
}

func (m metaSrc) ReadFrame([]byte) error                    { return nil }
func (m metaSrc) Live() bool                                { return true }
func (m metaSrc) Close() error                              { return nil }
func (m metaSrc) Metadata() (contracts.TrackMetadata, bool) { return m.md, m.ok }

// plainSrc is a MediaSource with no metadata channel (e.g. line-in).
type plainSrc struct{}

func (plainSrc) ReadFrame([]byte) error { return nil }
func (plainSrc) Live() bool             { return true }
func (plainSrc) Close() error           { return nil }

func TestPlaybackRecordFoldsMetadata(t *testing.T) {
	now := time.Unix(1000, 0)
	md := contracts.TrackMetadata{Title: "Song", Artist: "A", Album: "Alb"}
	s := &session{uri: "spotify:", startedUnix: 990, src: metaSrc{md: md, ok: true}}

	rec := s.playbackRecord(now, contracts.SourceStats{})
	if rec.Metadata == nil {
		t.Fatal("expected metadata folded into playback record")
	}
	if *rec.Metadata != md {
		t.Fatalf("metadata mismatch: %+v", *rec.Metadata)
	}
}

func TestPlaybackRecordNoMetadataWhenSourceHasNone(t *testing.T) {
	now := time.Unix(1000, 0)
	// A source with the method but ok=false, and one without the method at all.
	for name, src := range map[string]MediaSource{
		"absent":   plainSrc{},
		"notReady": metaSrc{ok: false},
	} {
		s := &session{uri: "input:", startedUnix: 990, src: src}
		if rec := s.playbackRecord(now, contracts.SourceStats{}); rec.Metadata != nil {
			t.Fatalf("%s: expected nil metadata, got %+v", name, *rec.Metadata)
		}
	}
}
