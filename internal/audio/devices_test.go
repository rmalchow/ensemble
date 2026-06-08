package audio

import (
	"os"
	"testing"
)

func TestParsePactlSourcesExcludesMonitors(t *testing.T) {
	const out = `Source #46
	State: SUSPENDED
	Name: alsa_input.pci-0000_00_1f.3.analog-stereo
	Description: Built-in Audio Analog Stereo
	Monitor of Sink: n/a
Source #47
	State: RUNNING
	Name: alsa_output.pci-0000_00_1f.3.analog-stereo.monitor
	Description: Monitor of Built-in Audio
	Monitor of Sink: alsa_output.pci-0000_00_1f.3.analog-stereo
Source #48
	Name: alsa_input.usb-Blue_Yeti.analog-stereo
	Description: Yeti Stereo Microphone
	Monitor of Sink: n/a
`
	devs := parsePactlSources(out)
	if len(devs) != 2 {
		t.Fatalf("got %d devices, want 2 (monitors excluded): %+v", len(devs), devs)
	}
	if devs[0].ID != "alsa_input.pci-0000_00_1f.3.analog-stereo" || devs[0].Desc != "Built-in Audio Analog Stereo" {
		t.Errorf("dev0 = %+v", devs[0])
	}
	if devs[1].Desc != "Yeti Stereo Microphone" {
		t.Errorf("dev1 = %+v", devs[1])
	}
}

func TestParsePactlSourcesEmpty(t *testing.T) {
	if devs := parsePactlSources(""); len(devs) != 0 {
		t.Errorf("want none, got %+v", devs)
	}
}

func TestAlsaCaptureDevices(t *testing.T) {
	dir := t.TempDir()
	f := dir + "/pcm"
	const pcm = `00-00: ALC892 Analog : ALC892 Analog : playback 1 : capture 1
01-03: HDMI 0 : HDMI 0 : playback 1
02-00: USB Mic : USB Mic : capture 1
`
	if err := os.WriteFile(f, []byte(pcm), 0o644); err != nil {
		t.Fatal(err)
	}
	old := procPCMPath
	procPCMPath = f
	defer func() { procPCMPath = old }()

	devs := alsaCaptureDevices()
	if len(devs) != 2 { // the two capture-capable cards; the HDMI playback-only is skipped
		t.Fatalf("got %d, want 2: %+v", len(devs), devs)
	}
	if devs[0].ID != "hw:0,0" {
		t.Errorf("dev0 ID = %q, want hw:0,0", devs[0].ID)
	}
	if devs[1].ID != "hw:2,0" {
		t.Errorf("dev1 ID = %q, want hw:2,0", devs[1].ID)
	}
}
