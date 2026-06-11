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

// posSrc exposes the optional metadata + authoritative-position channels (Spotify).
type posSrc struct {
	md    contracts.TrackMetadata
	sec   float64
	posOK bool
}

func (p posSrc) ReadFrame([]byte) error                    { return nil }
func (p posSrc) Live() bool                                { return true }
func (p posSrc) Close() error                              { return nil }
func (p posSrc) Metadata() (contracts.TrackMetadata, bool) { return p.md, true }
func (p posSrc) Position() (float64, bool)                 { return p.sec, p.posOK }

// A source reporting an authoritative position overrides the wall-clock guess
// (now-startedUnix), so a phone-side seek/replay reflects in the record.
func TestPlaybackRecordPrefersSourcePosition(t *testing.T) {
	now := time.Unix(1000, 0) // wall-clock would yield 1000-990 = 10s
	s := &session{uri: "spotify:", startedUnix: 990, src: posSrc{sec: 2.5, posOK: true}}
	if rec := s.playbackRecord(now, contracts.SourceStats{}); rec.PositionSec != 2.5 {
		t.Fatalf("PositionSec = %v, want 2.5 (source position, not wall-clock 10)", rec.PositionSec)
	}
}

// Until the source has a position (ok=false) the wall-clock value stands.
func TestPlaybackRecordFallsBackToWallClock(t *testing.T) {
	now := time.Unix(1000, 0)
	s := &session{uri: "spotify:", startedUnix: 990, src: posSrc{posOK: false}}
	if rec := s.playbackRecord(now, contracts.SourceStats{}); rec.PositionSec != 10 {
		t.Fatalf("PositionSec = %v, want 10 (wall-clock fallback)", rec.PositionSec)
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
