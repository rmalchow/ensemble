package web

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestWebImportConstraints enforces doc 01 §2 rule 1: internal/web must never
// import group, stream/* or audio/* — the engine is reachable only through the
// Deps function-value seam. It parses every non-test .go file in this package
// and fails if any forbidden module path appears in an import.
func TestWebImportConstraints(t *testing.T) {
	const modPrefix = "gitlab.rand0m.me/ruben/go/ensemble/internal/"
	forbidden := []string{"group", "stream/", "stream\"", "audio/"}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("unquote import %s in %s: %v", imp.Path.Value, name, err)
			}
			rel, ok := strings.CutPrefix(p, modPrefix)
			if !ok {
				continue
			}
			for _, bad := range forbidden {
				b := strings.TrimSuffix(bad, "\"")
				if rel == b || strings.HasPrefix(rel, b) {
					t.Errorf("%s imports forbidden package %q (doc 01 §2 rule 1: web must not import group/stream/audio)", name, p)
				}
			}
		}
	}
}
