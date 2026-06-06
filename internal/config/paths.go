package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultDataDir is the default value for the --data flag.
const DefaultDataDir = "./data"

// Paths holds the resolved locations within a node's data directory (doc 01
// §5.2). It is filesystem layout only: most leaf files are written by their
// owning packages (Identity here, certs by pki, doc.json by state), not by
// OpenDataDir.
type Paths struct {
	Root       string // <data>
	Config     string // <data>/config.yaml      operator config
	NodeConfig string // <data>/node.json         persisted Identity (0644)
	Cluster    string // <data>/cluster.yaml      membership marker (0600)
	Doc        string // <data>/doc.json          persisted ConfigDoc (0644)
	Peers      string // <data>/peers.json        gossip seed cache (A.14.1, 0644)
	Certs      string // <data>/certs/            dir holding ca.crt/node.key/node.crt (0700)
	CACert     string // <data>/certs/ca.crt      cluster CA public cert
	NodeKey    string // <data>/certs/node.key    this node's private key (0600)
	NodeCert   string // <data>/certs/node.crt    this node's signed leaf
	Data       string // <data>/data/             media folder (mp3s) per node
	Run        string // <data>/run/              runtime sockets / scratch
}

// OpenDataDir resolves dir to an absolute path, creates the directory set
// {Root, Certs, Data, Run} if missing, and returns the resolved Paths. Root,
// Data and Run are created 0755; Certs is created 0700 because it holds the
// node's private key (doc 01 §5.2). Leaf files are NOT created here — they are
// written by their owners (Identity here, certs by pki, doc.json by state).
func OpenDataDir(dir string) (Paths, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Paths{}, fmt.Errorf("resolve data dir %q: %w", dir, err)
	}
	certs := filepath.Join(abs, "certs")
	p := Paths{
		Root:       abs,
		Config:     filepath.Join(abs, "config.yaml"),
		NodeConfig: filepath.Join(abs, "node.json"),
		Cluster:    filepath.Join(abs, "cluster.yaml"),
		Doc:        filepath.Join(abs, "doc.json"),
		Peers:      filepath.Join(abs, "peers.json"),
		Certs:      certs,
		CACert:     filepath.Join(certs, "ca.crt"),
		NodeKey:    filepath.Join(certs, "node.key"),
		NodeCert:   filepath.Join(certs, "node.crt"),
		Data:       filepath.Join(abs, "data"),
		Run:        filepath.Join(abs, "run"),
	}
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{
		{p.Root, 0o755},
		{p.Data, 0o755},
		{p.Run, 0o755},
		{p.Certs, 0o700}, // holds node.key
	} {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return Paths{}, fmt.Errorf("create %q: %w", d.path, err)
		}
	}
	return p, nil
}
