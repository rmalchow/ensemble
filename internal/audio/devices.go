package audio

import (
	"os"
	"os/exec"
	"strings"

	"ensemble/internal/contracts"
)

// procPCMPath is the kernel's ALSA PCM listing (overridable in tests).
var procPCMPath = "/proc/asound/pcm"

// ListInputDevices enumerates the host's capture devices for the UI to offer as
// a microphone (D48): for calibration and for playing an `input:` source. The
// IDs it returns are what the active capture tool selects, so enumeration and
// capture stay consistent on the same host:
//
//   - pw-record (PipeWire): PipeWire SOURCE node names via `pactl list sources`
//     (monitor sources excluded — they capture digital output, not a mic). The
//     name is passed to `pw-record --target`.
//   - arecord (bare ALSA): capture-capable PCMs from /proc/asound/pcm as
//     "hw:C,D", passed to `arecord -D`.
//
// A leading "" (system default) entry is always present. Empty when there is no
// capture backend at all.
func ListInputDevices() []contracts.InputDevice {
	bin := findCaptureBinary()
	if bin == "" {
		return nil
	}

	var devs []contracts.InputDevice
	if baseName(bin) == "pw-record" {
		devs = pipewireSources()
	}
	if len(devs) == 0 {
		devs = alsaCaptureDevices()
	}

	out := []contracts.InputDevice{{ID: "", Desc: "system default"}}
	return append(out, devs...)
}

// pipewireSources parses `pactl list sources` for non-monitor capture sources.
// Returns nil when pactl is unavailable or yields nothing (caller falls back).
func pipewireSources() []contracts.InputDevice {
	path, err := exec.LookPath("pactl")
	if err != nil {
		return nil
	}
	out, err := exec.Command(path, "list", "sources").Output()
	if err != nil {
		return nil
	}
	return parsePactlSources(string(out))
}

// parsePactlSources extracts {name, description} for each non-monitor source
// block. Pure, for testability. Blocks look like:
//
//	Source #46
//		Name: alsa_input.pci-0000_00_1f.3.analog-stereo
//		Description: Built-in Audio Analog Stereo
//		...
//		Monitor of Sink: n/a
func parsePactlSources(content string) []contracts.InputDevice {
	var devs []contracts.InputDevice
	var name, desc string
	monitor := false

	flush := func() {
		if name != "" && !monitor && !strings.HasSuffix(name, ".monitor") {
			d := desc
			if d == "" {
				d = name
			}
			devs = append(devs, contracts.InputDevice{ID: name, Desc: d})
		}
		name, desc, monitor = "", "", false
	}

	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "Source #"):
			flush()
		case strings.HasPrefix(t, "Name:"):
			name = strings.TrimSpace(strings.TrimPrefix(t, "Name:"))
		case strings.HasPrefix(t, "Description:"):
			desc = strings.TrimSpace(strings.TrimPrefix(t, "Description:"))
		case strings.HasPrefix(t, "Monitor of Sink:"):
			v := strings.TrimSpace(strings.TrimPrefix(t, "Monitor of Sink:"))
			if v != "" && v != "n/a" {
				monitor = true
			}
		}
	}
	flush()
	return devs
}

// alsaCaptureDevices parses /proc/asound/pcm for capture-capable PCMs as
// "hw:C,D" (mirrors sink.parseProcPCM but selects the capture marker).
func alsaCaptureDevices() []contracts.InputDevice {
	data, err := os.ReadFile(procPCMPath)
	if err != nil {
		return nil
	}
	var devs []contracts.InputDevice
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 2 {
			continue
		}
		head := strings.Fields(strings.TrimSpace(fields[0]))
		if len(head) == 0 {
			continue
		}
		cardDev := head[0]
		dash := strings.IndexByte(cardDev, '-')
		if dash <= 0 || dash >= len(cardDev)-1 {
			continue
		}
		card := strings.TrimLeft(cardDev[:dash], "0")
		if card == "" {
			card = "0"
		}
		dev := strings.TrimLeft(cardDev[dash+1:], "0")
		if dev == "" {
			dev = "0"
		}
		capture := false
		for _, f := range fields {
			if strings.Contains(strings.ToLower(f), "capture") {
				capture = true
				break
			}
		}
		if !capture {
			continue
		}
		desc := strings.TrimSpace(fields[1])
		if desc == "" {
			desc = cardDev
		}
		devs = append(devs, contracts.InputDevice{ID: "hw:" + card + "," + dev, Desc: desc})
	}
	return devs
}
