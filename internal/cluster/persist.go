package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"ensemble/internal/id"
)

// clusterState is the persisted lookup table (D41): the group NAMES + SETTINGS
// maps with their FULL records (incl. version + writer) so the load-vs-gossip
// merge follows the exact same LWW rule. Node records and playback are NOT
// persisted (runtime/replicated). Maps are keyed by group id (hex via the
// id.ID TextMarshaler).
type clusterState struct {
	Groups   map[id.ID]*GroupNameRecord     `json:"groups"`
	Settings map[id.ID]*GroupSettingsRecord `json:"settings"`
}

// snapshotState clones the doc's names + settings maps under the lock for an
// atomic save. Caller must NOT hold c.mu.
func (c *Cluster) snapshotState() clusterState {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := clusterState{
		Groups:   make(map[id.ID]*GroupNameRecord, len(c.doc.Groups)),
		Settings: make(map[id.ID]*GroupSettingsRecord, len(c.doc.Settings)),
	}
	for k, v := range c.doc.Groups {
		cp := *v
		st.Groups[k] = &cp
	}
	for k, v := range c.doc.Settings {
		cp := *v
		st.Settings[k] = &cp
	}
	return st
}

// into merges the loaded state into doc using the exact LWW rules (older loaded
// versions lose to a newer gossiped record, and vice-versa). Called at New with
// a fresh doc (so every loaded record wins into the empty maps); the merge rules
// matter once gossip starts.
func (s clusterState) into(doc *Document) {
	for g, r := range s.Groups {
		doc.mergeGroupName(g, r)
	}
	for g, r := range s.Settings {
		doc.mergeSettings(g, r)
	}
}

// markDirty signals the save loop that the names/settings tables changed (D41).
// Coalesced (buffer 1, non-blocking); a no-op when persistence is disabled.
func (c *Cluster) markDirty() {
	if c.statePath == "" || c.dirty == nil {
		return
	}
	select {
	case c.dirty <- struct{}{}:
	default:
	}
}

// saveLoop debounces dirty signals and writes the lookup table at most once per
// saveDebounce after the last change (D41). A final save also runs in Close.
func (c *Cluster) saveLoop() {
	defer c.wg.Done()
	var pending bool
	// A stopped timer with a drained channel; (re)armed on the first dirty signal.
	timer := time.NewTimer(c.saveDebounce)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-c.dirty:
			if !pending {
				pending = true
				timer.Reset(c.saveDebounce)
			}
			// Already pending: coalesce (do NOT reset, so a storm of changes still
			// saves within ~one debounce window — bounded latency).
		case <-timer.C:
			pending = false
			if err := c.saveState(); err != nil {
				c.log.Warn("cluster state save failed", "path", c.statePath, "err", err)
			}
			if c.saveNotify != nil {
				select {
				case c.saveNotify <- struct{}{}:
				default:
				}
			}
		}
	}
}

// saveState writes the names + settings tables to statePath atomically (temp +
// fsync + rename in the same dir), mirroring node.json's write (D41). No-op when
// persistence is disabled.
func (c *Cluster) saveState() error {
	if c.statePath == "" {
		return nil
	}
	st := c.snapshotState()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("cluster: marshal state: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(c.statePath)
	tmp, err := os.CreateTemp(dir, filepath.Base(c.statePath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("cluster: create temp state: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("cluster: write temp state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("cluster: sync temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("cluster: close temp state: %w", err)
	}
	if err := os.Rename(tmpPath, c.statePath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("cluster: replace state: %w", err)
	}
	return nil
}

// loadState reads the persisted lookup table from path (D41). A missing file is
// not an error (returns an empty state); a corrupt file IS an error (the caller
// warns + starts empty, never fatal).
func loadState(path string) (clusterState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return clusterState{}, nil
	}
	if err != nil {
		return clusterState{}, fmt.Errorf("cluster: read state: %w", err)
	}
	var st clusterState
	if err := json.Unmarshal(data, &st); err != nil {
		return clusterState{}, fmt.Errorf("cluster: parse state: %w", err)
	}
	return st, nil
}
