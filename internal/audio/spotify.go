package audio

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"ensemble/internal/contracts"
)

// spotifyInputRate is the PCM sample rate go-librespot's pipe backend emits for
// Spotify content (44.1 kHz, s16le stereo). The framer resamples it to 48 kHz.
const spotifyInputRate = 44100

// spotifyEndpoint is a registered bridge: its live PCM tap + metadata/position
// accessors.
type spotifyEndpoint struct {
	attach func() (io.ReadCloser, error)
	meta   func() (contracts.TrackMetadata, bool)
	pos    func() (float64, bool) // authoritative position (s); ok=false → unknown yet
}

// spotifyReg maps an endpoint id to its bridge (D57: a node may run several
// Spotify Connect bridges). The default endpoint registers under "". The Spotify
// manager (internal/spotify) registers/unregisters as bridges come and go; a
// "spotify:<id>" URI resolves to the matching entry.
var (
	spotifyMu  sync.RWMutex
	spotifyReg = map[string]spotifyEndpoint{}
)

// RegisterSpotifyEndpoint wires a bridge's audio tap + metadata/position accessors
// under an endpoint id ("" = the default endpoint). Replaces any existing entry.
// pos may be nil (no position channel); the source then reports none.
func RegisterSpotifyEndpoint(id string, attach func() (io.ReadCloser, error), meta func() (contracts.TrackMetadata, bool), pos func() (float64, bool)) {
	spotifyMu.Lock()
	spotifyReg[id] = spotifyEndpoint{attach: attach, meta: meta, pos: pos}
	spotifyMu.Unlock()
}

// UnregisterSpotifyEndpoint removes a bridge's registration (bridge stopped).
func UnregisterSpotifyEndpoint(id string) {
	spotifyMu.Lock()
	delete(spotifyReg, id)
	spotifyMu.Unlock()
}

func lookupSpotify(id string) (spotifyEndpoint, bool) {
	spotifyMu.RLock()
	e, ok := spotifyReg[id]
	spotifyMu.RUnlock()
	return e, ok
}

// FindSpotifyBinary returns the go-librespot/librespot binary (working directory
// first, then $PATH), or "" — so main can decide whether to launch the bridge.
func FindSpotifyBinary() string { return findSpotifyBinary() }

// spotifyEndpointID extracts the endpoint id from a spotify URI: "spotify" or
// "spotify:" → "" (default); "spotify:<id>" → "<id>".
func spotifyEndpointID(uri string) string {
	i := strings.IndexByte(uri, ':')
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(uri[i+1:])
}

// spotifySource is a live-paced source over a Spotify bridge's PCM tap.
type spotifySource struct {
	*liveReader
	meta func() (contracts.TrackMetadata, bool)
	pos  func() (float64, bool)
}

// Metadata satisfies the optional metadata channel: the current Spotify track.
func (s *spotifySource) Metadata() (contracts.TrackMetadata, bool) {
	if s.meta == nil {
		return contracts.TrackMetadata{}, false
	}
	return s.meta()
}

// Position satisfies the optional authoritative-position channel: go-librespot's
// reported position (interpolated), so a phone-side seek/replay reaches the bar
// instead of the master's wall-clock guess. ok=false → none yet (use wall-clock).
func (s *spotifySource) Position() (float64, bool) {
	if s.pos == nil {
		return 0, false
	}
	return s.pos()
}

// openSpotify attaches to the running Spotify bridge for the URI's endpoint id and
// streams its PCM. It is live (never EOF): with no track playing the bridge yields
// nothing and the live layer emits silence; the phone starting playback fills the
// pipe. The bridge drives the actual switch to/from this source via the engine.
func openSpotify(_ context.Context, uri, _ string) (Source, error) {
	ep, ok := lookupSpotify(spotifyEndpointID(uri))
	if !ok {
		return nil, fmt.Errorf("%w: no Spotify bridge for %q", ErrBadMedia, uri)
	}
	r, err := ep.attach()
	if err != nil {
		return nil, fmt.Errorf("%w: spotify attach: %v", ErrBadMedia, err)
	}
	dec := &rawS16Source{r: r, rate: spotifyInputRate}
	fr := newFramer(dec)
	cleanup := func() { _ = r.Close() }
	lr := newLiveReader(fr, func() {}, cleanup)
	return &spotifySource{liveReader: lr, meta: ep.meta, pos: ep.pos}, nil
}
