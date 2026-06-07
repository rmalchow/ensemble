package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ensemble/internal/id"
)

// nodeFileName is the fixed basename inside DataDir.
const nodeFileName = "node.json"

// Volume / output-delay bounds (D35/D36).
const (
	minVolume      = 0.0
	maxVolume      = 1.0
	defaultVolume  = 1.0
	minDelayMs     = -500
	maxDelayMs     = 500
	defaultDelayMs = 0
)

// defaultOutputDevice is the ALSA device selected when node.json omits it (D37).
const defaultOutputDevice = "default"

// disableableFeatures is the set an operator may disable per node (D40). The
// store normalizes any persisted list down to this set, deduped + sorted.
var disableableFeatures = map[string]bool{"playback": true, "opus": true, "input": true}

// maxOutputDeviceLen bounds a hand-edited device id (matches the API cap).
const maxOutputDeviceLen = 64

var (
	// ErrCorruptNodeFile is returned when node.json exists but cannot be parsed
	// into a valid identity (bad JSON, missing/blank id, malformed id hex). The
	// id is never silently regenerated; the operator must fix or remove the file.
	ErrCorruptNodeFile = errors.New("config: node.json is corrupt")
	// ErrIDImmutable is returned when a mutator is asked to write under a node id
	// that does not match the persisted one (defensive: rename/set never changes
	// identity).
	ErrIDImmutable = errors.New("config: node id is immutable")
)

// NodeFile is the on-disk identity document (§1, D1). Exactly these four fields
// are persisted; everything else in the node record (addrs, ports, caps,
// following, observed) is runtime/replicated state owned by the cluster piece
// (C), NOT stored here.
type NodeFile struct {
	ID            id.ID    `json:"id"`            // immutable, 32-hex (id.ID TextMarshaler)
	Name          string   `json:"name"`          // renameable
	Volume        float64  `json:"volume"`        // playback gain 0.0–1.0, default 1.0 (D35)
	OutputDelayMs int      `json:"outputDelayMs"` // hardware latency calibration, default 0, clamp ±500 (D36)
	OutputDevice  string   `json:"outputDevice"`  // selected ALSA device id, default "default" (D37)
	Disabled      []string `json:"disabled"`      // operator-disabled features (D40): subset of {playback,opus,input}
}

// rawNodeFile is the presence-aware decode shape: pointer fields tell "absent"
// (→ default) from an explicit value (e.g. volume 0.0 is a real, muted setting,
// D35) — the JSON zero value alone cannot distinguish the two.
type rawNodeFile struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Volume        *float64  `json:"volume"`
	OutputDelayMs *int      `json:"outputDelayMs"`
	OutputDevice  *string   `json:"outputDevice"`
	Disabled      *[]string `json:"disabled"`
}

// Store owns a single node.json file. One Store per node. Methods are safe for
// sequential use from one goroutine; concurrent renames are the caller's
// problem (in practice only the API handler renames, serialized upstream).
// Holds the directory path, not an open fd.
type Store struct {
	path string // DataDir/node.json (absolute)
}

// NewStore returns a Store for dataDir/node.json. Does not touch disk.
func NewStore(dataDir string) *Store {
	return &Store{path: filepath.Join(dataDir, nodeFileName)}
}

// Path returns the absolute node.json path.
func (s *Store) Path() string { return s.path }

// LoadOrCreate reads node.json. If it does not exist, it creates one with a
// fresh id.New(), name = initialName (or first 8 hex of the id when
// initialName == ""), volume = 1.0 and outputDelayMs = 0 (D35/D36 defaults).
// If it exists, it is parsed and returned with the id+name UNCHANGED (the id is
// immutable, §1; initialName is ignored on an existing file) — while a MISSING
// volume defaults to 1.0 and a missing outputDelayMs to 0 (back-compat for
// files predating D35/D36), and volume/outputDelayMs are clamped on load (a
// hand-edited out-of-range value is corrected, not rejected). A file that
// exists but is empty/corrupt/has a malformed id is ErrCorruptNodeFile (we
// never silently regenerate an id, which would orphan the cluster identity).
func (s *Store) LoadOrCreate(initialName string) (NodeFile, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s.create(initialName)
	}
	if err != nil {
		return NodeFile{}, fmt.Errorf("config: read node.json: %w", err)
	}

	var raw rawNodeFile
	if err := json.Unmarshal(data, &raw); err != nil {
		return NodeFile{}, ErrCorruptNodeFile
	}
	nodeID, err := id.Parse(raw.ID)
	if err != nil {
		return NodeFile{}, ErrCorruptNodeFile
	}

	nf := NodeFile{
		ID:            nodeID,
		Name:          raw.Name,
		Volume:        defaultVolume,
		OutputDelayMs: defaultDelayMs,
		OutputDevice:  defaultOutputDevice,
	}
	if raw.Volume != nil {
		nf.Volume = *raw.Volume
	}
	if raw.OutputDelayMs != nil {
		nf.OutputDelayMs = *raw.OutputDelayMs
	}
	if raw.OutputDevice != nil {
		nf.OutputDevice = *raw.OutputDevice
	}
	if raw.Disabled != nil {
		nf.Disabled = *raw.Disabled
	}
	nf.Volume = clampVolume(nf.Volume)
	nf.OutputDelayMs = clampDelayMs(nf.OutputDelayMs)
	nf.OutputDevice = normalizeDevice(nf.OutputDevice)
	nf.Disabled = normalizeDisabled(nf.Disabled)
	return nf, nil
}

// create mints a fresh identity and writes the initial node.json.
func (s *Store) create(initialName string) (NodeFile, error) {
	nodeID := id.New()
	name := initialName
	if name == "" {
		name = defaultName(nodeID)
	}
	nf := NodeFile{
		ID:            nodeID,
		Name:          name,
		Volume:        defaultVolume,
		OutputDelayMs: defaultDelayMs,
		OutputDevice:  defaultOutputDevice,
	}
	if err := s.write(nf); err != nil {
		return NodeFile{}, err
	}
	return nf, nil
}

// Rename writes a new name while preserving the immutable id, volume, and
// outputDelayMs, via the atomic replace below. The id argument MUST equal the
// persisted id (ErrIDImmutable on mismatch). Returns the written NodeFile.
func (s *Store) Rename(nodeID id.ID, name string) (NodeFile, error) {
	return s.writeAtomic(nodeID, func(nf *NodeFile) {
		nf.Name = name
	})
}

// SetVolume writes a new volume while preserving id/name/outputDelayMs, via the
// same atomic replace. The id argument MUST equal the persisted id
// (ErrIDImmutable on mismatch). vol is clamped to [0.0, 1.0] before write (D35).
func (s *Store) SetVolume(nodeID id.ID, vol float64) (NodeFile, error) {
	return s.writeAtomic(nodeID, func(nf *NodeFile) {
		nf.Volume = clampVolume(vol)
	})
}

// SetOutputDelayMs writes a new outputDelayMs while preserving id/name/volume,
// via the same atomic replace. The id argument MUST equal the persisted id
// (ErrIDImmutable on mismatch). ms is clamped to [-500, 500] before write (D36).
func (s *Store) SetOutputDelayMs(nodeID id.ID, ms int) (NodeFile, error) {
	return s.writeAtomic(nodeID, func(nf *NodeFile) {
		nf.OutputDelayMs = clampDelayMs(ms)
	})
}

// SetOutputDevice writes a new outputDevice while preserving id/name/volume/
// outputDelayMs, via the same atomic replace. The id argument MUST equal the
// persisted id (ErrIDImmutable on mismatch). The device is normalized (blank →
// "default", trimmed, capped) before write (D37).
func (s *Store) SetOutputDevice(nodeID id.ID, device string) (NodeFile, error) {
	return s.writeAtomic(nodeID, func(nf *NodeFile) {
		nf.OutputDevice = normalizeDevice(device)
	})
}

// SetDisabled writes the operator-disabled feature list while preserving the
// other fields, via the same atomic replace (D40). The id argument MUST equal the
// persisted id (ErrIDImmutable on mismatch). The list is normalized (subset of
// {playback,opus,input}, deduped + sorted) before write.
func (s *Store) SetDisabled(nodeID id.ID, disabled []string) (NodeFile, error) {
	return s.writeAtomic(nodeID, func(nf *NodeFile) {
		nf.Disabled = normalizeDisabled(disabled)
	})
}

// writeAtomic re-reads the current NodeFile, asserts the id matches, applies the
// single-field mutate, and atomically replaces node.json. On any error the old
// file is untouched.
func (s *Store) writeAtomic(nodeID id.ID, mutate func(*NodeFile)) (NodeFile, error) {
	cur, err := s.LoadOrCreate("")
	if err != nil {
		return NodeFile{}, err
	}
	if cur.ID != nodeID {
		return NodeFile{}, ErrIDImmutable
	}
	mutate(&cur)
	cur.Volume = clampVolume(cur.Volume)
	cur.OutputDelayMs = clampDelayMs(cur.OutputDelayMs)
	cur.OutputDevice = normalizeDevice(cur.OutputDevice)
	cur.Disabled = normalizeDisabled(cur.Disabled)
	if err := s.write(cur); err != nil {
		return NodeFile{}, err
	}
	return cur, nil
}

// write serializes nf to node.json atomically: a temp file in the same
// directory, fsync, close, then os.Rename onto the target (same filesystem).
func (s *Store) write(nf NodeFile) error {
	data, err := json.MarshalIndent(nf, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal node.json: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, nodeFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("config: create temp node.json: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("config: write temp node.json: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("config: sync temp node.json: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("config: close temp node.json: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("config: replace node.json: %w", err)
	}
	return nil
}

// defaultName returns the first 8 hex chars of the id (§1 default node name).
func defaultName(nodeID id.ID) string {
	return nodeID.String()[:8]
}

func clampVolume(v float64) float64 {
	if v < minVolume {
		return minVolume
	}
	if v > maxVolume {
		return maxVolume
	}
	return v
}

// normalizeDevice trims, defaults a blank value to "default", and caps the
// length of an output-device id (D37). It does not validate against an
// enumerated list — that is the API handler's job at PATCH time.
func normalizeDevice(d string) string {
	d = strings.TrimSpace(d)
	if d == "" {
		return defaultOutputDevice
	}
	if len(d) > maxOutputDeviceLen {
		d = d[:maxOutputDeviceLen]
	}
	return d
}

// normalizeDisabled keeps only valid disableable features (D40), deduped and
// sorted for a stable on-disk + replicated representation. Returns nil for an
// empty result so the JSON encodes as a present-but-empty (or absent) list
// without churn.
func normalizeDisabled(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range in {
		f = strings.TrimSpace(f)
		if disableableFeatures[f] && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

func clampDelayMs(ms int) int {
	if ms < minDelayMs {
		return minDelayMs
	}
	if ms > maxDelayMs {
		return maxDelayMs
	}
	return ms
}
