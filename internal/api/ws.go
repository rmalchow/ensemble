package api

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"

	"ensemble/internal/contracts"
)

// wsEvent is the envelope for every server→client WS message (§9.2).
type wsEvent struct {
	Type string             `json:"type"` // "cluster"
	Data contracts.Snapshot `json:"data"`
}

// WS timing per §9.2. Debounce/heartbeat are vars so tests can shorten them.
var (
	wsDebounce  = 250 * time.Millisecond
	wsHeartbeat = 5 * time.Second
)

const (
	wsWriteWait = 10 * time.Second
	wsPongWait  = 60 * time.Second
	wsPingEvery = wsPongWait * 8 / 10
	wsSendBuf   = 8
)

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 8192,
	// Trusted LAN, SPA served same-origin; allow any origin (§9 no auth).
	CheckOrigin: func(*http.Request) bool { return true },
}

// wsHub fans cluster snapshots out to all connected clients. One goroutine
// (run) owns the client set and the debounce timer; clients are added/removed
// over channels — no per-client mutex.
type wsHub struct {
	cluster    Cluster
	log        *slog.Logger
	register   chan *wsClient
	unregister chan *wsClient
	done       chan struct{}
	wg         sync.WaitGroup
}

// wsClient is one upgraded connection. Its own write pump owns all writes; the
// hub pushes snapshots into send (buffered; slow client → drop oldest).
type wsClient struct {
	conn      *websocket.Conn
	send      chan wsEvent
	closeOnce sync.Once
}

func (cl *wsClient) closeSend() {
	cl.closeOnce.Do(func() { close(cl.send) })
}

func newWSHub(c Cluster, log *slog.Logger) *wsHub {
	return &wsHub{
		cluster:    c,
		log:        log,
		register:   make(chan *wsClient),
		unregister: make(chan *wsClient),
		done:       make(chan struct{}),
	}
}

// run owns the live client set. Selects on cluster changes (debounced), the
// heartbeat ticker, register/unregister, and done.
func (h *wsHub) run() {
	clients := map[*wsClient]struct{}{}
	var changes <-chan struct{}
	if h.cluster != nil {
		changes = h.cluster.Subscribe()
	}

	debounce := time.NewTimer(0)
	if !debounce.Stop() {
		<-debounce.C
	}
	debounceArmed := false

	heartbeat := time.NewTicker(wsHeartbeat)
	defer heartbeat.Stop()

	broadcast := func() {
		if h.cluster == nil {
			return
		}
		ev := wsEvent{Type: "cluster", Data: h.cluster.Snapshot()}
		for cl := range clients {
			select {
			case cl.send <- ev:
			default:
				// Slow client: drop the oldest queued event, enqueue the newest.
				select {
				case <-cl.send:
				default:
				}
				select {
				case cl.send <- ev:
				default:
				}
			}
		}
	}

	for {
		select {
		case <-h.done:
			for cl := range clients {
				cl.closeSend()
			}
			return

		case cl := <-h.register:
			clients[cl] = struct{}{}
			// Send the current snapshot immediately to the new client.
			if h.cluster != nil {
				select {
				case cl.send <- wsEvent{Type: "cluster", Data: h.cluster.Snapshot()}:
				default:
				}
			}

		case cl := <-h.unregister:
			if _, ok := clients[cl]; ok {
				delete(clients, cl)
				cl.closeSend()
			}

		case <-changes:
			if !debounceArmed {
				debounce.Reset(wsDebounce)
				debounceArmed = true
			}

		case <-debounce.C:
			debounceArmed = false
			broadcast()

		case <-heartbeat.C:
			broadcast()
		}
	}
}

// close stops run and closes all clients.
func (h *wsHub) close() {
	select {
	case <-h.done:
		// already closed
	default:
		close(h.done)
	}
	h.wg.Wait()
}

// handleWS upgrades the connection and spawns its read+write pumps (§9.2).
func (s *Server) handleWS(c echo.Context) error {
	conn, err := wsUpgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		// Upgrade already wrote an error response.
		return nil
	}
	cl := &wsClient{conn: conn, send: make(chan wsEvent, wsSendBuf)}

	select {
	case s.hub.register <- cl:
	case <-s.hub.done:
		conn.Close()
		return nil
	}

	s.hub.wg.Add(2)
	go s.wsWritePump(cl)
	go s.wsReadPump(cl)
	return nil
}

// wsWritePump owns all writes for one client: events, periodic pings.
func (s *Server) wsWritePump(cl *wsClient) {
	defer s.hub.wg.Done()
	ping := time.NewTicker(wsPingEvery)
	defer ping.Stop()
	defer cl.conn.Close()

	for {
		select {
		case ev, ok := <-cl.send:
			cl.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if !ok {
				// Hub closed our channel: send a close frame and exit.
				cl.conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
			if err := cl.conn.WriteJSON(ev); err != nil {
				s.unregister(cl)
				return
			}

		case <-ping.C:
			cl.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := cl.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				s.unregister(cl)
				return
			}
		}
	}
}

// wsReadPump drains client messages (mostly pongs/close) and refreshes the read
// deadline; on any read error it unregisters the client.
func (s *Server) wsReadPump(cl *wsClient) {
	defer s.hub.wg.Done()
	cl.conn.SetReadLimit(512)
	cl.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	cl.conn.SetPongHandler(func(string) error {
		cl.conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})
	for {
		if _, _, err := cl.conn.ReadMessage(); err != nil {
			s.unregister(cl)
			return
		}
	}
}

// unregister asks the hub to drop a client; safe if the hub is shutting down.
func (s *Server) unregister(cl *wsClient) {
	select {
	case s.hub.unregister <- cl:
	case <-s.hub.done:
	}
}
