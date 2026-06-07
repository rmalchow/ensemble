package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ensemble/internal/id"
)

func writeFile(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, nodeFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write node.json: %v", err)
	}
}

func TestLoadOrCreateMintsIDOnMissing(t *testing.T) {
	dir := t.TempDir()
	nf, err := NewStore(dir).LoadOrCreate("")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if nf.ID.IsZero() {
		t.Fatal("minted id is zero")
	}
	if want := nf.ID.String()[:8]; nf.Name != want {
		t.Errorf("name = %q, want %q", nf.Name, want)
	}
	if nf.Volume != 1.0 {
		t.Errorf("volume = %v, want 1.0", nf.Volume)
	}
	if nf.OutputDelayMs != 0 {
		t.Errorf("outputDelayMs = %v, want 0", nf.OutputDelayMs)
	}
	if _, err := os.Stat(filepath.Join(dir, nodeFileName)); err != nil {
		t.Errorf("node.json not written: %v", err)
	}
}

func TestLoadOrCreateDefaultsVolumeAndDelayOnLegacyFile(t *testing.T) {
	dir := t.TempDir()
	nodeID := id.New()
	writeFile(t, dir, `{"id":"`+nodeID.String()+`","name":"x"}`)
	nf, err := NewStore(dir).LoadOrCreate("ignored")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if nf.Volume != 1.0 {
		t.Errorf("volume = %v, want 1.0", nf.Volume)
	}
	if nf.OutputDelayMs != 0 {
		t.Errorf("outputDelayMs = %v, want 0", nf.OutputDelayMs)
	}
}

func TestLoadOrCreateKeepsExplicitZeroVolume(t *testing.T) {
	dir := t.TempDir()
	nodeID := id.New()
	writeFile(t, dir, `{"id":"`+nodeID.String()+`","name":"x","volume":0}`)
	nf, err := NewStore(dir).LoadOrCreate("")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if nf.Volume != 0.0 {
		t.Errorf("volume = %v, want 0.0 (explicit, not defaulted)", nf.Volume)
	}
}

func TestLoadOrCreateClampsDelayOnLoad(t *testing.T) {
	dir := t.TempDir()
	nodeID := id.New()
	writeFile(t, dir, `{"id":"`+nodeID.String()+`","name":"x","outputDelayMs":9000}`)
	nf, err := NewStore(dir).LoadOrCreate("")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if nf.OutputDelayMs != 500 {
		t.Errorf("outputDelayMs = %v, want 500 (clamped)", nf.OutputDelayMs)
	}
}

func TestLoadOrCreateUsesInitialName(t *testing.T) {
	dir := t.TempDir()
	nf, err := NewStore(dir).LoadOrCreate("kitchen")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if nf.Name != "kitchen" {
		t.Errorf("name = %q, want kitchen", nf.Name)
	}
	if nf.ID.IsZero() {
		t.Error("id not minted")
	}
}

func TestLoadOrCreateLoadsExisting(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	first, err := s.LoadOrCreate("orig")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	second, err := s.LoadOrCreate("ignored")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("id changed: %v != %v", second.ID, first.ID)
	}
	if second.Name != "orig" {
		t.Errorf("name = %q, want orig (initialName ignored)", second.Name)
	}
}

func TestLoadOrCreateImmutableIDOnRestart(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	first, err := s.LoadOrCreate("a")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	second, err := s.LoadOrCreate("b")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("id changed on restart")
	}
	if second.Name != "a" {
		t.Errorf("name changed on restart: %q", second.Name)
	}
}

func TestLoadOrCreateCorruptFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "{garbage")
	_, err := NewStore(dir).LoadOrCreate("")
	if !errors.Is(err, ErrCorruptNodeFile) {
		t.Errorf("err = %v, want ErrCorruptNodeFile", err)
	}
}

func TestLoadOrCreateMissingID(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, `{"name":"x"}`)
	_, err := NewStore(dir).LoadOrCreate("")
	if !errors.Is(err, ErrCorruptNodeFile) {
		t.Errorf("err = %v, want ErrCorruptNodeFile", err)
	}
}

func TestLoadOrCreateBadIDHex(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, `{"id":"zz","name":"x"}`)
	_, err := NewStore(dir).LoadOrCreate("")
	if !errors.Is(err, ErrCorruptNodeFile) {
		t.Errorf("err = %v, want ErrCorruptNodeFile", err)
	}
}

func TestRenamePreservesID(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	orig, err := s.LoadOrCreate("")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Rename(orig.ID, "den"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	reloaded, err := s.LoadOrCreate("")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.ID != orig.ID {
		t.Error("id changed on rename")
	}
	if reloaded.Name != "den" {
		t.Errorf("name = %q, want den", reloaded.Name)
	}
}

func TestSetDisabledRoundTripAndNormalize(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	orig, err := s.LoadOrCreate("")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if orig.Disabled != nil {
		t.Errorf("default disabled = %v, want nil", orig.Disabled)
	}
	// Includes a bogus feature + a dup; normalize keeps only valid, deduped, sorted.
	nf, err := s.SetDisabled(orig.ID, []string{"opus", "bogus", "input", "opus"})
	if err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	if want := []string{"input", "opus"}; !equalStr(nf.Disabled, want) {
		t.Fatalf("disabled = %v, want %v", nf.Disabled, want)
	}
	reloaded, err := s.LoadOrCreate("")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !equalStr(reloaded.Disabled, []string{"input", "opus"}) {
		t.Errorf("reloaded disabled = %v", reloaded.Disabled)
	}
	// Clearing.
	nf, err = s.SetDisabled(orig.ID, nil)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if nf.Disabled != nil {
		t.Errorf("cleared disabled = %v, want nil", nf.Disabled)
	}
}

func equalStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRenameAtomicTrailingNewlineValidJSON(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	orig, _ := s.LoadOrCreate("")
	if _, err := s.Rename(orig.ID, "hall"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, nodeFileName))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Error("file does not end in newline")
	}
	var nf NodeFile
	if err := json.Unmarshal(data, &nf); err != nil {
		t.Errorf("file not valid JSON: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestRenameRejectsIDMismatch(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if _, err := s.LoadOrCreate("orig"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Rename(id.New(), "x"); !errors.Is(err, ErrIDImmutable) {
		t.Errorf("err = %v, want ErrIDImmutable", err)
	}
	reloaded, _ := s.LoadOrCreate("")
	if reloaded.Name != "orig" {
		t.Errorf("file changed despite mismatch: name = %q", reloaded.Name)
	}
}

func TestRenameOverwritesNotAppends(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	orig, _ := s.LoadOrCreate("")
	if _, err := s.Rename(orig.ID, "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rename(orig.ID, "two"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, nodeFileName))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	var first NodeFile
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := dec.Decode(new(NodeFile)); err == nil {
		t.Error("file contains more than one JSON object")
	}
	if first.Name != "two" {
		t.Errorf("name = %q, want two", first.Name)
	}
}

func TestRenamePreservesVolumeAndDelay(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	orig, _ := s.LoadOrCreate("")
	if _, err := s.SetVolume(orig.ID, 0.4); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetOutputDelayMs(orig.ID, 120); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rename(orig.ID, "x"); err != nil {
		t.Fatal(err)
	}
	nf, _ := s.LoadOrCreate("")
	if nf.Name != "x" || nf.Volume != 0.4 || nf.OutputDelayMs != 120 {
		t.Errorf("got %+v, want name x volume 0.4 delay 120", nf)
	}
}

func TestSetVolumePersistsAndPreservesOthers(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	orig, _ := s.LoadOrCreate("home")
	if _, err := s.SetVolume(orig.ID, 0.5); err != nil {
		t.Fatal(err)
	}
	nf, _ := s.LoadOrCreate("")
	if nf.Volume != 0.5 {
		t.Errorf("volume = %v, want 0.5", nf.Volume)
	}
	if nf.ID != orig.ID || nf.Name != "home" || nf.OutputDelayMs != 0 {
		t.Errorf("other fields changed: %+v", nf)
	}
}

func TestSetVolumeClamps(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	orig, _ := s.LoadOrCreate("")
	if nf, _ := s.SetVolume(orig.ID, 1.7); nf.Volume != 1.0 {
		t.Errorf("clamp high: %v, want 1.0", nf.Volume)
	}
	if nf, _ := s.SetVolume(orig.ID, -0.2); nf.Volume != 0.0 {
		t.Errorf("clamp low: %v, want 0.0", nf.Volume)
	}
}

func TestSetOutputDelayMsPersistsAndPreservesOthers(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	orig, _ := s.LoadOrCreate("home")
	if _, err := s.SetOutputDelayMs(orig.ID, -80); err != nil {
		t.Fatal(err)
	}
	nf, _ := s.LoadOrCreate("")
	if nf.OutputDelayMs != -80 {
		t.Errorf("delay = %v, want -80", nf.OutputDelayMs)
	}
	if nf.ID != orig.ID || nf.Name != "home" || nf.Volume != 1.0 {
		t.Errorf("other fields changed: %+v", nf)
	}
}

func TestSetOutputDelayMsClamps(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	orig, _ := s.LoadOrCreate("")
	if nf, _ := s.SetOutputDelayMs(orig.ID, 800); nf.OutputDelayMs != 500 {
		t.Errorf("clamp high: %v, want 500", nf.OutputDelayMs)
	}
	if nf, _ := s.SetOutputDelayMs(orig.ID, -800); nf.OutputDelayMs != -500 {
		t.Errorf("clamp low: %v, want -500", nf.OutputDelayMs)
	}
}

func TestSetVolumeRejectsIDMismatch(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	orig, _ := s.LoadOrCreate("")
	if _, err := s.SetVolume(id.New(), 0.3); !errors.Is(err, ErrIDImmutable) {
		t.Errorf("err = %v, want ErrIDImmutable", err)
	}
	nf, _ := s.LoadOrCreate("")
	if nf.Volume != orig.Volume {
		t.Error("volume changed despite id mismatch")
	}
}

func TestSetOutputDelayMsRejectsIDMismatch(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	orig, _ := s.LoadOrCreate("")
	if _, err := s.SetOutputDelayMs(id.New(), 50); !errors.Is(err, ErrIDImmutable) {
		t.Errorf("err = %v, want ErrIDImmutable", err)
	}
	nf, _ := s.LoadOrCreate("")
	if nf.OutputDelayMs != orig.OutputDelayMs {
		t.Error("delay changed despite id mismatch")
	}
}

func TestLoadOrCreateDefaultsOutputDeviceOnLegacyFile(t *testing.T) {
	dir := t.TempDir()
	nodeID := id.New()
	writeFile(t, dir, `{"id":"`+nodeID.String()+`","name":"x"}`)
	nf, err := NewStore(dir).LoadOrCreate("")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if nf.OutputDevice != "default" {
		t.Errorf("outputDevice = %q, want default", nf.OutputDevice)
	}
}

func TestLoadOrCreateKeepsExplicitOutputDevice(t *testing.T) {
	dir := t.TempDir()
	nodeID := id.New()
	writeFile(t, dir, `{"id":"`+nodeID.String()+`","name":"x","outputDevice":"hw:1,0"}`)
	nf, err := NewStore(dir).LoadOrCreate("")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if nf.OutputDevice != "hw:1,0" {
		t.Errorf("outputDevice = %q, want hw:1,0", nf.OutputDevice)
	}
}

func TestSetOutputDeviceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	nf, _ := s.LoadOrCreate("")
	got, err := s.SetOutputDevice(nf.ID, "hw:2,0")
	if err != nil {
		t.Fatalf("SetOutputDevice: %v", err)
	}
	if got.OutputDevice != "hw:2,0" {
		t.Errorf("outputDevice = %q, want hw:2,0", got.OutputDevice)
	}
	reread, _ := s.LoadOrCreate("")
	if reread.OutputDevice != "hw:2,0" {
		t.Errorf("persisted outputDevice = %q, want hw:2,0", reread.OutputDevice)
	}
}

func TestSetOutputDeviceBlankNormalizesToDefault(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	nf, _ := s.LoadOrCreate("")
	got, err := s.SetOutputDevice(nf.ID, "   ")
	if err != nil {
		t.Fatalf("SetOutputDevice: %v", err)
	}
	if got.OutputDevice != "default" {
		t.Errorf("outputDevice = %q, want default", got.OutputDevice)
	}
}

func TestSetOutputDeviceRejectsIDMismatch(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	orig, _ := s.LoadOrCreate("")
	if _, err := s.SetOutputDevice(id.New(), "hw:1,0"); !errors.Is(err, ErrIDImmutable) {
		t.Errorf("err = %v, want ErrIDImmutable", err)
	}
	nf, _ := s.LoadOrCreate("")
	if nf.OutputDevice != orig.OutputDevice {
		t.Error("device changed despite id mismatch")
	}
}
