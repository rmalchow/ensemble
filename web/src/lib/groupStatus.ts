// Per-group live telemetry store (09 §3 / 08 §G.2). Multiplexes the P0.4
// live.ts single-feed poller into one shared map keyed by group id, with
// reference-counted subscriptions so N visible cards for the same group share
// ONE ~1 Hz poll (dedup) and the poll stops when the last subscriber disposes.
//
// Pattern: live.ts gives the writable-store + reconnect shape (it prefers a WS
// stream if P2.7 ever wires one, falling back to ~1 Hz polling — see its
// connectWS dormant helper). This layer adds the keyed map + refcount the
// screens consume.

import { writable, type Readable } from 'svelte/store'
import { pollGroupStatus as pollOne, type LiveFeed } from './live'
import { setLiveness } from './groups'
import type { GroupStatus } from './types'

// groupStatus maps group id → latest GroupStatus (08 §G.2). Cards read this and
// re-render reactively as each ~1 Hz tick lands. A group with no live sample
// (never polled / offline) is simply absent from the map.
const statusMap = writable<Map<string, GroupStatus>>(new Map())
export const groupStatus: Readable<Map<string, GroupStatus>> = statusMap

// Per-group subscription bookkeeping: the live feed + a refcount + the unsub of
// the inner status subscription so concurrent subscribers share one poller.
interface Entry {
  feed: LiveFeed
  refs: number
  unsub: () => void
}
const entries = new Map<string, Entry>()

// pollGroupStatus subscribes a group id to the shared ~1 Hz poll and returns a
// disposer. Concurrent subscribers for the same id are deduped: the second call
// just bumps the refcount; the underlying poll starts on the first subscriber
// and stops when the last disposer runs. Each landed sample is folded into the
// shared statusMap and the per-node liveness map (so offline members dim across
// every card, 09 §3).
export function pollGroupStatus(id: string): () => void {
  let entry = entries.get(id)
  if (!entry) {
    const feed = pollOne(id)
    const unsub = feed.status.subscribe((s) => {
      if (s == null) return
      statusMap.update((m) => {
        const next = new Map(m)
        next.set(id, s)
        return next
      })
      // Fold per-member online flags into the shared liveness map.
      const live: Record<string, boolean> = {}
      for (const member of s.members) live[member.nodeId] = member.online
      setLiveness(live)
    })
    entry = { feed, refs: 0, unsub }
    entries.set(id, entry)
  }
  entry.refs++

  let disposed = false
  return () => {
    if (disposed) return
    disposed = true
    const e = entries.get(id)
    if (!e) return
    e.refs--
    if (e.refs <= 0) {
      e.unsub()
      e.feed.stop()
      entries.delete(id)
      statusMap.update((m) => {
        const next = new Map(m)
        next.delete(id)
        return next
      })
    }
  }
}
