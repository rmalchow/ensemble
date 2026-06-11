// Cluster store + websocket connection state (J arch §3.1). One websocket, the
// single writer of cluster.snapshot. Auto-reconnect with capped backoff.

// Reactive singleton; components import and read fields directly.
export const cluster = $state({
  snapshot: { nodes: [], groups: [] }, // latest Snapshot; empty until first frame
  status: "connecting", // "connecting" | "open" | "reconnecting" | "closed"
  lastError: "", // last ws error message, for the header
  receivedAt: 0, // ms epoch of last cluster frame (staleness hint)
});

let socket = null;
let backoff = 500; // ms; 0.5→1→2→4→max
const MAX_BACKOFF = 5000; // cap retry gap so a recovered node flips back to green promptly
const CONNECT_TIMEOUT = 4000; // abandon a stalled attempt instead of waiting out the TCP timeout
let reconnectTimer = null;
let connectTimer = null;
let stopped = false;

function clearConnectTimer() {
  if (connectTimer) {
    clearTimeout(connectTimer);
    connectTimer = null;
  }
}

// wsURL derives ws(s)://<host>/api/ws from the page location.
function wsURL() {
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return proto + "//" + window.location.host + "/api/ws";
}

function scheduleReconnect() {
  if (stopped) return;
  if (reconnectTimer) return;
  const delay = backoff;
  backoff = Math.min(backoff * 2, MAX_BACKOFF);
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    open();
  }, delay);
}

function open() {
  if (stopped) return;
  let ws;
  try {
    ws = new WebSocket(wsURL());
  } catch (e) {
    cluster.lastError = e && e.message ? e.message : "ws open failed";
    cluster.status = "reconnecting";
    scheduleReconnect();
    return;
  }
  socket = ws;
  // Bound the connect attempt: an UNREACHABLE host (no SYN-ACK) would otherwise
  // leave the socket CONNECTING for the browser's long TCP timeout, stalling
  // recovery. Force-close a stalled attempt so onclose → scheduleReconnect cycles
  // and we notice the node returning within ~CONNECT_TIMEOUT + backoff.
  connectTimer = setTimeout(() => {
    if (ws.readyState === WebSocket.CONNECTING) {
      try {
        ws.close();
      } catch {
        // ignore
      }
    }
  }, CONNECT_TIMEOUT);
  ws.onopen = () => {
    clearConnectTimer();
    cluster.status = "open";
    cluster.lastError = "";
    backoff = 500; // reset on a clean open
  };
  ws.onmessage = (ev) => {
    let msg;
    try {
      msg = JSON.parse(ev.data);
    } catch {
      return; // ignore non-JSON
    }
    if (msg && msg.type === "cluster" && msg.data) {
      cluster.snapshot = msg.data;
      cluster.receivedAt = Date.now();
      rememberNodes(msg.data);
    }
    // unknown types ignored (forward-compat)
  };
  ws.onerror = () => {
    cluster.lastError = "websocket error";
  };
  ws.onclose = () => {
    clearConnectTimer();
    if (socket === ws) socket = null;
    if (stopped) {
      cluster.status = "closed";
      return;
    }
    cluster.status = "reconnecting";
    scheduleReconnect();
  };
}

// --- known-node roster (resilience: reach the UI via another node) -------------
// We persist every node's name + candidate HTTP origins so that when THIS node
// (the one serving the page) drops, the UI can still offer links to its peers.
// Addresses are NOT pre-filtered here — the banner probes each before showing it,
// so internal/unreachable CIDRs are dropped by an actual reachability test.
const ROSTER_KEY = "ensemble.roster";

// originsFor turns a node's self-reported CIDRs + httpPort into http(s) origins,
// stripping the mask and bracketing IPv6. Same scheme as the current page.
function originsFor(node) {
  const port = node && node.httpPort;
  if (!port) return [];
  const proto = window.location.protocol === "https:" ? "https:" : "http:";
  const out = [];
  for (const cidr of node.addrs || []) {
    const ip = String(cidr).split("/")[0].trim();
    if (!ip) continue;
    const host = ip.includes(":") ? `[${ip}]` : ip; // bracket IPv6
    out.push(`${proto}//${host}:${port}`);
  }
  return out;
}

function rememberNodes(snap) {
  try {
    const roster = (snap.nodes || [])
      .map((n) => ({ id: n.id, name: n.name || "", origins: originsFor(n) }))
      .filter((n) => n.origins.length);
    if (roster.length) localStorage.setItem(ROSTER_KEY, JSON.stringify(roster));
  } catch {
    // localStorage unavailable / quota — non-fatal
  }
}

// knownNodes returns the last-remembered roster ([{id, name, origins}]). The
// in-memory snapshot holds the same data while the page lives; localStorage lets
// it survive a reload served by a different (still-alive) node.
export function knownNodes() {
  try {
    const v = JSON.parse(localStorage.getItem(ROSTER_KEY) || "[]");
    return Array.isArray(v) ? v : [];
  } catch {
    return [];
  }
}

// connect opens the websocket and wires auto-reconnect. Idempotent while a
// socket is live.
export function connect() {
  stopped = false;
  if (socket && (socket.readyState === 0 || socket.readyState === 1)) return;
  backoff = 500;
  open();
}

// disconnect closes the socket and stops reconnect (HMR teardown).
export function disconnect() {
  stopped = true;
  if (reconnectTimer) {
    clearTimeout(reconnectTimer);
    reconnectTimer = null;
  }
  clearConnectTimer();
  if (socket) {
    try {
      socket.close();
    } catch {
      // ignore
    }
    socket = null;
  }
  cluster.status = "closed";
}
