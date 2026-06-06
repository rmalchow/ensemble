package group

import (
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

func node(id string, render bool, dec, fec []string, maxRate int, enc []string) state.NodeRecord {
	return state.NodeRecord{
		ID: id,
		Caps: state.Capabilities{
			Render:       render,
			DecodeCodecs: dec,
			EncodeCodecs: enc,
			FEC:          fec,
			MaxRate:      maxRate,
		},
	}
}

func TestNegotiateProfile(t *testing.T) {
	// doc 04 §4.3.2 worked example nodes.
	n1Master := node("n1", false, nil, nil, 0, []string{"pcm", "opus"}) // sink-less NAS master
	n2 := node("n2", true, []string{"pcm", "opus"}, []string{"xorParity"}, 48000, []string{"pcm"})
	n3 := node("n3", true, []string{"pcm", "opus"}, []string{"xorParity", "duplicate"}, 48000, []string{"pcm"})
	n4 := node("n4", true, []string{"pcm"}, []string{"duplicate"}, 48000, []string{"pcm"}) // dumb

	tests := []struct {
		name    string
		members []state.NodeRecord
		master  state.NodeRecord
		want    Profile
	}{
		{
			name:    "04 §4.3.2 with dumb N4 ⇒ pcm/duplicate",
			members: []state.NodeRecord{n1Master, n2, n3, n4},
			master:  n1Master,
			want:    Profile{Codec: "pcm", FEC: "duplicate", Rate: 48000, FramesPerChunk: 480},
		},
		{
			name:    "04 §4.3.2 drop N4 ⇒ opus/xorParity",
			members: []state.NodeRecord{n1Master, n2, n3},
			master:  n1Master,
			want:    Profile{Codec: "opus", FEC: "xorParity", Rate: 48000, FramesPerChunk: 480},
		},
		{
			name:    "PCM floor for full-node listeners",
			members: []state.NodeRecord{n2},
			master:  node("m", false, nil, nil, 0, []string{"pcm", "opus"}),
			want:    Profile{Codec: "opus", FEC: "xorParity", Rate: 48000, FramesPerChunk: 480},
		},
		{
			name: "master EncodeCodecs ceiling drops opus even if all listeners decode it",
			members: []state.NodeRecord{
				node("a", true, []string{"pcm", "opus"}, []string{"xorParity"}, 48000, nil),
			},
			master: node("m", false, nil, nil, 0, []string{"pcm"}), // master cannot encode opus
			want:   Profile{Codec: "pcm", FEC: "xorParity", Rate: 48000, FramesPerChunk: 480},
		},
		{
			name:    "zero-listener group ⇒ default bounded by master encode (§4.3.4)",
			members: []state.NodeRecord{n1Master}, // sink-less only, no listeners
			master:  n1Master,
			want:    Profile{Codec: "opus", FEC: "none", Rate: 48000, FramesPerChunk: 480},
		},
		{
			name: "min(MaxRate) over listeners",
			members: []state.NodeRecord{
				node("a", true, []string{"pcm"}, []string{"none"}, 48000, nil),
				node("b", true, []string{"pcm"}, []string{"none"}, 44100, nil),
			},
			master: node("m", true, []string{"pcm"}, []string{"none"}, 48000, []string{"pcm"}),
			want:   Profile{Codec: "pcm", FEC: "duplicate", Rate: 44100, FramesPerChunk: 480},
		},
		{
			name: "FEC rank xorParity > duplicate > none",
			members: []state.NodeRecord{
				node("a", true, []string{"pcm"}, []string{"xorParity", "duplicate", "none"}, 48000, nil),
				node("b", true, []string{"pcm"}, []string{"xorParity", "none"}, 48000, nil),
			},
			master: node("m", true, []string{"pcm"}, nil, 48000, []string{"pcm"}),
			want:   Profile{Codec: "pcm", FEC: "xorParity", Rate: 48000, FramesPerChunk: 480},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NegotiateProfile(tc.members, tc.master)
			if got != tc.want {
				t.Errorf("NegotiateProfile = %+v, want %+v", got, tc.want)
			}
			if got.Codec == "" {
				t.Errorf("codec must never be empty (PCM floor)")
			}
			if got.FramesPerChunk != 480 {
				t.Errorf("FramesPerChunk = %d, want 480", got.FramesPerChunk)
			}
		})
	}
}

// fullNodeCaps mirrors what caps.Probe writes for a full (render-capable) node:
// PCM always present, "opus" in BOTH Encode/DecodeCodecs iff the runtime prober
// reported it available (P5.2 §4.4, §5.4.1). withOpus models the two prober
// outcomes — present (opus-tagged build + libopus) vs absent (default build).
func fullNodeCaps(id string, withOpus bool) state.NodeRecord {
	enc, dec := []string{"pcm"}, []string{"pcm"}
	if withOpus {
		enc = append(enc, "opus")
		dec = append(dec, "opus")
	}
	return state.NodeRecord{ID: id, Caps: state.Capabilities{
		Render: true, EncodeCodecs: enc, DecodeCodecs: dec,
		FEC: []string{"xorParity"}, MaxRate: 48000,
	}}
}

// TestNegotiationOpusAvailabilityDrivesCodec is the P5.2 §7.4 integration
// realization of doc 04 §4.3.2: negotiation selects "opus" only when every
// listener's prober reported it (and the master can encode it), and falls to
// the PCM floor otherwise — graceful absence is automatic, no special-casing.
func TestNegotiationOpusAvailabilityDrivesCodec(t *testing.T) {
	master := fullNodeCaps("m", true)
	tests := []struct {
		name      string
		listeners []state.NodeRecord
		wantCodec string
	}{
		{
			name:      "all listeners advertise opus ⇒ opus",
			listeners: []state.NodeRecord{fullNodeCaps("a", true), fullNodeCaps("b", true)},
			wantCodec: "opus",
		},
		{
			name:      "one listener lacks opus (default build / no libopus) ⇒ pcm floor",
			listeners: []state.NodeRecord{fullNodeCaps("a", true), fullNodeCaps("b", false)},
			wantCodec: "pcm",
		},
		{
			name:      "no node advertises opus (default build) ⇒ pcm floor",
			listeners: []state.NodeRecord{fullNodeCaps("a", false), fullNodeCaps("b", false)},
			wantCodec: "pcm",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			members := append([]state.NodeRecord{master}, tc.listeners...)
			if got := NegotiateProfile(members, master).Codec; got != tc.wantCodec {
				t.Errorf("Codec = %q, want %q", got, tc.wantCodec)
			}
		})
	}
}

// TestCodecFlipBumpsStreamGen ties the negotiation outcome to the master's
// streamGen: when a dumb node joins and flips opus→pcm, Recompute must bump
// StreamGen so the origin keyframes the new generation (doc 04 §4.3.3, doc 05
// §5.8). This is the negotiation→generation consequence P5.2 §5.4.3 guarantees.
func TestCodecFlipBumpsStreamGen(t *testing.T) {
	master := fullNodeCaps("m", true)
	opusMembers := []state.NodeRecord{master, fullNodeCaps("a", true)}
	pcmMembers := []state.NodeRecord{master, fullNodeCaps("a", true), fullNodeCaps("dumb", false)}

	opusProfile := NegotiateProfile(opusMembers, master)
	pcmProfile := NegotiateProfile(pcmMembers, master)
	if opusProfile.Codec != "opus" || pcmProfile.Codec != "pcm" {
		t.Fatalf("setup: opus=%q pcm=%q", opusProfile.Codec, pcmProfile.Codec)
	}

	in := Inputs{
		SelfID: "m", MasterID: "m", MyCaps: master.Caps,
		Members: opusMembers, Generation: 1, Profile: opusProfile,
	}
	d0 := Recompute(Decision{}, in) // become master @ opus ⇒ streamGen 1
	if d0.StreamGen != 1 {
		t.Fatalf("initial streamGen = %d, want 1", d0.StreamGen)
	}
	// Dumb node joins, negotiation flips to pcm ⇒ new profile ⇒ streamGen bump.
	in.Members, in.Profile = pcmMembers, pcmProfile
	d1 := Recompute(d0, in)
	if d1.StreamGen != 2 {
		t.Errorf("after opus→pcm flip streamGen = %d, want 2", d1.StreamGen)
	}
}
