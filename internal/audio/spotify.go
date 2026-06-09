package audio

import (
	"context"
	"fmt"
	"io"
)

// spotifyInputRate is the PCM sample rate go-librespot's pipe backend emits for
// Spotify content (44.1 kHz, s16le stereo). The framer resamples it to 48 kHz.
const spotifyInputRate = 44100

// spotifyAttach is set by main when the Spotify bridge (internal/spotify) is running:
// it returns a live reader of go-librespot's PCM. openSpotify reads from it rather
// than spawning a process — the bridge owns the one go-librespot instance (D57).
var spotifyAttach func() (io.ReadCloser, error)

// SetSpotifyAttach wires the bridge's audio tap. nil (the default) means no bridge is
// running, so opening a "spotify:" source fails cleanly.
func SetSpotifyAttach(fn func() (io.ReadCloser, error)) { spotifyAttach = fn }

// FindSpotifyBinary returns the go-librespot/librespot binary (working directory
// first, then $PATH), or "" — so main can decide whether to launch the bridge.
func FindSpotifyBinary() string { return findSpotifyBinary() }

// spotifySource is a live-paced source over the Spotify bridge's PCM tap.
type spotifySource struct {
	*liveReader
}

// openSpotify attaches to the running Spotify bridge and streams its PCM. It is live
// (never EOF): with no track playing the bridge yields nothing and the live layer
// emits silence; the phone starting playback fills the pipe. The bridge drives the
// actual switch to/from this source via the engine (the URI carries no payload).
func openSpotify(_ context.Context, _, _ string) (Source, error) {
	if spotifyAttach == nil {
		return nil, fmt.Errorf("%w: no Spotify bridge", ErrBadMedia)
	}
	r, err := spotifyAttach()
	if err != nil {
		return nil, fmt.Errorf("%w: spotify attach: %v", ErrBadMedia, err)
	}
	dec := &rawS16Source{r: r, rate: spotifyInputRate}
	fr := newFramer(dec)
	cleanup := func() { _ = r.Close() }
	lr := newLiveReader(fr, func() {}, cleanup)
	return &spotifySource{liveReader: lr}, nil
}
