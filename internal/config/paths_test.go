package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOpenDataDirLayout(t *testing.T) {
	dir := t.TempDir()
	p, err := OpenDataDir(dir)
	if err != nil {
		t.Fatalf("OpenDataDir: %v", err)
	}

	// Root is absolute and equals filepath.Abs(dir).
	wantRoot, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	if !filepath.IsAbs(p.Root) {
		t.Errorf("Root %q is not absolute", p.Root)
	}
	if p.Root != wantRoot {
		t.Errorf("Root = %q, want %q", p.Root, wantRoot)
	}

	// Every field equals filepath.Join(Root, expected) per the spec layout.
	tests := []struct {
		field string
		got   string
		rel   string
	}{
		{"Config", p.Config, "config.yaml"},
		{"NodeConfig", p.NodeConfig, "node.json"},
		{"Cluster", p.Cluster, "cluster.yaml"},
		{"Doc", p.Doc, "doc.json"},
		{"Peers", p.Peers, "peers.json"},
		{"Certs", p.Certs, "certs"},
		{"CACert", p.CACert, "certs/ca.crt"},
		{"NodeKey", p.NodeKey, "certs/node.key"},
		{"NodeCert", p.NodeCert, "certs/node.crt"},
		{"Data", p.Data, "data"},
		{"Run", p.Run, "run"},
	}
	for _, tt := range tests {
		want := filepath.Join(wantRoot, filepath.FromSlash(tt.rel))
		if tt.got != want {
			t.Errorf("%s = %q, want %q", tt.field, tt.got, want)
		}
	}
}

func TestOpenDataDirCreatesDirs(t *testing.T) {
	dir := t.TempDir()
	p, err := OpenDataDir(dir)
	if err != nil {
		t.Fatalf("OpenDataDir: %v", err)
	}

	// The directory set {Root, Certs, Data, Run} must exist.
	for _, d := range []struct {
		name string
		path string
	}{
		{"Root", p.Root},
		{"Certs", p.Certs},
		{"Data", p.Data},
		{"Run", p.Run},
	} {
		info, err := os.Stat(d.path)
		if err != nil {
			t.Errorf("%s %q: stat: %v", d.name, d.path, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s %q is not a directory", d.name, d.path)
		}
	}

	// Leaf files must NOT be created by OpenDataDir.
	for _, f := range []struct {
		name string
		path string
	}{
		{"NodeConfig", p.NodeConfig},
		{"Doc", p.Doc},
		{"CACert", p.CACert},
		{"Config", p.Config},
		{"Cluster", p.Cluster},
		{"Peers", p.Peers},
	} {
		if _, err := os.Stat(f.path); !os.IsNotExist(err) {
			t.Errorf("%s %q exists or errored unexpectedly (err=%v); should not be created", f.name, f.path, err)
		}
	}
}

func TestOpenDataDirCertsPerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits not meaningful on this OS")
	}
	dir := t.TempDir()
	p, err := OpenDataDir(dir)
	if err != nil {
		t.Fatalf("OpenDataDir: %v", err)
	}
	info, err := os.Stat(p.Certs)
	if err != nil {
		t.Fatalf("stat certs: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("Certs perm = %04o, want 0700", perm)
	}
}

func TestOpenDataDirIdempotent(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenDataDir(dir); err != nil {
		t.Fatalf("first OpenDataDir: %v", err)
	}
	// A second call on the same dir must be a no-op success.
	if _, err := OpenDataDir(dir); err != nil {
		t.Fatalf("second OpenDataDir: %v", err)
	}
}

func TestOpenDataDirBadPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only parent semantics differ on this OS")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	parent := t.TempDir()
	ro := filepath.Join(parent, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatalf("mkdir ro: %v", err)
	}
	_, err := OpenDataDir(filepath.Join(ro, "child"))
	if err == nil {
		t.Fatalf("OpenDataDir under read-only parent: want error, got nil")
	}
}
