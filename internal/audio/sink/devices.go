package audio

// Playback-device enumeration (06 §1.1): the per-node list of selectable output
// devices the UI offers. Sourced from /proc/asound/pcm (which carries the human
// stream name for a label), falling back to a /dev/snd/pcmC*D*p scan. The
// resulting "hw:<card>,<dev>" strings are accepted by the alsa backend AND the
// exec players (aplay -D / pw-play's ALSA targets), so one list serves every
// backend. Pure file reads — no device is opened, so enumeration is safe to run
// periodically even while rendering. Empty on a host with no ALSA cards.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Device is one enumerated playback device choice. ID is the string Open
// accepts ("hw:1,0"); Label is the human stream name when known ("ALC295
// Analog").
type Device struct {
	ID    string
	Label string
}

// ListPlaybackDevices enumerates this host's playback PCMs, headed by the
// shared "default" PCM when the libasound (alsalib) tier is usable — the choice
// that routes through dmix/PipeWire/Pulse and coexists with a desktop session.
func ListPlaybackDevices() []Device {
	out := listPlaybackDevices("/proc/asound/pcm", "/dev/snd")
	if probeAlsaLib("") {
		out = append([]Device{{ID: "default", Label: "system default (shared)"}}, out...)
	}
	return out
}

// listPlaybackDevices is the path-injected core (unit-tested with fixtures).
func listPlaybackDevices(pcmPath, devSnd string) []Device {
	var out []Device
	if b, err := os.ReadFile(pcmPath); err == nil {
		// Lines look like: "01-00: ALC295 Analog : ALC295 Analog : playback 1 : capture 1".
		// Keep playback-capable streams; card-dev from the "CC-DD:" prefix, the
		// label from the first name field.
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.Contains(line, "playback") {
				continue
			}
			parts := strings.SplitN(line, ":", 3)
			if len(parts) < 2 {
				continue
			}
			cd := strings.SplitN(strings.TrimSpace(parts[0]), "-", 2)
			if len(cd) != 2 {
				continue
			}
			card, err1 := strconv.Atoi(cd[0])
			dev, err2 := strconv.Atoi(cd[1])
			if err1 != nil || err2 != nil {
				continue
			}
			out = append(out, Device{
				ID:    fmt.Sprintf("hw:%d,%d", card, dev),
				Label: strings.TrimSpace(parts[1]),
			})
		}
	}
	if len(out) > 0 {
		return out
	}
	// Fallback: device nodes only (no labels): /dev/snd/pcmC<card>D<dev>p.
	entries, err := os.ReadDir(devSnd)
	if err != nil {
		return out
	}
	for _, e := range entries {
		var card, dev int
		var dir rune
		if n, _ := fmt.Sscanf(e.Name(), "pcmC%dD%d%c", &card, &dev, &dir); n == 3 && dir == 'p' {
			out = append(out, Device{ID: fmt.Sprintf("hw:%d,%d", card, dev)})
		}
	}
	return out
}
