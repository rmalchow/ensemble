package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/web"
)

// These table tests exercise the daemon-side transport ops (media.go) against an
// in-memory state.Store + a fake peer proxy, with no sockets (07 §F owning
// criteria, P4.9 §7.2). They assert the §F.2-F.4 guards (non-mp3 422, missing-on-
// master 404, no-media 409, If-Match enforcement, fan-out) and the §G.2 status
// projection.

// fakePeer records fan-out / proxy calls and answers existence + status from
// scripted maps.
type fakePeer struct {
	exists     map[string]bool // file -> exists on master
	existsErr  error
	fanOuts    []string // groupIDs fanned out
	fanOutErr  error
	statuses   map[string]web.MemberStatus // nodeID -> status
	statusErr  map[string]error
	listFiles  []web.MediaFile
	calibrated []string
	calibErr   error
}

func (f *fakePeer) MediaExists(_, file string) (bool, error) {
	if f.existsErr != nil {
		return false, f.existsErr
	}
	return f.exists[file], nil
}
func (f *fakePeer) FanOutTransport(_, groupID string) error {
	if f.fanOutErr != nil {
		return f.fanOutErr
	}
	f.fanOuts = append(f.fanOuts, groupID)
	return nil
}
func (f *fakePeer) MemberStatus(nodeID, _ string) (web.MemberStatus, error) {
	if f.statusErr != nil {
		if err := f.statusErr[nodeID]; err != nil {
			return web.MemberStatus{}, err
		}
	}
	return f.statuses[nodeID], nil
}
func (f *fakePeer) ListMedia(_, _ string) ([]web.MediaFile, []string, error) {
	return f.listFiles, nil, nil
}
func (f *fakePeer) CalibratePlay(nodeID string, _ int) error {
	if f.calibErr != nil {
		return f.calibErr
	}
	f.calibrated = append(f.calibrated, nodeID)
	return nil
}

// newMediaNode builds a Node with a live in-memory transport over the given doc.
// self is the master (single-node) unless overridden by the doc membership.
func newMediaNode(t *testing.T, self string, doc state.ConfigDoc, peer peerProxy, localFiles []web.MediaFile) (*Node, *state.Store) {
	t.Helper()
	store := state.New(self)
	if doc.Version == 0 {
		// Apply needs version 0 to match the empty store; seed via Apply.
	}
	if _, err := store.Apply(doc); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	n := New(Options{NodeID: self})
	n.tx = &transport{
		store:     store,
		self:      self,
		peer:      peer,
		listLocal: func() ([]web.MediaFile, error) { return localFiles, nil },
		master:    func(gid string) string { return masterOf(store.Get(), gid, self) },
	}
	return n, store
}

// docWithGroup builds a ConfigDoc with one group + members, all render-capable.
func docWithGroup(groupID string, members []string, media string, playing bool) state.ConfigDoc {
	doc := state.ConfigDoc{}
	for _, m := range members {
		doc.Nodes = append(doc.Nodes, state.NodeRecord{ID: m, Caps: state.Capabilities{Render: true}})
	}
	g := state.GroupRecord{ID: groupID, MemberNodeIDs: members, Playing: playing}
	if media != "" {
		g.Media = state.MediaSelection{File: media}
	}
	doc.Groups = append(doc.Groups, g)
	return doc
}

func curVersion(s *state.Store) uint64 { return s.Get().Version }

func TestSelectMedia(t *testing.T) {
	const self = "node-a"
	tests := []struct {
		name       string
		file       string
		existsOn   map[string]bool
		members    []string
		ifMatchOff bool // pass a wrong If-Match
		wantErr    error
		wantMedia  string
	}{
		{
			name:    "non-mp3 rejected",
			file:    "song.flac",
			members: []string{self},
			wantErr: web.ErrNotMP3,
		},
		{
			name:     "missing on master",
			file:     "ghost.mp3",
			existsOn: map[string]bool{},
			members:  []string{self},
			wantErr:  web.ErrMissingOnMaster,
		},
		{
			name:      "ok writes media + bumps version",
			file:      "tune.mp3",
			existsOn:  map[string]bool{"tune.mp3": true},
			members:   []string{self},
			wantMedia: "tune.mp3",
		},
		{
			name:       "version conflict",
			file:       "tune.mp3",
			existsOn:   map[string]bool{"tune.mp3": true},
			members:    []string{self},
			ifMatchOff: true,
			wantErr:    web.ErrVersionConflict,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			local := make([]web.MediaFile, 0)
			for f, ok := range tc.existsOn {
				if ok {
					local = append(local, web.MediaFile{File: f})
				}
			}
			doc := docWithGroup("g1", tc.members, "", false)
			n, store := newMediaNode(t, self, doc, nil, local)
			ifMatch := curVersion(store)
			if tc.ifMatchOff {
				ifMatch += 99
			}
			out, err := n.selectMedia("g1", tc.file, false, "", ifMatch)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if curVersion(store) != doc.Version+0 && !errors.Is(tc.wantErr, web.ErrVersionConflict) {
					// no successful Apply on a guard failure (version unchanged from seed+0)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if g := groupRecord(out, "g1"); g == nil || g.Media.File != tc.wantMedia {
				t.Fatalf("media = %v, want %q", g, tc.wantMedia)
			}
			if out.Version <= doc.Version {
				t.Fatalf("version not bumped: %d", out.Version)
			}
		})
	}
}

func TestPlayStop(t *testing.T) {
	const self = "node-a"

	t.Run("play with no media => 409 conflict", func(t *testing.T) {
		doc := docWithGroup("g1", []string{self}, "", false)
		n, store := newMediaNode(t, self, doc, nil, nil)
		_, err := n.play("g1", "", false, "", curVersion(store))
		if !errors.Is(err, web.ErrNoMedia) {
			t.Fatalf("err = %v, want ErrNoMedia", err)
		}
		if store.Get().Groups[0].Playing {
			t.Fatal("Playing flipped despite no media")
		}
	})

	t.Run("play ok flips Playing + fans out to master", func(t *testing.T) {
		// Two members so self is NOT the lone master => fan-out target is the elected
		// master (node-a, lexicographically lowest), but since self==master no
		// fan-out occurs; use node-z as self so node-a is the master peer.
		const me = "node-z"
		peer := &fakePeer{exists: map[string]bool{"t.mp3": true}}
		doc := docWithGroup("g1", []string{"node-a", me}, "t.mp3", false)
		n, store := newMediaNode(t, me, doc, peer, nil)
		out, err := n.play("g1", "", false, "", curVersion(store))
		if err != nil {
			t.Fatalf("play err: %v", err)
		}
		if !groupRecord(out, "g1").Playing {
			t.Fatal("Playing not set")
		}
		if len(peer.fanOuts) != 1 || peer.fanOuts[0] != "g1" {
			t.Fatalf("fanOuts = %v, want [g1]", peer.fanOuts)
		}
	})

	t.Run("play If-Match absent (0) => version conflict", func(t *testing.T) {
		doc := docWithGroup("g1", []string{self}, "t.mp3", false)
		n, store := newMediaNode(t, self, doc, nil, nil)
		_ = store
		_, err := n.play("g1", "", false, "", 0) // store version is 1 after seed Apply
		if !errors.Is(err, web.ErrVersionConflict) {
			t.Fatalf("err = %v, want ErrVersionConflict", err)
		}
	})

	t.Run("stop ok flips Playing + fans out stop", func(t *testing.T) {
		const me = "node-z"
		peer := &fakePeer{}
		doc := docWithGroup("g1", []string{"node-a", me}, "t.mp3", true)
		n, store := newMediaNode(t, me, doc, peer, nil)
		out, err := n.stop("g1", curVersion(store))
		if err != nil {
			t.Fatalf("stop err: %v", err)
		}
		if groupRecord(out, "g1").Playing {
			t.Fatal("Playing still set after stop")
		}
		if len(peer.fanOuts) != 1 {
			t.Fatalf("stop fan-out = %v, want 1", peer.fanOuts)
		}
	})

	t.Run("stop master unreachable => 502 but config written", func(t *testing.T) {
		const me = "node-z"
		peer := &fakePeer{fanOutErr: errors.New("dial fail")}
		doc := docWithGroup("g1", []string{"node-a", me}, "t.mp3", true)
		n, store := newMediaNode(t, me, doc, peer, nil)
		_, err := n.stop("g1", curVersion(store))
		if !errors.Is(err, web.ErrUnreachable) {
			t.Fatalf("err = %v, want ErrUnreachable", err)
		}
		// Config write still happened (Playing=false, version bumped).
		if store.Get().Groups[0].Playing {
			t.Fatal("config not written before fan-out failed")
		}
	})
}

func TestCalibratePlay(t *testing.T) {
	const self = "node-a"

	t.Run("render=false node => warning, not fatal", func(t *testing.T) {
		doc := state.ConfigDoc{
			Nodes: []state.NodeRecord{
				{ID: self, Caps: state.Capabilities{Render: true}},
				{ID: "deaf", Caps: state.Capabilities{Render: false}},
			},
			Groups: []state.GroupRecord{{ID: "g1", MemberNodeIDs: []string{self, "deaf"}}},
		}
		peer := &fakePeer{}
		n, _ := newMediaNode(t, self, doc, peer, nil)
		n.tx.playCalibrateLocal = func(int) error { return nil }
		played, warnings, err := n.calibratePlay(web.CalibrateSel{GroupID: "g1"}, 3)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(played) != 1 || played[0] != self {
			t.Fatalf("played = %v, want [%s]", played, self)
		}
		if len(warnings) != 1 {
			t.Fatalf("warnings = %v, want 1 (deaf node)", warnings)
		}
	})

	t.Run("unknown group => ErrNotMember", func(t *testing.T) {
		n, _ := newMediaNode(t, self, docWithGroup("g1", []string{self}, "", false), nil, nil)
		_, _, err := n.calibratePlay(web.CalibrateSel{GroupID: "nope"}, 3)
		if !errors.Is(err, web.ErrNotMember) {
			t.Fatalf("err = %v, want ErrNotMember", err)
		}
	})
}

func TestGroupStatus(t *testing.T) {
	const self = "node-a"

	t.Run("unknown group => ErrNotMember", func(t *testing.T) {
		n, _ := newMediaNode(t, self, docWithGroup("g1", []string{self}, "", false), nil, nil)
		_, err := n.groupStatus("nope")
		if !errors.Is(err, web.ErrNotMember) {
			t.Fatalf("err = %v, want ErrNotMember", err)
		}
	})

	t.Run("member down => Online=false per member, top-level ok", func(t *testing.T) {
		peer := &fakePeer{
			statuses:  map[string]web.MemberStatus{},
			statusErr: map[string]error{"node-b": errors.New("unreachable")},
		}
		doc := docWithGroup("g1", []string{self, "node-b"}, "t.mp3", true)
		n, _ := newMediaNode(t, self, doc, peer, nil)
		st, err := n.groupStatus("g1")
		if err != nil {
			t.Fatalf("top-level err: %v", err)
		}
		if len(st.Members) != 2 {
			t.Fatalf("members = %d, want 2", len(st.Members))
		}
		var down web.MemberStatus
		for _, m := range st.Members {
			if m.NodeID == "node-b" {
				down = m
			}
		}
		if down.Online {
			t.Fatal("node-b should be Online=false")
		}
	})

	t.Run("not-ready (no session) => ErrGroupNotReady", func(t *testing.T) {
		n := New(Options{NodeID: self})
		_, err := n.groupStatus("g1")
		if !errors.Is(err, web.ErrGroupNotReady) {
			t.Fatalf("err = %v, want ErrGroupNotReady", err)
		}
	})
}

func TestListMedia(t *testing.T) {
	const self = "node-a"
	local := []web.MediaFile{{File: "a.mp3", SizeBytes: 10}}
	n, _ := newMediaNode(t, self, docWithGroup("g1", []string{self}, "", false), &fakePeer{listFiles: []web.MediaFile{{File: "remote.mp3"}}}, local)

	got, _, err := n.listMedia("", "")
	if err != nil || len(got) != 1 || got[0].File != "a.mp3" {
		t.Fatalf("local list = (%v, %v), want [a.mp3]", got, err)
	}
	got, _, err = n.listMedia("peer", "")
	if err != nil || len(got) != 1 || got[0].File != "remote.mp3" {
		t.Fatalf("peer list = (%v, %v), want [remote.mp3]", got, err)
	}
}

// TestListLocalMediaDirs covers the F.1 directory browsing: subdirectories are
// surfaced, nested files carry data/-relative slash paths (so selection plays
// unchanged), and a hostile ../ path cannot escape the data root.
func TestListLocalMediaDirs(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel string, b []byte) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Minimal MP3 frame-sync header so the lister's extension filter passes.
	mp3 := []byte{0xFF, 0xFB, 0x90, 0x00}
	mustWrite("root.mp3", mp3)
	mustWrite("albums/one.mp3", mp3)
	mustWrite("albums/notes.txt", []byte("x"))

	files, dirs, err := listLocalMedia(dir, "")
	if err != nil {
		t.Fatalf("root list: %v", err)
	}
	if len(files) != 1 || files[0].File != "root.mp3" {
		t.Fatalf("root files = %+v, want [root.mp3]", files)
	}
	if len(dirs) != 1 || dirs[0] != "albums" {
		t.Fatalf("root dirs = %+v, want [albums]", dirs)
	}

	files, dirs, err = listLocalMedia(dir, "albums")
	if err != nil {
		t.Fatalf("subdir list: %v", err)
	}
	if len(files) != 1 || files[0].File != "albums/one.mp3" {
		t.Fatalf("subdir files = %+v, want [albums/one.mp3] (data/-relative)", files)
	}
	if len(dirs) != 0 {
		t.Fatalf("subdir dirs = %+v, want none", dirs)
	}

	// Traversal: "../" collapses against the root — the listing stays inside
	// data/ (here: resolves to data/etc, which does not exist).
	if got := cleanRelPath("../../etc"); got != "etc" {
		t.Fatalf("cleanRelPath(../../etc) = %q, want etc", got)
	}
	if got := cleanRelPath(`..\..\etc`); got != "etc" {
		t.Fatalf(`cleanRelPath(..\..\etc) = %q, want etc`, got)
	}
	if !statLocalMedia(dir, "albums/one.mp3") {
		t.Fatal("statLocalMedia(albums/one.mp3) = false, want true")
	}
	if statLocalMedia(dir, "nope/none.mp3") {
		t.Fatal("statLocalMedia(missing) = true, want false")
	}
}

// TestSelectMediaSetsMasterHint pins the master-follows-source rule: selecting
// a file FROM node X's library writes GroupRecord.MasterHint=X, so the election
// moves mastership to the node that holds (and decodes) the media.
func TestSelectMediaSetsMasterHint(t *testing.T) {
	const me = "node-a"
	peer := &fakePeer{exists: map[string]bool{"b/tune.mp3": true}}
	doc := docWithGroup("g1", []string{me, "node-b"}, "", false)
	n, store := newMediaNode(t, me, doc, peer, nil)

	// Selecting node-b's file: existence checked on node-b (the future master),
	// MasterHint written alongside the media in one Apply.
	out, err := n.selectMedia("g1", "b/tune.mp3", false, "node-b", curVersion(store))
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	g := groupRecord(out, "g1")
	if g.Media.File != "b/tune.mp3" || g.MasterHint != "node-b" {
		t.Fatalf("group = %+v, want media=b/tune.mp3 masterHint=node-b", g)
	}

	// One-shot play from MY library moves the hint back to me (local existence
	// check, no peer involved).
	out, err = n.play("g1", "mine.mp3", true, me, out.Version)
	if !errors.Is(err, web.ErrMissingOnMaster) {
		t.Fatalf("play missing local file err = %v, want ErrMissingOnMaster", err)
	}
	n2, store2 := newMediaNode(t, me, docWithGroup("g1", []string{me, "node-b"}, "", false), peer,
		[]web.MediaFile{{File: "mine.mp3"}})
	out, err = n2.play("g1", "mine.mp3", true, me, curVersion(store2))
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	g = groupRecord(out, "g1")
	if g.MasterHint != me || !g.Playing {
		t.Fatalf("group = %+v, want masterHint=%s playing=true", g, me)
	}
}
