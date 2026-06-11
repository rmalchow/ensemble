import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { cluster, connect, disconnect } from "./ws.svelte.js";

// Minimal mock WebSocket capturing handlers and readyState.
class MockWS {
  static instances = [];
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;
  constructor(url) {
    this.url = url;
    this.readyState = 0; // CONNECTING
    this.onopen = null;
    this.onmessage = null;
    this.onerror = null;
    this.onclose = null;
    MockWS.instances.push(this);
  }
  open() {
    this.readyState = 1;
    this.onopen && this.onopen();
  }
  message(data) {
    this.onmessage && this.onmessage({ data });
  }
  close() {
    this.readyState = 3;
    this.onclose && this.onclose();
  }
}

beforeEach(() => {
  MockWS.instances = [];
  global.WebSocket = MockWS;
  global.window = {
    location: { protocol: "http:", host: "h:8080" },
  };
  vi.useFakeTimers();
  // reset store
  cluster.snapshot = { nodes: [], groups: [] };
  cluster.status = "connecting";
  cluster.receivedAt = 0;
});

afterEach(() => {
  disconnect();
  vi.useRealTimers();
});

describe("wsURL via connect", () => {
  it("derives ws from http page", () => {
    connect();
    expect(MockWS.instances[0].url).toBe("ws://h:8080/api/ws");
  });
  it("derives wss from https page", () => {
    global.window.location.protocol = "https:";
    connect();
    expect(MockWS.instances[0].url).toBe("wss://h:8080/api/ws");
  });
});

describe("open + message", () => {
  it("open sets status=open", () => {
    connect();
    MockWS.instances[0].open();
    expect(cluster.status).toBe("open");
  });
  it("cluster message updates snapshot and receivedAt", () => {
    connect();
    const ws = MockWS.instances[0];
    ws.open();
    const data = { nodes: [{ id: "a" }], groups: [] };
    ws.message(JSON.stringify({ type: "cluster", data }));
    expect(cluster.snapshot).toEqual(data);
    expect(cluster.receivedAt).toBeGreaterThan(0);
  });
  it("unknown type is ignored", () => {
    connect();
    const ws = MockWS.instances[0];
    ws.open();
    cluster.snapshot = { nodes: [], groups: [] };
    ws.message(JSON.stringify({ type: "other", data: { x: 1 } }));
    expect(cluster.snapshot).toEqual({ nodes: [], groups: [] });
  });
});

describe("reconnect", () => {
  it("onclose sets reconnecting and schedules a new socket", () => {
    connect();
    const ws = MockWS.instances[0];
    ws.open();
    ws.close();
    expect(cluster.status).toBe("reconnecting");
    expect(MockWS.instances.length).toBe(1);
    vi.advanceTimersByTime(500);
    expect(MockWS.instances.length).toBe(2);
  });

  // measure the reconnect delay after the latest socket was closed: a new
  // socket must appear exactly at one of the geometric backoff steps.
  function reconnectDelay() {
    const before = MockWS.instances.length;
    let elapsed = 0;
    for (const step of [500, 1000, 2000, 4000, 5000, 5000]) {
      const wait = step - elapsed;
      if (wait > 1) {
        vi.advanceTimersByTime(wait - 1);
        if (MockWS.instances.length > before) return step - 1; // too early
      }
      vi.advanceTimersByTime(wait > 1 ? 1 : wait);
      elapsed = step;
      if (MockWS.instances.length > before) return step;
    }
    return Infinity;
  }

  it("backoff grows geometrically and caps at 5s", () => {
    connect();
    const seen = [];
    for (let i = 0; i < 5; i++) {
      const ws = MockWS.instances[MockWS.instances.length - 1];
      ws.close();
      const d = reconnectDelay();
      seen.push(d);
    }
    expect(seen[0]).toBe(500);
    expect(seen[1]).toBe(1000);
    expect(seen[2]).toBe(2000);
    expect(seen[3]).toBe(4000);
    expect(seen[4]).toBe(5000);
    // one more stays capped
    MockWS.instances[MockWS.instances.length - 1].close();
    expect(reconnectDelay()).toBe(5000);
  });

  it("abandons a stalled connect attempt and retries", () => {
    connect();
    const ws = MockWS.instances[0];
    expect(ws.readyState).toBe(MockWS.CONNECTING);
    // never opens: after the connect timeout the stalled socket is force-closed...
    vi.advanceTimersByTime(4000);
    expect(ws.readyState).toBe(MockWS.CLOSED);
    expect(cluster.status).toBe("reconnecting");
    // ...and a fresh attempt follows on the backoff.
    vi.advanceTimersByTime(500);
    expect(MockWS.instances.length).toBe(2);
  });

  it("recovers to open when a later attempt connects", () => {
    connect();
    MockWS.instances[0].close(); // node down
    vi.advanceTimersByTime(500);
    const ws = MockWS.instances[1];
    ws.open(); // node back
    expect(cluster.status).toBe("open");
  });

  it("clean open resets backoff", () => {
    connect();
    // grow backoff
    MockWS.instances[0].close();
    reconnectDelay(); // 500
    MockWS.instances[MockWS.instances.length - 1].close();
    reconnectDelay(); // 1000
    // a clean open resets it
    const ws = MockWS.instances[MockWS.instances.length - 1];
    ws.open();
    expect(cluster.status).toBe("open");
    ws.close();
    expect(reconnectDelay()).toBe(500);
  });

  it("second connect() while open is a no-op", () => {
    connect();
    MockWS.instances[0].open();
    connect();
    expect(MockWS.instances.length).toBe(1);
  });
});
