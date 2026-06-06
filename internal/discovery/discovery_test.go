package discovery

import (
	"reflect"
	"testing"
)

// TestTXTRoundTrip: a fully-initialized Announce encodes via txtRecords and
// decodes via parseTXT/nodeFromTXT preserving all nine keys, with Initialized==true
// and the ports parsed as ints (doc 02 §2.2).
func TestTXTRoundTrip(t *testing.T) {
	a := Announce{
		NodeID:      "node-1",
		Name:        "livingroom",
		ClusterFP:   "cf-abc123",
		GroupID:     "grp-kitchen",
		Initialized: true,
		ControlPort: 8443,
		ClockPort:   9000,
		AudioPort:   9100,
		WebPort:     8080,
	}
	got := nodeFromTXT(parseTXT(txtRecords(a)), "192.168.1.5", 7946)
	want := DiscoveredNode{
		NodeID:      "node-1",
		Name:        "livingroom",
		ClusterFP:   "cf-abc123",
		GroupID:     "grp-kitchen",
		Initialized: true,
		Addr:        "192.168.1.5",
		Port:        7946,
		ControlPort: 8443,
		ClockPort:   9000,
		AudioPort:   9100,
		WebPort:     8080,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", got, want)
	}
}

// TestTXTUninitialized: an uninitialized Announce (Initialized:false, ClusterFP:"")
// round-trips to a DiscoveredNode with Initialized==false and ClusterFP=="" — the
// adoption hook (doc 02 §2.4).
func TestTXTUninitialized(t *testing.T) {
	a := Announce{NodeID: "raw-node", Name: "freshpi", Initialized: false}
	got := nodeFromTXT(parseTXT(txtRecords(a)), "10.0.0.9", 7946)
	if got.Initialized {
		t.Errorf("Initialized = true, want false")
	}
	if got.ClusterFP != "" {
		t.Errorf("ClusterFP = %q, want empty", got.ClusterFP)
	}
	if got.NodeID != "raw-node" || got.Name != "freshpi" {
		t.Errorf("id/name = %q/%q", got.NodeID, got.Name)
	}
}

// TestTXTMissingPorts: absent/empty ctrl/clk/aud/wp TXT values decode to 0
// (robust parse, mirrors mpvsync TestTXTEmptyWebPort).
func TestTXTMissingPorts(t *testing.T) {
	got := nodeFromTXT(parseTXT([]string{"id=n", "name=n", "cf=", "gid=", "init=0"}), "", 0)
	if got.ControlPort != 0 || got.ClockPort != 0 || got.AudioPort != 0 || got.WebPort != 0 {
		t.Fatalf("ports = ctrl %d clk %d aud %d wp %d, want all 0",
			got.ControlPort, got.ClockPort, got.AudioPort, got.WebPort)
	}
}

// TestServiceType pins the DNS-SD service type and domain (doc 02 §2.2).
func TestServiceType(t *testing.T) {
	if mdnsService != "_ensemble._udp" {
		t.Errorf("mdnsService = %q, want _ensemble._udp", mdnsService)
	}
	if mdnsDomain != "local." {
		t.Errorf("mdnsDomain = %q, want local.", mdnsDomain)
	}
}

// TestClassify covers each of the five states plus the precedence rules
// (doc 02 §2.4 table). Each row asserts exactly one Device per id and its State.
func TestClassify(t *testing.T) {
	const ourCF = "cf-ours"

	tests := []struct {
		name string
		in   ClassifyInput
		want map[string]NodeState // id -> expected state
	}{
		{
			name: "present: alive known member",
			in: ClassifyInput{
				OurClusterFP: ourCF,
				AliveIDs:     map[string]bool{"a": true},
				KnownIDs:     map[string]bool{"a": true},
			},
			want: map[string]NodeState{"a": StatePresent},
		},
		{
			name: "offline: known member not alive",
			in: ClassifyInput{
				OurClusterFP: ourCF,
				AliveIDs:     map[string]bool{},
				KnownIDs:     map[string]bool{"a": true},
			},
			want: map[string]NodeState{"a": StateOffline},
		},
		{
			name: "uninitialized: survey init=0 / cf empty",
			in: ClassifyInput{
				OurClusterFP: ourCF,
				Discovered:   []DiscoveredNode{{NodeID: "u", Initialized: false, ClusterFP: ""}},
			},
			want: map[string]NodeState{"u": StateUninitialized},
		},
		{
			name: "foreign: survey cf set and not ours",
			in: ClassifyInput{
				OurClusterFP: ourCF,
				Discovered:   []DiscoveredNode{{NodeID: "f", Initialized: true, ClusterFP: "cf-other"}},
			},
			want: map[string]NodeState{"f": StateForeign},
		},
		{
			name: "discovered: survey cf == ours, not yet alive/known",
			in: ClassifyInput{
				OurClusterFP: ourCF,
				Discovered:   []DiscoveredNode{{NodeID: "d", Initialized: true, ClusterFP: ourCF}},
			},
			want: map[string]NodeState{"d": StateDiscovered},
		},
		{
			name: "precedence: alive AND in survey -> present, survey suppressed",
			in: ClassifyInput{
				OurClusterFP: ourCF,
				AliveIDs:     map[string]bool{"a": true},
				KnownIDs:     map[string]bool{"a": true},
				Discovered:   []DiscoveredNode{{NodeID: "a", Initialized: true, ClusterFP: ourCF}},
			},
			want: map[string]NodeState{"a": StatePresent},
		},
		{
			name: "precedence: known-offline id also in survey same cf -> offline not discovered",
			in: ClassifyInput{
				OurClusterFP: ourCF,
				AliveIDs:     map[string]bool{},
				KnownIDs:     map[string]bool{"a": true},
				Discovered:   []DiscoveredNode{{NodeID: "a", Initialized: true, ClusterFP: ourCF}},
			},
			want: map[string]NodeState{"a": StateOffline},
		},
		{
			name: "self-cluster uninitialized observer: foreign stays foreign, uninit stays uninit",
			in: ClassifyInput{
				OurClusterFP: "",
				Discovered: []DiscoveredNode{
					{NodeID: "f", Initialized: true, ClusterFP: "cf-other"},
					{NodeID: "u", Initialized: false, ClusterFP: ""},
				},
			},
			want: map[string]NodeState{"f": StateForeign, "u": StateUninitialized},
		},
		{
			name: "freshly joined: alive but not yet known, advertised our cf -> present",
			in: ClassifyInput{
				OurClusterFP: ourCF,
				AliveIDs:     map[string]bool{"j": true},
				Discovered:   []DiscoveredNode{{NodeID: "j", Initialized: true, ClusterFP: ourCF}},
			},
			want: map[string]NodeState{"j": StatePresent},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d devices, want %d: %+v", len(got), len(tt.want), got)
			}
			seen := map[string]bool{}
			for _, dv := range got {
				if seen[dv.NodeID] {
					t.Fatalf("duplicate device for id %q", dv.NodeID)
				}
				seen[dv.NodeID] = true
				want, ok := tt.want[dv.NodeID]
				if !ok {
					t.Fatalf("unexpected device id %q (state %q)", dv.NodeID, dv.State)
				}
				if dv.State != want {
					t.Errorf("id %q: state = %q, want %q", dv.NodeID, dv.State, want)
				}
			}
		})
	}
}

// TestClassifyAttachesSurveyRecord verifies a survey-derived row carries the
// DiscoveredNode pointer while an offline known-but-not-advertised member does not.
func TestClassifyAttachesSurveyRecord(t *testing.T) {
	in := ClassifyInput{
		OurClusterFP: "cf-ours",
		KnownIDs:     map[string]bool{"off": true},
		Discovered:   []DiscoveredNode{{NodeID: "d", Initialized: true, ClusterFP: "cf-ours", Addr: "1.2.3.4"}},
	}
	got := Classify(in)
	byID := map[string]Device{}
	for _, dv := range got {
		byID[dv.NodeID] = dv
	}
	if byID["off"].Node != nil {
		t.Errorf("offline member should have nil Node, got %+v", byID["off"].Node)
	}
	if byID["d"].Node == nil || byID["d"].Node.Addr != "1.2.3.4" {
		t.Errorf("discovered member should carry survey record, got %+v", byID["d"].Node)
	}
}
