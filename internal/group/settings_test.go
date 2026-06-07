package group

import (
	"errors"
	"testing"
	"time"

	"ensemble/internal/contracts"
	"ensemble/internal/stream"
)

func TestSettingsDefaults(t *testing.T) {
	self := idN(1)
	r := newRig(self, 0, false)
	r.cl.setSnap(soloSnap(self))
	s := r.e.Settings()
	if s.Codec != "opus" || s.Transport != "udp" || s.BufferMs != 150 {
		t.Fatalf("settings = %+v, want opus/udp/150", s)
	}
}

func TestSetSettingsMasterWritesAndReconfigs(t *testing.T) {
	self := idN(1)
	r := newRig(self, 100, true)
	r.cl.setSnap(soloSnap(self))
	// Start a session so the live path engages.
	if err := r.e.Play("input:"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	defer r.e.Close()
	genBefore := r.e.gen
	startsBefore := r.srv.startCount()

	if err := r.e.SetSettings(contracts.GroupSettings{Codec: "pcm", Transport: "tcp", BufferMs: 300}); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}
	sc, ok := r.cl.lastSettings()
	if !ok || sc.s.Transport != "tcp" || sc.s.BufferMs != 300 {
		t.Fatalf("SetGroupSettings = %+v", sc.s)
	}
	if r.e.gen != genBefore+1 {
		t.Fatalf("gen = %d, want %d", r.e.gen, genBefore+1)
	}
	if r.srv.startCount() != startsBefore+1 {
		t.Fatalf("StartSession not re-armed: %d -> %d", startsBefore, r.srv.startCount())
	}
	st, _ := r.srv.lastStart()
	if st.t != stream.TransportTCP || st.bufferMs != 300 {
		t.Fatalf("re-arm start = %+v", st)
	}
}

func TestSetSettingsRejectsFollower(t *testing.T) {
	master, self := idN(1), idN(2)
	r := newRig(self, 0, false)
	r.cl.setSnap(masterSnap(master, defaultSettings(), self))
	if err := r.e.SetSettings(contracts.GroupSettings{Codec: "pcm", Transport: "udp", BufferMs: 150}); !errors.Is(err, ErrNotMaster) {
		t.Fatalf("err = %v, want ErrNotMaster", err)
	}
}

func TestSetSettingsValidates(t *testing.T) {
	self := idN(1)
	r := newRig(self, 0, false)
	r.cl.setSnap(soloSnap(self))
	if err := r.e.SetSettings(contracts.GroupSettings{Codec: "bogus", Transport: "udp"}); !errors.Is(err, ErrBadSettings) {
		t.Fatalf("bad codec err = %v, want ErrBadSettings", err)
	}
	if err := r.e.SetSettings(contracts.GroupSettings{Codec: "pcm", Transport: "bogus"}); !errors.Is(err, ErrBadSettings) {
		t.Fatalf("bad transport err = %v, want ErrBadSettings", err)
	}
	if err := r.e.SetSettings(contracts.GroupSettings{Codec: "opus", Transport: "udp"}); !errors.Is(err, ErrNoOpus) {
		t.Fatalf("opus no-cap err = %v, want ErrNoOpus", err)
	}
}

func TestSetSettingsClampsBuffer(t *testing.T) {
	self := idN(1)
	r := newRig(self, 0, false)
	r.cl.setSnap(soloSnap(self))

	if err := r.e.SetSettings(contracts.GroupSettings{Codec: "pcm", Transport: "udp", BufferMs: 5}); err != nil {
		t.Fatalf("SetSettings low: %v", err)
	}
	sc, _ := r.cl.lastSettings()
	if sc.s.BufferMs != minBufferMs {
		t.Fatalf("low buffer = %d, want %d", sc.s.BufferMs, minBufferMs)
	}

	if err := r.e.SetSettings(contracts.GroupSettings{Codec: "pcm", Transport: "udp", BufferMs: 99999}); err != nil {
		t.Fatalf("SetSettings high: %v", err)
	}
	sc, _ = r.cl.lastSettings()
	if sc.s.BufferMs != maxBufferMs {
		t.Fatalf("high buffer = %d, want %d", sc.s.BufferMs, maxBufferMs)
	}

	if err := r.e.SetSettings(contracts.GroupSettings{Codec: "pcm", Transport: "udp", BufferMs: 0}); err != nil {
		t.Fatalf("SetSettings zero: %v", err)
	}
	sc, _ = r.cl.lastSettings()
	if sc.s.BufferMs != contracts.DefaultBufferMs {
		t.Fatalf("zero buffer = %d, want %d", sc.s.BufferMs, contracts.DefaultBufferMs)
	}
}

func TestSetSettingsIdleWritesRecordOnly(t *testing.T) {
	self := idN(1)
	r := newRig(self, 0, false)
	r.cl.setSnap(soloSnap(self))
	if err := r.e.SetSettings(contracts.GroupSettings{Codec: "pcm", Transport: "tcp", BufferMs: 150}); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}
	if _, ok := r.cl.lastSettings(); !ok {
		t.Fatal("settings record not written")
	}
	// idle: no StartSession re-arm.
	if r.srv.startCount() != 0 {
		t.Fatalf("StartSession called while idle: %d", r.srv.startCount())
	}
	// gen not bumped while idle.
	if r.e.gen != 0 {
		t.Fatalf("gen = %d, want 0 (idle)", r.e.gen)
	}
}

func TestSetSettingsLiveMidSession(t *testing.T) {
	self := idN(1)
	r := newRig(self, 1000, true)
	r.cl.setSnap(soloSnap(self))
	if err := r.e.Play("input:"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	defer r.e.Close()
	waitFor(t, time.Second, func() bool { return len(r.srv.snapshotReleases()) >= 1 }, "first release")

	if err := r.e.SetSettings(contracts.GroupSettings{Codec: "pcm", Transport: "tcp", BufferMs: 200}); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}
	// The subscriber should have been re-pointed (extra Subscribe call) under the
	// new gen.
	waitFor(t, time.Second, func() bool {
		subs := r.sub.snapshotSubs()
		if len(subs) == 0 {
			return false
		}
		return subs[len(subs)-1].gen == r.e.gen
	}, "subscriber re-pointed to new gen")
}
