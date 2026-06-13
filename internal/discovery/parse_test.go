package discovery

import (
	"net"
	"net/netip"
	"strings"
	"testing"

	"ensemble/internal/id"

	"github.com/grandcat/zeroconf"
)

var (
	selfID = id.MustParse("00000000000000000000000000000001")
	peerID = id.MustParse("aabbccddeeff00112233445566778899")
)

// entry builds a *zeroconf.ServiceEntry with the given TXT and addresses.
func entry(text []string, v4, v6 []net.IP) *zeroconf.ServiceEntry {
	e := zeroconf.NewServiceEntry(peerID.String(), ServiceName, Domain)
	e.Text = text
	e.AddrIPv4 = v4
	e.AddrIPv6 = v6
	return e
}

func validTXT() []string {
	return []string{
		"id=" + peerID.String(),
		"gossip=7946",
		"http=8080",
		"stream=9090",
		"source=9200",
	}
}

func TestParseEntryValid(t *testing.T) {
	e := entry(validTXT(), []net.IP{net.ParseIP("192.168.1.17")}, nil)
	p, ok := parseEntry(e, selfID)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if p.ID != peerID {
		t.Errorf("ID = %v, want %v", p.ID, peerID)
	}
	if p.GossipPort != 7946 || p.HTTPPort != 8080 || p.StreamPort != 9090 || p.SourcePort != 9200 {
		t.Errorf("ports = %d/%d/%d/%d", p.GossipPort, p.HTTPPort, p.StreamPort, p.SourcePort)
	}
	if p.Addr != netip.MustParseAddr("192.168.1.17") {
		t.Errorf("Addr = %v", p.Addr)
	}
}

func TestParseEntryDropsSelf(t *testing.T) {
	txt := []string{
		"id=" + selfID.String(),
		"gossip=7946", "http=8080", "stream=9090", "source=9200",
	}
	e := entry(txt, []net.IP{net.ParseIP("192.168.1.17")}, nil)
	if _, ok := parseEntry(e, selfID); ok {
		t.Fatal("expected self advertisement to be dropped")
	}
}

func TestParseEntryMissingKey(t *testing.T) {
	keys := []string{"id", "gossip", "http", "stream", "source"}
	for _, drop := range keys {
		txt := []string{}
		for _, kv := range validTXT() {
			if k, _, _ := strings.Cut(kv, "="); k == drop {
				continue
			}
			txt = append(txt, kv)
		}
		e := entry(txt, []net.IP{net.ParseIP("192.168.1.17")}, nil)
		if _, ok := parseEntry(e, selfID); ok {
			t.Errorf("missing %q: expected ok=false", drop)
		}
	}
}

func TestParseEntryBadPort(t *testing.T) {
	// Each case overrides one port key with an invalid value.
	cases := []string{"gossip=abc", "http=0", "stream=70000", "source=-1"}
	for _, bad := range cases {
		badKey, _, _ := strings.Cut(bad, "=")
		out := make([]string, 0, 5)
		for _, kv := range validTXT() {
			k, _, _ := strings.Cut(kv, "=")
			if k == badKey {
				out = append(out, bad)
			} else {
				out = append(out, kv)
			}
		}
		e := entry(out, []net.IP{net.ParseIP("192.168.1.17")}, nil)
		if _, ok := parseEntry(e, selfID); ok {
			t.Errorf("bad port %q: expected ok=false", bad)
		}
	}
}

func TestParseEntryBadID(t *testing.T) {
	txt := []string{"id=nothex", "gossip=7946", "http=8080", "stream=9090", "source=9200"}
	e := entry(txt, []net.IP{net.ParseIP("192.168.1.17")}, nil)
	if _, ok := parseEntry(e, selfID); ok {
		t.Fatal("expected bad id to be dropped")
	}
}

func TestParseEntryPrefersIPv4(t *testing.T) {
	e := entry(validTXT(),
		[]net.IP{net.ParseIP("192.168.1.17")},
		[]net.IP{net.ParseIP("fd00::5")})
	p, ok := parseEntry(e, selfID)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if p.Addr != netip.MustParseAddr("192.168.1.17") {
		t.Errorf("Addr = %v, want IPv4", p.Addr)
	}
}

func TestParseEntryIPv6Fallback(t *testing.T) {
	e := entry(validTXT(), nil, []net.IP{net.ParseIP("fd00::5")})
	p, ok := parseEntry(e, selfID)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if p.Addr != netip.MustParseAddr("fd00::5") {
		t.Errorf("Addr = %v, want fd00::5", p.Addr)
	}
}

func TestParseEntrySkipsLoopbackLinkLocal(t *testing.T) {
	e := entry(validTXT(),
		[]net.IP{net.ParseIP("127.0.0.1")},
		[]net.IP{net.ParseIP("fe80::1")})
	if _, ok := parseEntry(e, selfID); ok {
		t.Fatal("expected loopback+link-local to yield no usable address")
	}
}

func TestGossipAddrPort(t *testing.T) {
	p := Peer{ID: peerID, Addr: netip.MustParseAddr("10.0.0.4"), GossipPort: 7946}
	got := p.GossipAddrPort()
	want := netip.MustParseAddrPort("10.0.0.4:7946")
	if got != want {
		t.Errorf("GossipAddrPort = %v, want %v", got, want)
	}
}

func TestTXTRecords(t *testing.T) {
	// A master (incl. the default/legacy zero-role Config) advertises its name + ports.
	cfg := Config{ID: peerID, Master: true, Name: "living-room", Version: "v9.9.9", GossipPort: 7946, HTTPPort: 8080, StreamPort: 9090, SourcePort: 9200}
	txt := txtRecords(cfg)
	want := []string{
		"id=" + peerID.String(),
		"role=master",
		"ver=v9.9.9",
		"name=living-room",
		"gossip=7946", "http=8080", "stream=9090", "source=9200",
	}
	if len(txt) != len(want) {
		t.Fatalf("got %d records (%v), want %d", len(txt), txt, len(want))
	}
	for i := range want {
		if txt[i] != want[i] {
			t.Errorf("txt[%d] = %q, want %q", i, txt[i], want[i])
		}
	}
}

func TestTXTRecordsPlaybackOnly(t *testing.T) {
	cfg := Config{
		ID: peerID, Playback: true, ControlPort: 9300, Name: "kitchen", Version: "v9.9.9",
		Caps: Caps{Codecs: []string{"opus", "pcm"}, MaxRate: 48000, HWVolume: true, FixedDelayMs: 30, CanReportQueue: true, Input: false},
	}
	want := []string{
		"id=" + peerID.String(),
		"role=playback",
		"ver=v9.9.9",
		"control=9300", "name=kitchen", "codecs=opus,pcm", "rate=48000", "hwvol=1", "delayms=30", "queue=1", "input=0",
	}
	got := txtRecords(cfg)
	if len(got) != len(want) {
		t.Fatalf("got %d records (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("txt[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// A combined master+playback node advertises as a master (D61).
func TestTXTRecordsCombinedAdvertisesMaster(t *testing.T) {
	cfg := Config{ID: peerID, Master: true, Playback: true, GossipPort: 7946, HTTPPort: 8080, StreamPort: 9090, SourcePort: 9200, ControlPort: 9300}
	got := txtRecords(cfg)
	if got[1] != "role=master" {
		t.Fatalf("combined node role = %q, want role=master", got[1])
	}
	for _, r := range got {
		if strings.HasPrefix(r, "control=") {
			t.Fatal("combined node must not advertise a control port")
		}
	}
}

func TestParseEntryPlaybackOnly(t *testing.T) {
	txt := []string{
		"id=" + peerID.String(),
		"role=playback", "ver=v0.12.1",
		"control=9300", "codecs=opus,pcm", "rate=48000", "hwvol=1", "delayms=30", "queue=1", "input=0",
	}
	e := entry(txt, []net.IP{net.ParseIP("192.168.1.17")}, nil)
	p, ok := parseEntry(e, selfID)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if p.Master || !p.Playback || !p.PlaybackOnly() {
		t.Fatalf("roles: master=%v playback=%v only=%v", p.Master, p.Playback, p.PlaybackOnly())
	}
	if p.AppVersion != "v0.12.1" {
		t.Fatalf("AppVersion = %q, want v0.12.1", p.AppVersion)
	}
	if p.ControlPort != 9300 || p.GossipPort != 0 {
		t.Fatalf("ports: control=%d gossip=%d", p.ControlPort, p.GossipPort)
	}
	if len(p.Caps.Codecs) != 2 || p.Caps.Codecs[0] != "opus" || p.Caps.MaxRate != 48000 ||
		!p.Caps.HWVolume || p.Caps.FixedDelayMs != 30 || !p.Caps.CanReportQueue || p.Caps.Input {
		t.Fatalf("caps: %+v", p.Caps)
	}
}

func TestParseEntryPlaybackMissingControlDropped(t *testing.T) {
	txt := []string{"id=" + peerID.String(), "role=playback", "codecs=opus"} // no control=
	e := entry(txt, []net.IP{net.ParseIP("192.168.1.17")}, nil)
	if _, ok := parseEntry(e, selfID); ok {
		t.Fatal("playback advert without a control port must be dropped")
	}
}

// A legacy advert with no role= key parses as a master (back-compat).
func TestParseEntryNoRoleDefaultsMaster(t *testing.T) {
	e := entry(validTXT(), []net.IP{net.ParseIP("192.168.1.17")}, nil)
	p, ok := parseEntry(e, selfID)
	if !ok || !p.Master || p.Playback {
		t.Fatalf("no-role advert: ok=%v master=%v playback=%v", ok, p.Master, p.Playback)
	}
}
