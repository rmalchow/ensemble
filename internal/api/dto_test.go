package api

import (
	"encoding/json"
	"testing"

	"ensemble/internal/contracts"
)

// TestStatusRespJSONGolden pins the D19 envelope key names exactly.
func TestStatusRespJSONGolden(t *testing.T) {
	r := StatusResp{
		ID:      "0123456789abcdef0123456789abcdef",
		Name:    "alice",
		Role:    "solo",
		GroupID: "0123456789abcdef0123456789abcdef",
		Ports:   PortsResp{HTTP: 8080, Stream: 9090, Source: 9200, Gossip: 7946},
		Sink: sinkStatsResp(contracts.SinkStats{
			Played: 1, Silence: 2, LateDrop: 3, StaleGen: 4,
			Synced: true, RatePPM: 5.5, Buffered: 6,
		}),
		Clock: ClockStat{Synced: true, OffsetNs: 7, RTTNs: 8},
	}
	b, _ := json.Marshal(r)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}

	for _, k := range []string{"id", "name", "role", "groupId", "ports", "sink", "clock"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing top-level key %q in %s", k, b)
		}
	}
	if _, ok := m["source"]; ok {
		t.Errorf("source must be omitted when nil: %s", b)
	}

	ports := m["ports"].(map[string]any)
	for _, k := range []string{"http", "stream", "source", "gossip"} {
		if _, ok := ports[k]; !ok {
			t.Errorf("missing ports.%s", k)
		}
	}
	sink := m["sink"].(map[string]any)
	for _, k := range []string{"played", "silence", "lateDrop", "staleGen", "synced", "ratePPM", "buffered"} {
		if _, ok := sink[k]; !ok {
			t.Errorf("missing sink.%s (got %v)", k, sink)
		}
	}
	clock := m["clock"].(map[string]any)
	for _, k := range []string{"synced", "offsetNs", "rttNs"} {
		if _, ok := clock[k]; !ok {
			t.Errorf("missing clock.%s", k)
		}
	}
}

// TestStatusSourcePresent verifies source appears when set.
func TestStatusSourcePresent(t *testing.T) {
	r := StatusResp{Source: &contracts.SourceStats{Clients: 1}}
	b, _ := json.Marshal(r)
	var m map[string]any
	json.Unmarshal(b, &m)
	src, ok := m["source"].(map[string]any)
	if !ok {
		t.Fatalf("source missing: %s", b)
	}
	for _, k := range []string{"clients", "connects", "restarts", "primes"} {
		if _, ok := src[k]; !ok {
			t.Errorf("missing source.%s", k)
		}
	}
}

func TestErrorRespOmitsEmptyHint(t *testing.T) {
	b, _ := json.Marshal(ErrorResp{Error: "x"})
	if string(b) != `{"error":"x"}` {
		t.Errorf("hint not omitted: %s", b)
	}
	b2, _ := json.Marshal(ErrorResp{Error: "x", Hint: "y"})
	if string(b2) != `{"error":"x","hint":"y"}` {
		t.Errorf("hint shape: %s", b2)
	}
}

func TestNodePatchReqOptionalFields(t *testing.T) {
	var empty NodePatchReq
	if err := json.Unmarshal([]byte(`{}`), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Name != nil || empty.Volume != nil || empty.OutputDelayMs != nil {
		t.Errorf("empty object should leave all pointers nil")
	}

	var zeroVol NodePatchReq
	if err := json.Unmarshal([]byte(`{"volume":0}`), &zeroVol); err != nil {
		t.Fatal(err)
	}
	if zeroVol.Volume == nil || *zeroVol.Volume != 0 {
		t.Errorf("explicit zero volume must be a non-nil 0 pointer (D35)")
	}
}

// TestSnapshotJSONTagsStable re-asserts the contracts tags the SPA codes against.
func TestSnapshotJSONTagsStable(t *testing.T) {
	snap := contracts.Snapshot{
		Nodes: []contracts.NodeView{{Volume: 0.5, OutputDelayMs: 10}},
	}
	b, _ := json.Marshal(snap)
	var m map[string]any
	json.Unmarshal(b, &m)
	if _, ok := m["nodes"]; !ok {
		t.Error("snapshot.nodes tag")
	}
	if _, ok := m["groups"]; !ok {
		t.Error("snapshot.groups tag")
	}
	node := m["nodes"].([]any)[0].(map[string]any)
	for _, k := range []string{"id", "name", "volume", "outputDelayMs"} {
		if _, ok := node[k]; !ok {
			t.Errorf("missing node.%s", k)
		}
	}
}
