package audio

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListPlaybackDevices(t *testing.T) {
	dir := t.TempDir()
	pcm := filepath.Join(dir, "pcm")
	if err := os.WriteFile(pcm, []byte(
		"00-03: HDMI 0 : HDMI 0 : playback 1\n"+
			"01-00: ALC295 Analog : ALC295 Analog : playback 1 : capture 1\n"+
			"01-01: ALC295 Digital : ALC295 Digital : capture 1\n"+ // capture-only: skipped
			"garbage line\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}

	got := listPlaybackDevices(pcm, filepath.Join(dir, "nosnd"))
	if len(got) != 2 {
		t.Fatalf("devices = %+v, want 2 playback entries", got)
	}
	if got[0].ID != "hw:0,3" || got[0].Label != "HDMI 0" {
		t.Fatalf("first = %+v, want hw:0,3 / HDMI 0", got[0])
	}
	if got[1].ID != "hw:1,0" || got[1].Label != "ALC295 Analog" {
		t.Fatalf("second = %+v, want hw:1,0 / ALC295 Analog", got[1])
	}

	// Fallback: no pcm file, scan /dev/snd-style nodes (playback 'p' only).
	snd := filepath.Join(dir, "snd")
	if err := os.MkdirAll(snd, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"pcmC0D3p", "pcmC1D0p", "pcmC1D0c", "controlC0"} {
		if err := os.WriteFile(filepath.Join(snd, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got = listPlaybackDevices(filepath.Join(dir, "missing"), snd)
	if len(got) != 2 || got[0].ID != "hw:0,3" || got[1].ID != "hw:1,0" {
		t.Fatalf("fallback devices = %+v, want [hw:0,3 hw:1,0]", got)
	}
}
