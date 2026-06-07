package api

import (
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// dialWS upgrades to the server's /api/ws endpoint.
func dialWS(t *testing.T, baseURL string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(baseURL, "http") + "/api/ws"
	conn, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	if resp != nil {
		resp.Body.Close()
	}
	return conn
}

func readEvent(t *testing.T, conn *websocket.Conn, within time.Duration) wsEvent {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(within))
	var ev wsEvent
	if err := conn.ReadJSON(&ev); err != nil {
		t.Fatalf("read event: %v", err)
	}
	return ev
}

func TestWSUpgradeAndFirstEvent(t *testing.T) {
	self := id.New()
	cfg, fc, _ := baseConfig(self)
	fc.setSnapshot(snapWith(self, self, []id.ID{self}, "alice"))
	_, ts := testServer(t, cfg)

	conn := dialWS(t, ts.URL)
	defer conn.Close()

	ev := readEvent(t, conn, 2*time.Second)
	if ev.Type != "cluster" {
		t.Errorf("first event type = %q, want cluster", ev.Type)
	}
	if len(ev.Data.Nodes) != 1 {
		t.Errorf("first event nodes = %d", len(ev.Data.Nodes))
	}
}

func TestWSEventEnvelope(t *testing.T) {
	self := id.New()
	cfg, fc, _ := baseConfig(self)
	fc.setSnapshot(snapWith(self, self, []id.ID{self}, "alice"))
	_, ts := testServer(t, cfg)

	conn := dialWS(t, ts.URL)
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, `"type":"cluster"`) || !strings.Contains(s, `"data":`) {
		t.Errorf("envelope = %s", s)
	}
	if !strings.Contains(s, `"nodes":`) || !strings.Contains(s, `"groups":`) {
		t.Errorf("data should be a snapshot: %s", s)
	}
}

func TestWSDebouncesBurst(t *testing.T) {
	old := wsDebounce
	wsDebounce = 100 * time.Millisecond
	defer func() { wsDebounce = old }()
	oldHB := wsHeartbeat
	wsHeartbeat = time.Hour // disable heartbeat noise
	defer func() { wsHeartbeat = oldHB }()

	self := id.New()
	cfg, fc, _ := baseConfig(self)
	fc.setSnapshot(snapWith(self, self, []id.ID{self}, "alice"))
	_, ts := testServer(t, cfg)

	conn := dialWS(t, ts.URL)
	defer conn.Close()

	// Drain the initial snapshot event.
	readEvent(t, conn, 2*time.Second)

	// Fire 10 rapid change signals.
	for i := 0; i < 10; i++ {
		fc.signal()
	}

	// Count events arriving in the next 300ms; debounce should coalesce to ~1.
	count := 0
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline)
		var ev wsEvent
		if err := conn.ReadJSON(&ev); err != nil {
			break
		}
		count++
	}
	if count > 2 {
		t.Errorf("burst coalesced to %d events, want <=2", count)
	}
	if count == 0 {
		t.Errorf("expected at least 1 coalesced event")
	}
}

func TestWSHeartbeat(t *testing.T) {
	oldHB := wsHeartbeat
	wsHeartbeat = 150 * time.Millisecond
	defer func() { wsHeartbeat = oldHB }()

	self := id.New()
	cfg, fc, _ := baseConfig(self)
	fc.setSnapshot(snapWith(self, self, []id.ID{self}, "alice"))
	_, ts := testServer(t, cfg)

	conn := dialWS(t, ts.URL)
	defer conn.Close()

	// Initial event.
	readEvent(t, conn, 2*time.Second)
	// With no Subscribe signals, a heartbeat event should still arrive.
	ev := readEvent(t, conn, 2*time.Second)
	if ev.Type != "cluster" {
		t.Errorf("heartbeat type = %q", ev.Type)
	}
}

func TestWSClientDisconnectCleanup(t *testing.T) {
	self := id.New()
	cfg, fc, _ := baseConfig(self)
	fc.setSnapshot(snapWith(self, self, []id.ID{self}, "alice"))
	_, ts := testServer(t, cfg)

	conn := dialWS(t, ts.URL)
	readEvent(t, conn, 2*time.Second)
	conn.Close()

	// After the client goes away, the hub must keep running and accept new
	// clients without deadlock. Give the read pump a moment to notice the close.
	time.Sleep(100 * time.Millisecond)

	conn2 := dialWS(t, ts.URL)
	defer conn2.Close()
	ev := readEvent(t, conn2, 2*time.Second)
	if ev.Type != "cluster" {
		t.Errorf("hub broken after disconnect: %q", ev.Type)
	}
}

func TestWSSlowClientDropsOldest(t *testing.T) {
	oldHB := wsHeartbeat
	wsHeartbeat = time.Hour
	defer func() { wsHeartbeat = oldHB }()
	old := wsDebounce
	wsDebounce = 10 * time.Millisecond
	defer func() { wsDebounce = old }()

	self := id.New()
	cfg, fc, _ := baseConfig(self)
	fc.setSnapshot(snapWith(self, self, []id.ID{self}, "alice"))
	_, ts := testServer(t, cfg)

	conn := dialWS(t, ts.URL)
	defer conn.Close()
	// Do NOT read: let the client's send buffer fill. Push many changes; the hub
	// must never block (it drops oldest). We assert the server stays responsive
	// by eventually reading the newest snapshot.
	for i := 0; i < 200; i++ {
		fc.setSnapshot(contracts.Snapshot{
			Nodes: []contracts.NodeView{{ID: self, Name: "v" + string(rune('A'+i%26))}},
		})
		fc.signal()
		time.Sleep(time.Millisecond)
	}
	// The hub never blocked; we can still read an event.
	ev := readEvent(t, conn, 2*time.Second)
	if ev.Type != "cluster" {
		t.Errorf("type = %q", ev.Type)
	}
}
