import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { writable } from 'svelte/store'
import { get } from 'svelte/store'

// Mock the underlying single-feed poller (live.ts) so we can drive frames + count
// how many polls start. groupStatus.ts must dedup concurrent subscribers onto ONE
// feed and stop it when the last disposer runs.
const stop = vi.fn()
const feeds: { id: string; status: ReturnType<typeof writable> }[] = []
const pollOne = vi.fn((id: string) => {
  const status = writable<unknown>(null)
  feeds.push({ id, status })
  return { status, connected: writable(false), stop }
})

vi.mock('./live', () => ({
  pollGroupStatus: (id: string) => pollOne(id),
}))

import { pollGroupStatus, groupStatus } from './groupStatus'
import type { GroupStatus } from './types'

function sample(id: string): GroupStatus {
  return {
    groupId: id,
    masterNodeId: 'm',
    profile: { codec: 'pcm', fec: 'xorParity', rate: 48000, framesPerChunk: 480, fecK: 8, interleave: 4 },
    streamGen: 1,
    playing: true,
    members: [{ nodeId: 'm', syncErrorUs: 0, offsetUs: 0, driftRatio: 1, underruns: 0, clockQuality: 'good', online: true }],
  }
}

beforeEach(() => {
  feeds.length = 0
  pollOne.mockClear()
  stop.mockClear()
})

afterEach(() => vi.clearAllMocks())

describe('groupStatus multiplexer', () => {
  it('dedups concurrent subscribers onto a single underlying poll', () => {
    const d1 = pollGroupStatus('g1')
    const d2 = pollGroupStatus('g1')
    expect(pollOne).toHaveBeenCalledTimes(1) // one feed shared by two subscribers
    d1()
    expect(stop).not.toHaveBeenCalled() // still one ref left
    d2()
    expect(stop).toHaveBeenCalledTimes(1) // last disposer stops the poll
  })

  it('folds landed frames into the shared status map; disposer clears it', () => {
    const dispose = pollGroupStatus('g1')
    feeds[0].status.set(sample('g1'))
    expect(get(groupStatus).get('g1')?.groupId).toBe('g1')
    dispose()
    expect(get(groupStatus).has('g1')).toBe(false)
  })

  it('separate group ids get separate feeds', () => {
    const a = pollGroupStatus('g1')
    const b = pollGroupStatus('g2')
    expect(pollOne).toHaveBeenCalledTimes(2)
    a()
    b()
  })

  it('disposer is idempotent (double-dispose does not over-decrement)', () => {
    const d = pollGroupStatus('g1')
    d()
    d()
    expect(stop).toHaveBeenCalledTimes(1)
  })
})
