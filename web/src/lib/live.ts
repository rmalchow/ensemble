// Live per-group status feed (09 §0, 08 G.2). The MVP path is ~1 Hz polling of
// GET /api/v1/groups/{id}/status — 08 defines no streaming endpoint, so the
// WebSocket reconnect helper (the mpvsync ws.ts shape: open/scheduleReconnect/
// connect, capped backoff) ships DORMANT, ready to point at a future per-group
// stream without a rewrite.

import { writable, type Readable } from 'svelte/store'
import { getGroupStatus } from './api'
import { liveConnected } from './stores'
import type { GroupStatus } from './types'

// POLL_MS is the ~1 Hz polling cadence (09 §0).
const POLL_MS = 1000

export interface LiveFeed {
  status: Readable<GroupStatus | null>
  connected: Readable<boolean>
  stop: () => void
}

// pollGroupStatus starts a ~1 Hz poll of one group's live status. The returned
// feed exposes a status store, a connected flag, and a disposer. Errors flip
// `connected` to false (offline treatment, 09 §3) without tearing down the loop
// — the next tick retries. A 401 is handled centrally by apiFetch (redirect to
// /login); we just stop reporting.
export function pollGroupStatus(id: string): LiveFeed {
  const status = writable<GroupStatus | null>(null)
  const connected = writable<boolean>(false)
  let stopped = false
  let timer: ReturnType<typeof setTimeout> | undefined
  let ctrl: AbortController | null = null

  const tick = async () => {
    if (stopped) return
    ctrl = new AbortController()
    try {
      const s = await getGroupStatus(id, ctrl.signal)
      if (stopped) return
      status.set(s)
      connected.set(true)
      liveConnected.set(true)
    } catch {
      if (stopped) return
      connected.set(false)
      liveConnected.set(false)
    } finally {
      if (!stopped) timer = setTimeout(tick, POLL_MS)
    }
  }
  void tick()

  return {
    status,
    connected,
    stop() {
      stopped = true
      if (timer) clearTimeout(timer)
      ctrl?.abort()
      liveConnected.set(false)
    },
  }
}

// ---- Dormant WebSocket reconnect helper -----------------------------------
// Reused from the mpvsync ws.ts shape (capped exponential backoff, base 500 ms /
// max 10 s). Not used by the MVP polling path; kept so a backend stream can be
// enabled by wiring `onFrame` without re-deriving the reconnect logic.

const MAX_BACKOFF_MS = 10000
const BASE_BACKOFF_MS = 500

export interface WSOptions {
  url: string
  onFrame: (data: unknown) => void
  onOpen?: () => void
  onClose?: () => void
  // setTimeout/clearTimeout/WebSocket are injectable for tests.
  setTimeoutFn?: (fn: () => void, ms: number) => ReturnType<typeof setTimeout>
  clearTimeoutFn?: (h: ReturnType<typeof setTimeout>) => void
  socketFactory?: (url: string) => WebSocket
}

// connectWS opens a socket and auto-reconnects with capped backoff. Returns a
// disposer that stops the timer and closes the socket. The delay sequence is
// 500 → 1000 → 2000 → … → 10000 (capped).
export function connectWS(opts: WSOptions): () => void {
  const setT = opts.setTimeoutFn ?? ((fn, ms) => setTimeout(fn, ms))
  const clearT = opts.clearTimeoutFn ?? ((h) => clearTimeout(h))
  const make = opts.socketFactory ?? ((u) => new WebSocket(u))

  let sock: WebSocket | null = null
  let backoff = BASE_BACKOFF_MS
  let closed = false
  let reconnectTimer: ReturnType<typeof setTimeout> | undefined

  const open = () => {
    if (closed) return
    sock = make(opts.url)
    sock.addEventListener('open', () => {
      backoff = BASE_BACKOFF_MS
      opts.onOpen?.()
    })
    sock.addEventListener('message', (ev: MessageEvent) => {
      try {
        opts.onFrame(JSON.parse(ev.data))
      } catch {
        // ignore non-JSON frames
      }
    })
    sock.addEventListener('close', () => {
      opts.onClose?.()
      if (!closed) scheduleReconnect()
    })
    sock.addEventListener('error', () => sock?.close())
  }

  const scheduleReconnect = () => {
    if (reconnectTimer) clearT(reconnectTimer)
    reconnectTimer = setT(open, backoff)
    backoff = Math.min(backoff * 2, MAX_BACKOFF_MS)
  }

  open()

  return () => {
    closed = true
    if (reconnectTimer) clearT(reconnectTimer)
    sock?.close()
  }
}
