// Package source decodes the master input (mp3 via go-mp3, FLAC via mewkiz/flac,
// WAV/PCM) to PCM, from a local file or HTTP(S) stream with loop, and browses
// the data/ folder. Source formats only, never a wire codec.
package source
