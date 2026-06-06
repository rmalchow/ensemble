package web

import (
	"io/fs"
	"testing"
)

// TestDistFSHasIndex verifies the committed placeholder compiles into the embed
// FS and is readable via fs.Sub(DistFS, "dist") — the access path the downstream
// server piece uses to serve the SPA.
func TestDistFSHasIndex(t *testing.T) {
	sub, err := fs.Sub(DistFS, "dist")
	if err != nil {
		t.Fatalf("fs.Sub(DistFS, \"dist\"): %v", err)
	}
	b, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		t.Fatalf("read index.html via sub FS: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("index.html is empty")
	}
}
