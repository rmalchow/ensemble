package audio

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"ensemble/internal/stream"
)

func TestOpenSchemeDispatch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Unsupported schemes. (spotify: is a registered scheme now — D57 — even when
	// its binary is absent it routes to the spotify source, not ErrUnsupportedScheme.)
	for _, uri := range []string{"ftp://x/y", "gopher://x"} {
		if _, err := Open(ctx, uri, dir); !errors.Is(err, ErrUnsupportedScheme) {
			t.Fatalf("%q err = %v, want ErrUnsupportedScheme", uri, err)
		}
	}

	// file: and bare path route to file source (missing file → ErrBadMedia, not
	// scheme/traversal).
	if _, err := Open(ctx, "file:nope.wav", dir); !errors.Is(err, ErrBadMedia) {
		t.Fatalf("file: missing err = %v, want ErrBadMedia", err)
	}
	if _, err := Open(ctx, "nope.wav", dir); !errors.Is(err, ErrBadMedia) {
		t.Fatalf("bare missing err = %v, want ErrBadMedia", err)
	}

	// http/https route to the http source (bad host → ErrBadMedia at Open).
	if _, err := Open(ctx, "http://127.0.0.1:1/x.wav", dir); !errors.Is(err, ErrBadMedia) {
		t.Fatalf("http err = %v, want ErrBadMedia", err)
	}
}

func TestSchemesReportsFileHTTPAlways(t *testing.T) {
	s := Schemes()
	has := func(x string) bool {
		for _, v := range s {
			if v == x {
				return true
			}
		}
		return false
	}
	if !has(SchemeFile) || !has(SchemeHTTP) {
		t.Fatalf("Schemes missing file/http: %v", s)
	}
	if s[0] != SchemeFile || s[1] != SchemeHTTP {
		t.Fatalf("Schemes order = %v, want file,http first", s)
	}
}

func TestSchemesInputDependsOnPath(t *testing.T) {
	// Empty PATH → no capture binary → no "input".
	t.Setenv("PATH", "")
	for _, v := range Schemes() {
		if v == SchemeInput {
			t.Fatalf("input advertised with empty PATH")
		}
	}

	// A faked capture binary on PATH → "input" present.
	dir := t.TempDir()
	fake := filepath.Join(dir, "pw-record")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	found := false
	for _, v := range Schemes() {
		if v == SchemeInput {
			found = true
		}
	}
	if !found {
		t.Fatalf("input not advertised with capture binary on PATH")
	}
}

func TestSchemesSpotifyDependsOnBinary(t *testing.T) {
	// Empty PATH + no local binary → no "spotify".
	t.Setenv("PATH", "")
	for _, v := range Schemes() {
		if v == SchemeSpotify {
			t.Fatalf("spotify advertised with no librespot binary")
		}
	}

	// A faked librespot on PATH → "spotify" present (Schemes only LookPaths it).
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "librespot"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	found := false
	for _, v := range Schemes() {
		if v == SchemeSpotify {
			found = true
		}
	}
	if !found {
		t.Fatalf("spotify not advertised with librespot on PATH")
	}
}

func TestFileEOFContract(t *testing.T) {
	// D9: last frame nil (zero-padded tail), next call io.EOF; Live()==false.
	dir := t.TempDir()
	in := genTone(48000, 2, 440, 960+5)
	if err := os.WriteFile(filepath.Join(dir, "t.wav"), writeWAVs16(48000, 2, in), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := Open(context.Background(), "file:t.wav", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if src.Live() {
		t.Fatalf("file source reports Live()")
	}
	buf := make([]byte, stream.FrameBytes)
	if err := src.ReadFrame(buf); err != nil {
		t.Fatalf("frame 0: %v", err)
	}
	if err := src.ReadFrame(buf); err != nil {
		t.Fatalf("frame 1: %v", err)
	}
	if err := src.ReadFrame(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("frame 2: %v, want io.EOF", err)
	}
}

func TestDecodeMP3Fixture(t *testing.T) {
	p, skip := maybeFixture(t, "tone.mp3")
	if skip {
		t.Skip("no testdata/tone.mp3 fixture")
	}
	decodeFixture(t, p, "mp3")
}

func TestDecodeFLACFixture(t *testing.T) {
	p, skip := maybeFixture(t, "tone.flac")
	if skip {
		t.Skip("no testdata/tone.flac fixture")
	}
	decodeFixture(t, p, "flac")
}

func decodeFixture(t *testing.T, path, format string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec, err := newDecoder(f, format)
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	fr := newFramer(dec)
	buf := make([]byte, stream.FrameBytes)
	frames := 0
	for {
		err := fr.frame(buf)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("frame %d: %v", frames, err)
		}
		frames++
		if frames > 100000 {
			t.Fatal("runaway")
		}
	}
	if frames == 0 {
		t.Fatalf("%s decoded zero frames", format)
	}
}
