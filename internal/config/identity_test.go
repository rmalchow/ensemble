package config

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// openTmp resolves a fresh data dir for identity tests.
func openTmp(t *testing.T) Paths {
	t.Helper()
	p, err := OpenDataDir(t.TempDir())
	if err != nil {
		t.Fatalf("OpenDataDir: %v", err)
	}
	return p
}

func TestLoadOrCreateIdentityFirstRun(t *testing.T) {
	p := openTmp(t)
	id, err := LoadOrCreateIdentity(p)
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}
	if len(id.NodeID) != 32 {
		t.Errorf("NodeID = %q (len %d), want 32-char hex", id.NodeID, len(id.NodeID))
	}
	if _, err := os.Stat(p.NodeConfig); err != nil {
		t.Errorf("node.json not written: %v", err)
	}
}

func TestLoadOrCreateIdentityStability(t *testing.T) {
	p := openTmp(t)
	first, err := LoadOrCreateIdentity(p)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	before, err := os.ReadFile(p.NodeConfig)
	if err != nil {
		t.Fatalf("read after first: %v", err)
	}

	// A simulated restart must return the same NodeID and not rewrite the file.
	second, err := LoadOrCreateIdentity(p)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.NodeID != first.NodeID {
		t.Errorf("NodeID changed across restart: %q -> %q", first.NodeID, second.NodeID)
	}
	after, err := os.ReadFile(p.NodeConfig)
	if err != nil {
		t.Fatalf("read after second: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("node.json changed on second load:\nbefore=%s\nafter =%s", before, after)
	}
}

func TestLoadOrCreateIdentityEmptyIDRegen(t *testing.T) {
	p := openTmp(t)
	const seeded = `{"node_id":"","name":"studio"}`
	if err := os.WriteFile(p.NodeConfig, []byte(seeded), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, err := LoadOrCreateIdentity(p)
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}
	if len(id.NodeID) != 32 {
		t.Errorf("NodeID = %q, want fresh 32-char hex", id.NodeID)
	}
	if id.Name != "studio" {
		t.Errorf("Name = %q, want preserved \"studio\"", id.Name)
	}
	// The regenerated id must be persisted.
	reloaded, err := LoadOrCreateIdentity(p)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.NodeID != id.NodeID {
		t.Errorf("regenerated id not persisted: %q -> %q", id.NodeID, reloaded.NodeID)
	}
}

func TestLoadOrCreateIdentityCorruptJSON(t *testing.T) {
	p := openTmp(t)
	if err := os.WriteFile(p.NodeConfig, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := LoadOrCreateIdentity(p)
	if err == nil {
		t.Fatalf("want parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error %q does not mention parse", err)
	}
	// The corrupt file must not be silently overwritten.
	data, rerr := os.ReadFile(p.NodeConfig)
	if rerr != nil {
		t.Fatalf("read: %v", rerr)
	}
	if string(data) != "{not json" {
		t.Errorf("corrupt file was overwritten: %q", data)
	}
}

func TestSaveIdentityRoundTrip(t *testing.T) {
	p := openTmp(t)
	want := Identity{NodeID: "0123456789abcdef0123456789abcdef", Name: "kitchen", HWDelayUs: 4200, Device: "hw:0"}
	if err := SaveIdentity(p, want); err != nil {
		t.Fatalf("SaveIdentity: %v", err)
	}
	got, err := LoadOrCreateIdentity(p)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch:\n got=%+v\nwant=%+v", got, want)
	}
	info, err := os.Stat(p.NodeConfig)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("node.json perm = %04o, want 0644", perm)
	}
	// The atomic temp file must be gone.
	if _, err := os.Stat(p.NodeConfig + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file remains (err=%v)", err)
	}
}

func TestIdentityOmitEmpty(t *testing.T) {
	p := openTmp(t)
	if err := SaveIdentity(p, Identity{NodeID: "0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("SaveIdentity: %v", err)
	}
	data, err := os.ReadFile(p.NodeConfig)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Confirm omitempty fields are absent from the marshalled JSON.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"name", "hw_delay_us", "device"} {
		if _, ok := m[k]; ok {
			t.Errorf("zero-value field %q should be omitted, got: %s", k, data)
		}
	}
	if _, ok := m["node_id"]; !ok {
		t.Errorf("node_id missing: %s", data)
	}
}
