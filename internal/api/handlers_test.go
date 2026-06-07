package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// snapWith builds a snapshot where self is in a group with the given master and
// members.
func snapWith(self, master id.ID, members []id.ID, name string) contracts.Snapshot {
	nodes := make([]contracts.NodeView, 0, len(members))
	for _, m := range members {
		nodes = append(nodes, contracts.NodeView{ID: m, Name: name, Alive: true, HTTPPort: 8080})
	}
	gid := id.XOR(members...)
	return contracts.Snapshot{
		Nodes: nodes,
		Groups: []contracts.GroupView{{
			ID: gid, Master: master, Members: members,
		}},
	}
}

func TestStatusShape(t *testing.T) {
	self := id.New()
	cfg, fc, _ := baseConfig(self)
	fc.setSnapshot(snapWith(self, self, []id.ID{self}, "alice"))
	cfg.Stats = func() StatusStats {
		return StatusStats{
			Sink:  contracts.SinkStats{Played: 5, RatePPM: 12.5, Buffered: 3, Synced: true},
			Clock: ClockStat{Synced: true, OffsetNs: 100, RTTNs: 200},
		}
	}
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodGet, "/api/status", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var got StatusResp
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()

	if got.ID != self.String() {
		t.Errorf("id = %q", got.ID)
	}
	if got.Name != "alice" {
		t.Errorf("name = %q", got.Name)
	}
	if got.Ports.HTTP != 8080 || got.Ports.Stream != 9090 || got.Ports.Source != 9200 || got.Ports.Gossip != 7946 {
		t.Errorf("ports = %+v", got.Ports)
	}
	if got.Sink.RatePPM != 12.5 || got.Sink.Buffered != 3 {
		t.Errorf("sink = %+v", got.Sink)
	}
	if !got.Clock.Synced || got.Clock.OffsetNs != 100 {
		t.Errorf("clock = %+v", got.Clock)
	}
	if got.Source != nil {
		t.Errorf("source should be nil, got %+v", got.Source)
	}
}

func TestStatusSourceOnlyWhenActive(t *testing.T) {
	self := id.New()
	cfg, fc, _ := baseConfig(self)
	fc.setSnapshot(snapWith(self, self, []id.ID{self}, "n"))
	cfg.Stats = func() StatusStats {
		return StatusStats{Source: &contracts.SourceStats{Clients: 2, Connects: 4}}
	}
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodGet, "/api/status", nil)
	b := readBody(t, resp)
	var raw map[string]json.RawMessage
	json.Unmarshal(b, &raw)
	if _, ok := raw["source"]; !ok {
		t.Fatalf("source absent when active: %s", b)
	}
}

func TestStatusRoles(t *testing.T) {
	self := id.New()
	other := id.New()

	cases := []struct {
		name     string
		snap     contracts.Snapshot
		wantRole string
	}{
		{"solo", snapWith(self, self, []id.ID{self}, "n"), "solo"},
		{"master", snapWith(self, self, []id.ID{self, other}, "n"), "master"},
		{"follower", snapWith(self, other, []id.ID{self, other}, "n"), "follower"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, fc, _ := baseConfig(self)
			fc.setSnapshot(tc.snap)
			_, ts := testServer(t, cfg)
			resp := doJSON(t, ts, http.MethodGet, "/api/status", nil)
			var got StatusResp
			json.NewDecoder(resp.Body).Decode(&got)
			resp.Body.Close()
			if got.Role != tc.wantRole {
				t.Errorf("role = %q, want %q", got.Role, tc.wantRole)
			}
		})
	}
}

func TestClusterReturnsSnapshotVerbatim(t *testing.T) {
	self := id.New()
	cfg, fc, _ := baseConfig(self)
	snap := snapWith(self, self, []id.ID{self}, "alice")
	fc.setSnapshot(snap)
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodGet, "/api/cluster", nil)
	got := readBody(t, resp)
	want, _ := json.Marshal(snap)
	if string(got) != string(want)+"\n" && string(got) != string(want) {
		// Echo's JSON may append a newline; compare structurally.
		var a, b contracts.Snapshot
		json.Unmarshal(got, &a)
		json.Unmarshal(want, &b)
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		if string(ja) != string(jb) {
			t.Errorf("cluster body mismatch\n got=%s\nwant=%s", got, want)
		}
	}
}

func TestMediaList(t *testing.T) {
	self := id.New()
	cfg, _, _ := baseConfig(self)
	cfg.Media = &fakeMedia{files: []MediaFile{{Path: "a.flac", Name: "a.flac", SizeBytes: 10}}}
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodGet, "/api/media", nil)
	var got []MediaFile
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if len(got) != 1 || got[0].Path != "a.flac" {
		t.Errorf("media = %+v", got)
	}
}

func TestRenameNode(t *testing.T) {
	self := id.New()
	cfg, fc, _ := baseConfig(self)
	nc := &fakeNodeConfig{}
	cfg.NodeCfg = nc
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPatch, "/api/node", map[string]any{"name": "bob"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(nc.names) != 1 || nc.names[0] != "bob" {
		t.Errorf("config rename = %v", nc.names)
	}
	if len(fc.setName) != 1 || fc.setName[0] != "bob" {
		t.Errorf("cluster setName = %v", fc.setName)
	}
}

func TestPatchNodeDisabled(t *testing.T) {
	self := id.New()
	cfg, fc, _ := baseConfig(self)
	nc := &fakeNodeConfig{}
	cfg.NodeCfg = nc
	var applied [][]string
	cfg.ApplyDisabled = func(d []string) { applied = append(applied, d) }
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPatch, "/api/node", map[string]any{"disabled": []string{"playback", "opus"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(nc.disabled) != 1 || len(nc.disabled[0]) != 2 {
		t.Errorf("cfg disabled = %v", nc.disabled)
	}
	if len(fc.setDisabled) != 1 {
		t.Errorf("cluster setDisabled = %v", fc.setDisabled)
	}
	if len(applied) != 1 {
		t.Errorf("ApplyDisabled not called: %v", applied)
	}
}

func TestPatchNodeDisabledRejectsBadFeature(t *testing.T) {
	self := id.New()
	cfg, fc, _ := baseConfig(self)
	nc := &fakeNodeConfig{}
	cfg.NodeCfg = nc
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPatch, "/api/node", map[string]any{"disabled": []string{"playback", "bogus"}})
	e := decodeErr(t, resp)
	if resp.StatusCode != 400 || e.Error != "bad_disabled" {
		t.Fatalf("status=%d err=%q", resp.StatusCode, e.Error)
	}
	if len(nc.disabled) != 0 || len(fc.setDisabled) != 0 {
		t.Errorf("nothing should be persisted/replicated on invalid feature")
	}
}

func TestRenameEmptyName(t *testing.T) {
	self := id.New()
	cfg, fc, _ := baseConfig(self)
	nc := &fakeNodeConfig{}
	cfg.NodeCfg = nc
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPatch, "/api/node", map[string]any{"name": "   "})
	e := decodeErr(t, resp)
	if resp.StatusCode != 400 || e.Error != "empty_name" {
		t.Fatalf("status=%d err=%q", resp.StatusCode, e.Error)
	}
	if len(nc.names) != 0 || len(fc.setName) != 0 {
		t.Errorf("nothing should be persisted/replicated")
	}
}

func TestPatchNodeVolume(t *testing.T) {
	self := id.New()
	cfg, fc, _ := baseConfig(self)
	nc := &fakeNodeConfig{}
	sink := &fakeSink{}
	cfg.NodeCfg = nc
	cfg.Sink = func() SinkControl { return sink }
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPatch, "/api/node", map[string]any{"volume": 0.5})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(nc.vols) != 1 || nc.vols[0] != 0.5 {
		t.Errorf("cfg vols = %v", nc.vols)
	}
	if len(fc.setVolume) != 1 || fc.setVolume[0] != 0.5 {
		t.Errorf("cluster vols = %v", fc.setVolume)
	}
	if len(sink.gains) != 1 || sink.gains[0] != 0.5 {
		t.Errorf("sink gains = %v", sink.gains)
	}
}

func TestPatchNodeOutputDelay(t *testing.T) {
	self := id.New()
	cfg, fc, _ := baseConfig(self)
	nc := &fakeNodeConfig{}
	sink := &fakeSink{}
	cfg.NodeCfg = nc
	cfg.Sink = func() SinkControl { return sink }
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPatch, "/api/node", map[string]any{"outputDelayMs": 120})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(nc.delays) != 1 || nc.delays[0] != 120 {
		t.Errorf("cfg delays = %v", nc.delays)
	}
	if len(fc.setDelay) != 1 || fc.setDelay[0] != 120 {
		t.Errorf("cluster delays = %v", fc.setDelay)
	}
	if len(sink.delays) != 1 || sink.delays[0] != 120_000_000 {
		t.Errorf("sink delays = %v, want 120e6 ns", sink.delays)
	}
}

func TestPatchNodeMultipleFields(t *testing.T) {
	self := id.New()
	cfg, fc, _ := baseConfig(self)
	nc := &fakeNodeConfig{}
	sink := &fakeSink{}
	cfg.NodeCfg = nc
	cfg.Sink = func() SinkControl { return sink }
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPatch, "/api/node",
		map[string]any{"name": "x", "volume": 0.2, "outputDelayMs": -50})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(nc.names) != 1 || len(nc.vols) != 1 || len(nc.delays) != 1 {
		t.Errorf("persist counts: %v %v %v", nc.names, nc.vols, nc.delays)
	}
	if len(fc.setName) != 1 || len(fc.setVolume) != 1 || len(fc.setDelay) != 1 {
		t.Errorf("replicate counts")
	}
	if len(sink.gains) != 1 || len(sink.delays) != 1 {
		t.Errorf("apply counts")
	}
}

func TestPatchNodeEmptyBody(t *testing.T) {
	self := id.New()
	cfg, _, _ := baseConfig(self)
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPatch, "/api/node", map[string]any{})
	e := decodeErr(t, resp)
	if resp.StatusCode != 400 || e.Error != "empty_patch" {
		t.Fatalf("status=%d err=%q", resp.StatusCode, e.Error)
	}
}

func TestPatchNodeBadVolume(t *testing.T) {
	self := id.New()
	cfg, fc, _ := baseConfig(self)
	nc := &fakeNodeConfig{}
	cfg.NodeCfg = nc
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPatch, "/api/node", map[string]any{"volume": 1.5})
	e := decodeErr(t, resp)
	if resp.StatusCode != 400 || e.Error != "bad_volume" {
		t.Fatalf("status=%d err=%q", resp.StatusCode, e.Error)
	}
	if len(nc.vols) != 0 || len(fc.setVolume) != 0 {
		t.Errorf("nothing should be applied")
	}
}

func TestPatchNodeBadDelay(t *testing.T) {
	self := id.New()
	cfg, _, _ := baseConfig(self)
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPatch, "/api/node", map[string]any{"outputDelayMs": 9000})
	e := decodeErr(t, resp)
	if resp.StatusCode != 400 || e.Error != "bad_delay" {
		t.Fatalf("status=%d err=%q", resp.StatusCode, e.Error)
	}
}

// snapWithDevices is snapWith with the self node carrying an enumerated device
// list (D37), so the PATCH outputDevice validation can find it.
func snapWithDevices(self id.ID, devices []contracts.OutputDevice) contracts.Snapshot {
	return contracts.Snapshot{
		Nodes: []contracts.NodeView{
			{ID: self, Name: "n", Alive: true, HTTPPort: 8080, OutputDevices: devices},
		},
		Groups: []contracts.GroupView{{ID: self, Master: self, Members: []id.ID{self}}},
	}
}

func TestPatchNodeOutputDevice(t *testing.T) {
	self := id.New()
	cfg, fc, _ := baseConfig(self)
	fc.setSnapshot(snapWithDevices(self, []contracts.OutputDevice{{ID: "hw:1,0", Desc: "Card"}}))
	nc := &fakeNodeConfig{}
	cfg.NodeCfg = nc
	var applied []string
	cfg.ApplyOutputDevice = func(d string) { applied = append(applied, d) }
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPatch, "/api/node", map[string]any{"outputDevice": "hw:1,0"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(nc.devices) != 1 || nc.devices[0] != "hw:1,0" {
		t.Errorf("cfg devices = %v", nc.devices)
	}
	if len(fc.setDevice) != 1 || fc.setDevice[0] != "hw:1,0" {
		t.Errorf("cluster devices = %v", fc.setDevice)
	}
	if len(applied) != 1 || applied[0] != "hw:1,0" {
		t.Errorf("apply devices = %v", applied)
	}
}

func TestPatchNodeOutputDeviceDefaultAlwaysValid(t *testing.T) {
	self := id.New()
	cfg, fc, _ := baseConfig(self)
	fc.setSnapshot(snapWithDevices(self, nil)) // no enumerated devices
	nc := &fakeNodeConfig{}
	cfg.NodeCfg = nc
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPatch, "/api/node", map[string]any{"outputDevice": "default"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(nc.devices) != 1 || nc.devices[0] != "default" {
		t.Errorf("cfg devices = %v", nc.devices)
	}
}

func TestPatchNodeOutputDeviceUnknownRejected(t *testing.T) {
	self := id.New()
	cfg, fc, _ := baseConfig(self)
	fc.setSnapshot(snapWithDevices(self, []contracts.OutputDevice{{ID: "hw:1,0"}}))
	nc := &fakeNodeConfig{}
	cfg.NodeCfg = nc
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPatch, "/api/node", map[string]any{"outputDevice": "hw:9,9"})
	e := decodeErr(t, resp)
	if resp.StatusCode != 400 || e.Error != "bad_device" {
		t.Fatalf("status=%d err=%q", resp.StatusCode, e.Error)
	}
	if len(nc.devices) != 0 || len(fc.setDevice) != 0 {
		t.Errorf("nothing should be applied")
	}
}

func TestPatchNodeOutputDeviceEmptyRejected(t *testing.T) {
	self := id.New()
	cfg, _, _ := baseConfig(self)
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPatch, "/api/node", map[string]any{"outputDevice": "  "})
	e := decodeErr(t, resp)
	if resp.StatusCode != 400 || e.Error != "bad_device" {
		t.Fatalf("status=%d err=%q", resp.StatusCode, e.Error)
	}
}

func TestPatchNodeNilSinkNoOp(t *testing.T) {
	self := id.New()
	cfg, fc, _ := baseConfig(self)
	nc := &fakeNodeConfig{}
	cfg.NodeCfg = nc
	cfg.Sink = func() SinkControl { return nil }
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPatch, "/api/node", map[string]any{"volume": 0.3})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(nc.vols) != 1 || len(fc.setVolume) != 1 {
		t.Errorf("persist+replicate should still happen")
	}
}

func TestFollowOK(t *testing.T) {
	self := id.New()
	target := id.New()
	cfg, _, fg := baseConfig(self)
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPost, "/api/follow", map[string]any{"target": target.String()})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if fg.followTarget != target {
		t.Errorf("follow target = %v", fg.followTarget)
	}
}

func TestFollowBadTarget(t *testing.T) {
	self := id.New()
	cfg, _, _ := baseConfig(self)
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPost, "/api/follow", map[string]any{"target": "xyz"})
	e := decodeErr(t, resp)
	if resp.StatusCode != 400 || e.Error != "bad_target" {
		t.Fatalf("status=%d err=%q", resp.StatusCode, e.Error)
	}
}

func TestFollowUnknownNode(t *testing.T) {
	self := id.New()
	cfg, _, fg := baseConfig(self)
	fg.followErr = ErrUnknownNode
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPost, "/api/follow",
		map[string]any{"target": id.New().String()})
	e := decodeErr(t, resp)
	if resp.StatusCode != 404 || e.Error != "unknown_node" {
		t.Fatalf("status=%d err=%q", resp.StatusCode, e.Error)
	}
}

func TestFollowTargetNotMaster(t *testing.T) {
	self := id.New()
	cfg, _, fg := baseConfig(self)
	fg.followErr = ErrTargetNotMaster
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPost, "/api/follow",
		map[string]any{"target": id.New().String()})
	e := decodeErr(t, resp)
	if resp.StatusCode != 409 || e.Error != "target_not_master" {
		t.Fatalf("status=%d err=%q", resp.StatusCode, e.Error)
	}
}

func TestUnfollow(t *testing.T) {
	self := id.New()
	cfg, _, fg := baseConfig(self)
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPost, "/api/unfollow", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || fg.unfollowN != 1 {
		t.Fatalf("status=%d n=%d", resp.StatusCode, fg.unfollowN)
	}
}

func TestGroupNameOK(t *testing.T) {
	self := id.New()
	cfg, _, fg := baseConfig(self)
	gid := id.New()
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPost, "/api/group/name",
		map[string]any{"group": gid.String(), "name": "kitchen"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if fg.nameGroup != gid || fg.nameName != "kitchen" {
		t.Errorf("name args = %v %q", fg.nameGroup, fg.nameName)
	}
}

func TestGroupNameBadGroupID(t *testing.T) {
	self := id.New()
	cfg, _, _ := baseConfig(self)
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPost, "/api/group/name",
		map[string]any{"group": "nothex", "name": "x"})
	e := decodeErr(t, resp)
	if resp.StatusCode != 400 || e.Error != "bad_group" {
		t.Fatalf("status=%d err=%q", resp.StatusCode, e.Error)
	}
}

func TestGroupMasterForwards(t *testing.T) {
	self := id.New()
	node := id.New()
	cfg, _, fg := baseConfig(self)
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPost, "/api/group/master",
		map[string]any{"node": node.String()})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || fg.makeMasterArg != node {
		t.Fatalf("status=%d arg=%v", resp.StatusCode, fg.makeMasterArg)
	}
}

func TestPlayURI(t *testing.T) {
	self := id.New()
	cases := []string{"file:song.flac", "http://radio.example/stream", "input:"}
	for _, uri := range cases {
		cfg, _, fg := baseConfig(self)
		_, ts := testServer(t, cfg)
		resp := doJSON(t, ts, http.MethodPost, "/api/play", map[string]any{"uri": uri})
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("uri=%q status=%d", uri, resp.StatusCode)
		}
		if fg.playURI != uri {
			t.Errorf("uri=%q play got %q", uri, fg.playURI)
		}
	}
}

func TestPlayFileBackCompat(t *testing.T) {
	self := id.New()

	cfg, _, fg := baseConfig(self)
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPost, "/api/play", map[string]any{"file": "song.flac"})
	resp.Body.Close()
	if fg.playURI != "file:song.flac" {
		t.Errorf("file folded to %q", fg.playURI)
	}

	// bare scheme-less path
	cfg2, _, fg2 := baseConfig(self)
	_, ts2 := testServer(t, cfg2)
	resp2 := doJSON(t, ts2, http.MethodPost, "/api/play", map[string]any{"uri": "sub/song.mp3"})
	resp2.Body.Close()
	if fg2.playURI != "file:sub/song.mp3" {
		t.Errorf("bare path folded to %q", fg2.playURI)
	}

	// uri wins over file
	cfg3, _, fg3 := baseConfig(self)
	_, ts3 := testServer(t, cfg3)
	resp3 := doJSON(t, ts3, http.MethodPost, "/api/play",
		map[string]any{"uri": "http://x/y", "file": "z.flac"})
	resp3.Body.Close()
	if fg3.playURI != "http://x/y" {
		t.Errorf("uri should win, got %q", fg3.playURI)
	}
}

func TestPlayNonMaster409WithHint(t *testing.T) {
	self := id.New()
	cfg, _, fg := baseConfig(self)
	fg.playErr = ErrNotMaster
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPost, "/api/play", map[string]any{"uri": "file:a.flac"})
	e := decodeErr(t, resp)
	if resp.StatusCode != 409 || e.Error != "not_master" || e.Hint == "" {
		t.Fatalf("status=%d err=%q hint=%q", resp.StatusCode, e.Error, e.Hint)
	}
}

func TestPlayBadPath(t *testing.T) {
	self := id.New()
	for _, uri := range []string{"file:../escape", "../escape", "file:/abs"} {
		cfg, _, fg := baseConfig(self)
		_, ts := testServer(t, cfg)
		resp := doJSON(t, ts, http.MethodPost, "/api/play", map[string]any{"uri": uri})
		e := decodeErr(t, resp)
		if resp.StatusCode != 400 || e.Error != "bad_path" {
			t.Fatalf("uri=%q status=%d err=%q", uri, resp.StatusCode, e.Error)
		}
		if fg.playURI != "" {
			t.Errorf("uri=%q should not reach engine", uri)
		}
	}
}

func TestPlayBadScheme(t *testing.T) {
	self := id.New()
	cfg, _, fg := baseConfig(self)
	fg.playErr = ErrBadScheme
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPost, "/api/play", map[string]any{"uri": "spotify:track"})
	e := decodeErr(t, resp)
	if resp.StatusCode != 400 || e.Error != "bad_scheme" {
		t.Fatalf("status=%d err=%q", resp.StatusCode, e.Error)
	}
}

func TestStopOK(t *testing.T) {
	self := id.New()
	cfg, _, fg := baseConfig(self)
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPost, "/api/stop", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || fg.stopN != 1 {
		t.Fatalf("status=%d n=%d", resp.StatusCode, fg.stopN)
	}
}

func TestStopNonMaster(t *testing.T) {
	self := id.New()
	cfg, _, fg := baseConfig(self)
	fg.stopErr = ErrNotMaster
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPost, "/api/stop", nil)
	e := decodeErr(t, resp)
	if resp.StatusCode != 409 || e.Error != "not_master" {
		t.Fatalf("status=%d err=%q", resp.StatusCode, e.Error)
	}
}

func TestPauseOK(t *testing.T) {
	self := id.New()
	cfg, _, fg := baseConfig(self)
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPost, "/api/pause", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || fg.pauseN != 1 {
		t.Fatalf("status=%d n=%d", resp.StatusCode, fg.pauseN)
	}
}

func TestPauseNotPlaying(t *testing.T) {
	self := id.New()
	cfg, _, fg := baseConfig(self)
	fg.pauseErr = ErrNotPlaying
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPost, "/api/pause", nil)
	e := decodeErr(t, resp)
	if resp.StatusCode != 409 || e.Error != "not_playing" {
		t.Fatalf("status=%d err=%q", resp.StatusCode, e.Error)
	}
}

func TestResumeOK(t *testing.T) {
	self := id.New()
	cfg, _, fg := baseConfig(self)
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPost, "/api/resume", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || fg.resumeN != 1 {
		t.Fatalf("status=%d n=%d", resp.StatusCode, fg.resumeN)
	}
}

func TestResumeNotPaused(t *testing.T) {
	self := id.New()
	cfg, _, fg := baseConfig(self)
	fg.resumeErr = ErrNotPaused
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPost, "/api/resume", nil)
	e := decodeErr(t, resp)
	if resp.StatusCode != 409 || e.Error != "not_paused" {
		t.Fatalf("status=%d err=%q", resp.StatusCode, e.Error)
	}
}

func TestGetSettings(t *testing.T) {
	self := id.New()
	cfg, _, fg := baseConfig(self)
	fg.settings = contracts.GroupSettings{Codec: "opus", Transport: "tcp", BufferMs: 200}
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodGet, "/api/group/settings", nil)
	var got contracts.GroupSettings
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.Codec != "opus" || got.Transport != "tcp" || got.BufferMs != 200 {
		t.Errorf("settings = %+v", got)
	}
}

func TestSetSettingsMasterOnly(t *testing.T) {
	self := id.New()
	cfg, _, fg := baseConfig(self)
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPost, "/api/group/settings",
		map[string]any{"codec": "opus", "transport": "udp", "bufferMs": 150})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if fg.setSettingsArg.Codec != "opus" {
		t.Errorf("settings arg = %+v", fg.setSettingsArg)
	}
}

func TestSetSettingsBadCodec(t *testing.T) {
	self := id.New()
	cfg, _, fg := baseConfig(self)
	fg.setSettingsErr = ErrNoCodec
	_, ts := testServer(t, cfg)
	resp := doJSON(t, ts, http.MethodPost, "/api/group/settings",
		map[string]any{"codec": "opus"})
	e := decodeErr(t, resp)
	if resp.StatusCode != 400 || e.Error != "unsupported_codec" {
		t.Fatalf("status=%d err=%q", resp.StatusCode, e.Error)
	}
}

func TestMalformedJSON(t *testing.T) {
	self := id.New()
	cfg, _, _ := baseConfig(self)
	_, ts := testServer(t, cfg)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/follow",
		strReader(`{bad json`))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	e := decodeErr(t, resp)
	if resp.StatusCode != 400 || e.Error != "bad_request" {
		t.Fatalf("status=%d err=%q", resp.StatusCode, e.Error)
	}
}
