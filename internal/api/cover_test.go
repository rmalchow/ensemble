package api

import (
	"io"
	"net/http"
	"testing"

	"ensemble/internal/id"
)

// GET /cover streams the bytes + content type the Media layer reports, with a
// caching header, when art exists.
func TestHandleCoverServesArt(t *testing.T) {
	cfg, _, _ := baseConfig(id.New())
	cfg.Media = &fakeMedia{cover: []byte("JPEGDATA"), coverType: "image/jpeg", coverOK: true}
	_, ts := testServer(t, cfg)

	resp, err := http.Get(ts.URL + "/api/cover?uri=file:song.mp3")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("content type = %q, want image/jpeg", ct)
	}
	if resp.Header.Get("Cache-Control") == "" {
		t.Error("missing Cache-Control header")
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "JPEGDATA" {
		t.Errorf("body = %q, want JPEGDATA", body)
	}
}

// No art → 404 so the UI's <img> onerror can collapse the slot.
func TestHandleCoverNotFound(t *testing.T) {
	cfg, _, _ := baseConfig(id.New())
	cfg.Media = &fakeMedia{coverOK: false}
	_, ts := testServer(t, cfg)

	resp, err := http.Get(ts.URL + "/api/cover?uri=file:bare.mp3")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// A missing uri is a bad request.
func TestHandleCoverMissingURI(t *testing.T) {
	cfg, _, _ := baseConfig(id.New())
	_, ts := testServer(t, cfg)

	resp, err := http.Get(ts.URL + "/api/cover")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
