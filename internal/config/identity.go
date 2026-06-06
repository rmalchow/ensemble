package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

// Identity is the node's persisted, stable identity (doc 01 §5.2, README §6.5).
// It survives restart in <data>/node.json (snake_case JSON, matching the mpvsync
// node.json convention; doc 07 §2). The NodeID is generated once and reused
// across restarts: it is the tiebreak used by master election (A.13 P0), so a
// fresh id every boot would reshuffle the master. Name is the editable friendly
// name; it is omitted from node.json when empty so a never-renamed node derives
// its display name from the node id.
//
// HWDelayUs and Device are the local persisted hints for this node's audio
// output. Once the node is adopted the authoritative copy lives in the
// replicated NodeRecord (doc 07 §2.4); reconciliation between node.json and the
// ConfigDoc is done by cmd/group, not here — config only stores the fields.
type Identity struct {
	// NodeID is the stable 128-bit id (32-char hex) generated once on first run.
	NodeID string `json:"node_id"`
	// Name is the editable friendly name. Omitted from node.json when empty.
	Name string `json:"name,omitempty"`
	// HWDelayUs is this node's per-node output latency trim, in microseconds
	// (D13). It is consumed by audio/render (doc 06) as a fixed sample offset and
	// is hardware-specific. Omitted from node.json when zero.
	HWDelayUs int `json:"hw_delay_us,omitempty"`
	// Device is the audio sink device for this node, e.g. the ALSA name "hw:0"
	// (empty => the sink's own default). It is overridden by --device / config.yaml
	// at the cmd layer. Omitted from node.json when empty.
	Device string `json:"device,omitempty"`
}

// LoadOrCreateIdentity reads <data>/node.json, creating it with a fresh random
// NodeID on first run. If the file exists but carries an empty node_id (e.g. a
// hand-edited or partially written file), a fresh id is generated and persisted
// while the other fields are preserved.
func LoadOrCreateIdentity(paths Paths) (Identity, error) {
	data, err := os.ReadFile(paths.NodeConfig)
	if err == nil {
		var id Identity
		if err := json.Unmarshal(data, &id); err != nil {
			return Identity{}, fmt.Errorf("parse %s: %w", paths.NodeConfig, err)
		}
		if id.NodeID != "" {
			return id, nil
		}
		// Empty id: (re)generate it below, preserving the other fields.
		id.NodeID = newNodeID()
		if err := SaveIdentity(paths, id); err != nil {
			return Identity{}, err
		}
		return id, nil
	} else if !os.IsNotExist(err) {
		return Identity{}, fmt.Errorf("read %s: %w", paths.NodeConfig, err)
	}

	id := Identity{NodeID: newNodeID()}
	if err := SaveIdentity(paths, id); err != nil {
		return Identity{}, err
	}
	return id, nil
}

// SaveIdentity persists the identity to <data>/node.json (mode 0644, not secret)
// atomically (temp+rename). It is used by a rename / trim edit to keep the new
// value across restarts.
func SaveIdentity(paths Paths, id Identity) error {
	out, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	tmp := paths.NodeConfig + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, paths.NodeConfig); err != nil {
		return fmt.Errorf("rename %s: %w", paths.NodeConfig, err)
	}
	return nil
}

// newNodeID returns a 128-bit random id as a 32-char hex string.
func newNodeID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
