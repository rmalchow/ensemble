package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// dialWS opens a websocket to url with a bounded handshake timeout.
func dialWS(t *testing.T, url string, header ...http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	opts := &websocket.DialOptions{}
	if len(header) > 0 {
		opts.HTTPHeader = header[0]
	}
	return websocket.Dial(ctx, url, opts)
}

// readSnapshot reads one StateSnapshot frame with a bounded deadline.
func readSnapshot(t *testing.T, c *websocket.Conn) StateSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var snap StateSnapshot
	if err := wsjson.Read(ctx, c, &snap); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	return snap
}

func closeWS(c *websocket.Conn) { _ = c.Close(websocket.StatusNormalClosure, "") }

func TestHubBroadcastAndSlowClientDrop(t *testing.T) {
	h := newHub()

	// A "fast" client with room in its buffer, and a "slow" client whose buffer
	// is pre-filled so broadcast must skip it without blocking.
	fast := &wsConn{send: make(chan any, 8)}
	slow := &wsConn{send: make(chan any, 1)}
	slow.send <- StateSnapshot{T: "prefill"} // fill the slow client's only slot
	h.add(fast)
	h.add(slow)

	done := make(chan struct{})
	go func() {
		h.broadcast(StateSnapshot{T: "state"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("broadcast blocked on a slow client")
	}

	// Fast client received the broadcast.
	select {
	case msg := <-fast.send:
		snap, ok := msg.(StateSnapshot)
		if !ok || snap.T != "state" {
			t.Fatalf("fast client got %#v, want StateSnapshot{T:state}", msg)
		}
	default:
		t.Fatal("fast client did not receive the broadcast")
	}

	// Slow client still holds only its prefill frame (broadcast was dropped).
	select {
	case msg := <-slow.send:
		if snap, _ := msg.(StateSnapshot); snap.T != "prefill" {
			t.Fatalf("slow client got %#v, want the prefill frame", msg)
		}
	default:
		t.Fatal("slow client lost its prefill frame")
	}
	if len(slow.send) != 0 {
		t.Fatalf("slow client buffer should be drained to 0, got %d", len(slow.send))
	}
}

func TestWSSnapshotOnConnect(t *testing.T) {
	changed := make(chan struct{}, 1)
	s := New(Deps{
		NodeID:  "node-1",
		Changed: func() <-chan struct{} { return changed },
		Status:  func() NodeStatus { return NodeStatus{Role: "solo", MasterID: "node-1"} },
		State:   func() ConfigView { return ConfigView{Version: 7} },
	}, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.runHub(ctx) // drives the Changed()-triggered out-of-band push

	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	c, _, err := dialWS(t, wsURL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer closeWS(c)

	// First frame is the on-connect snapshot.
	first := readSnapshot(t, c)
	if first.T != "state" {
		t.Fatalf("first frame t: got %q want state", first.T)
	}
	if first.Self != "node-1" || first.Master != "node-1" || first.Config.Version != 7 {
		t.Fatalf("snapshot fields wrong: %+v", first)
	}

	// Firing Changed() triggers an out-of-band push (the next frame arrives well
	// inside the 333ms tick window only because of the change signal).
	changed <- struct{}{}
	next := readSnapshot(t, c)
	if next.T != "state" {
		t.Fatalf("out-of-band frame t: got %q want state", next.T)
	}
}

func TestWSOriginAllowed(t *testing.T) {
	s := New(Deps{NodeID: "n"}, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.runHub(ctx)

	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	hdr := http.Header{}
	hdr.Set("Origin", "http://evil.example.com:5173")
	c, _, err := dialWS(t, wsURL, hdr)
	if err != nil {
		t.Fatalf("cross-origin dial should upgrade (OriginPatterns *): %v", err)
	}
	defer closeWS(c)
	if snap := readSnapshot(t, c); snap.T != "state" {
		t.Fatalf("first frame t: got %q want state", snap.T)
	}
}

// TestBuildSnapshotMarshals confirms the snapshot serialises with the expected
// discriminator and field names (the wire contract later pieces depend on).
func TestBuildSnapshotMarshals(t *testing.T) {
	s := New(Deps{
		NodeID: "n1",
		Status: func() NodeStatus { return NodeStatus{Role: "master", MasterID: "n1"} },
		State:  func() ConfigView { return ConfigView{Version: 3} },
		Transcodes: func() []TranscodeStatus {
			return []TranscodeStatus{{GroupID: "g1", Status: "streaming", Codec: "opus"}}
		},
	}, "")
	b, err := json.Marshal(s.BuildSnapshot())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, want := range []string{`"t":"state"`, `"self":"n1"`, `"master":"n1"`, `"version":3`, `"streams"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("snapshot json %s does not contain %s", got, want)
		}
	}
}
