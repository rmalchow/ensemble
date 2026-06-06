package source

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestHTTPInput serves a clip over HTTP and asserts Open decodes it, loops by
// re-issuing the request (non-seekable body), and surfaces server errors.
func TestHTTPInput(t *testing.T) {
	dir := t.TempDir()
	const rate = 48000
	writeFLAC(t, filepath.Join(dir, "sine.flac"), sineSamples(6000, rate, 2), rate, 2)
	clip, err := os.ReadFile(filepath.Join(dir, "sine.flac"))
	if err != nil {
		t.Fatal(err)
	}

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sine.flac":
			requests++
			// Force a non-seekable body: no Content-Length, chunked.
			w.Header().Set("Content-Type", "audio/flac")
			w.Write(clip)
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	rd, err := Open(srv.URL+"/sine.flac", rate, 2)
	if err != nil {
		t.Fatalf("Open(http): %v", err)
	}
	defer rd.Close()
	if rd.Rate() != rate || rd.Channels() != 2 {
		t.Fatalf("rate/ch = %d/%d", rd.Rate(), rd.Channels())
	}

	// Read past the clip length: the loop must re-request (non-seekable body) and
	// never return io.EOF.
	buf := make([]float32, 4096)
	totalFrames := 0
	for totalFrames < 6000*3 { // ~3 loops
		n, rerr := rd.Read(buf)
		if rerr == io.EOF {
			t.Fatalf("io.EOF while looping http stream")
		}
		if rerr != nil {
			t.Fatalf("Read http: %v", rerr)
		}
		if n%2 != 0 {
			t.Fatalf("short frame n=%d", n)
		}
		if n == 0 {
			t.Fatal("zero-length read without progress")
		}
		totalFrames += n / 2
	}
	if requests < 2 {
		t.Errorf("expected the loop to re-issue the GET (requests=%d)", requests)
	}

	// 404 / 500 → error from Open.
	if r, err := Open(srv.URL+"/missing", rate, 2); err == nil {
		r.Close()
		t.Error("Open(404): expected error")
	}
	if r, err := Open(srv.URL+"/boom", rate, 2); err == nil {
		r.Close()
		t.Error("Open(500): expected error")
	}
}

// TestMonoUpmix verifies a mono source is duplicated to stereo before resample.
func TestMonoUpmix(t *testing.T) {
	dir := t.TempDir()
	// Mono FLAC at 44100 — exercises mono→stereo + resample + loop together.
	writeFLAC(t, filepath.Join(dir, "mono.flac"), sineSamples(4410, 44100, 1), 44100, 1)
	r, err := openClip("mono.flac", dir, canonRate, canonCh)
	if err != nil {
		t.Fatalf("Open mono: %v", err)
	}
	defer r.Close()
	if r.Channels() != 2 {
		t.Fatalf("Channels()=%d want 2", r.Channels())
	}
	buf := make([]float32, 2000)
	n, rerr := r.Read(buf)
	if rerr != nil {
		t.Fatalf("Read: %v", rerr)
	}
	if n == 0 || n%2 != 0 {
		t.Fatalf("n=%d", n)
	}
	// Duplicated channels: L == R for every frame.
	for i := 0; i < n; i += 2 {
		if buf[i] != buf[i+1] {
			t.Fatalf("mono upmix not duplicated at frame %d: %v != %v", i/2, buf[i], buf[i+1])
		}
	}
}

// TestPathTraversal rejects data-dir escapes.
func TestPathTraversal(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"../etc/passwd", "../../secret.flac", "/etc/hosts"} {
		if r, err := openClip(p, dir, canonRate, canonCh); err == nil {
			r.Close()
			t.Errorf("Open(%q): expected traversal rejection", p)
		}
	}
}
