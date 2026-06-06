package web

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// wsConn is one connected browser. The hub broadcasts snapshots to send; a
// per-conn writer goroutine drains it so a slow client cannot stall the hub.
type wsConn struct {
	c    *websocket.Conn
	send chan any // StateSnapshot for broadcasts; reply envelopes for command acks/errors
}

// hub is the single websocket fan-out. It holds the live connections and pushes
// the snapshot at 3 Hz plus an immediate push whenever state changes.
type hub struct {
	mu    sync.Mutex
	conns map[*wsConn]struct{}
}

func newHub() *hub {
	return &hub{conns: make(map[*wsConn]struct{})}
}

func (h *hub) add(c *wsConn) {
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
}

func (h *hub) remove(c *wsConn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

// broadcast pushes a snapshot to every connection, dropping it for any client
// whose buffer is full (it will get the next tick). The hub never blocks on a
// slow client.
func (h *hub) broadcast(snap StateSnapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.conns {
		select {
		case c.send <- snap:
		default: // slow client; skip this frame
		}
	}
}

// sendTo queues a single message (e.g. an ack/error reply) to one connection,
// dropping it if the buffer is full.
func (c *wsConn) sendTo(msg any) {
	select {
	case c.send <- msg:
	default:
	}
}

// runHub drives the 3 Hz snapshot ticker and the immediate-push-on-change
// subscription. The 333 ms cadence is copied from mpvsync (not an A.12
// tunable); it is cheap on the Pi budget (tiny JSON). It runs until ctx is
// cancelled (server shutdown).
func (s *Server) runHub(ctx context.Context) {
	tick := time.NewTicker(333 * time.Millisecond) // ~3 Hz
	defer tick.Stop()

	var changed <-chan struct{}
	if s.deps.Changed != nil {
		changed = s.deps.Changed()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.hub.broadcast(s.BuildSnapshot())
		case <-changed:
			// Immediate out-of-band push on state change.
			s.hub.broadcast(s.BuildSnapshot())
		}
	}
}

// handleWS upgrades to a websocket, registers the connection, and runs the
// reader and writer loops for its lifetime.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Allow any origin. coder/websocket otherwise 403s any request whose
		// Origin host != Host header — which breaks the Vite dev proxy
		// (:5173 origin -> control port host) and any access via a different
		// hostname/IP than the browser used. This is a LAN appliance with no
		// cookie-based browser auth (no ambient authority to steal), so
		// cross-site WS hijacking is not a concern. NOTE: once the auth
		// middleware lands the WS upgrade should sit behind it so the endpoint
		// is not an unauthenticated state firehose (P0.2 keeps "*", no auth yet).
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		return
	}
	conn := &wsConn{c: c, send: make(chan any, 8)}
	s.hub.add(conn)
	defer func() {
		s.hub.remove(conn)
		_ = c.Close(websocket.StatusNormalClosure, "")
	}()

	ctx := r.Context()

	// Push the current snapshot immediately so a fresh client renders at once.
	conn.sendTo(s.BuildSnapshot())

	// Writer goroutine: drains send + a ping keepalive.
	go s.wsWriter(ctx, conn)

	// Reader loop: demux inbound commands. Unknown / malformed messages must not
	// drop the connection. Reading also drives pong handling for the library's
	// ping keepalive.
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		s.handleWSCommand(ctx, conn, data)
	}
}

// wsCommand is the envelope for inbound websocket commands. The skeleton
// decodes only the discriminator; the real command fields (send / transport /
// config edits) arrive with the API-handler pieces.
type wsCommand struct {
	T string `json:"t"`
}

// handleWSCommand dispatches a single inbound command. The skeleton ships no
// commands (their handlers land in later pieces); it parses the envelope and
// ignores everything so an early client cannot drop the connection by sending
// an unrecognised message.
func (s *Server) handleWSCommand(_ context.Context, _ *wsConn, data []byte) {
	var cmd wsCommand
	if err := json.Unmarshal(data, &cmd); err != nil {
		return
	}
	switch cmd.T {
	// No commands in the skeleton; later pieces add cases here.
	default:
	}
}

// wsWriter sends queued snapshots with a 5 s write deadline and pings every 15 s.
func (s *Server) wsWriter(ctx context.Context, conn *wsConn) {
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-conn.send:
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := wsjson.Write(wctx, conn.c, msg)
			cancel()
			if err != nil {
				_ = conn.c.Close(websocket.StatusInternalError, "write failed")
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := conn.c.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// BuildSnapshot assembles the StateSnapshot pushed over the websocket. Every
// deps.* read is nil-safe (zero values) so the server runs with a partially
// wired Deps during early bring-up and in tests.
func (s *Server) BuildSnapshot() StateSnapshot {
	snap := StateSnapshot{
		T:    "state",
		Self: s.deps.NodeID,
	}
	if s.deps.Status != nil {
		snap.Status = s.deps.Status()
		snap.Master = snap.Status.MasterID
	}
	if s.deps.State != nil {
		snap.Config = s.deps.State()
	}
	if s.deps.Transcodes != nil {
		snap.Streams = s.deps.Transcodes()
	}
	return snap
}
