import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'

import { ApiError } from '../lib/groupActions'

const playGroup = vi.fn()
const stopGroup = vi.fn()
vi.mock('../lib/groupActions', async (orig) => {
  const real = (await orig()) as Record<string, unknown>
  return {
    ...real,
    playGroup: (...a: unknown[]) => playGroup(...a),
    stopGroup: (...a: unknown[]) => stopGroup(...a),
  }
})

// Controllable live status map (per-group). Created via vi.hoisted so the mock
// factory (hoisted above imports) can reference it.
const { statusStore } = await vi.hoisted(async () => {
  const { writable: w } = await import('svelte/store')
  return { statusStore: w<Map<string, unknown>>(new Map()) }
})
vi.mock('../lib/groupStatus', () => ({
  pollGroupStatus: () => () => {},
  groupStatus: statusStore,
}))

const refreshGroups = vi.fn()
vi.mock('../lib/groups', async (orig) => {
  const real = (await orig()) as Record<string, unknown>
  return { ...real, refreshGroups: (...a: unknown[]) => refreshGroups(...a) }
})

import Dashboard from './Dashboard.svelte'
import { __setSnapshotForTest } from '../lib/groups'
import { configVersion } from '../lib/stores'
import type { NodeRecord, GroupRecord, GroupStatus } from '../lib/types'

function node(id: string): NodeRecord {
  return { id, name: id, addrs: [], hwDelayUs: 0, channel: 'stereo', gainDb: 0, caps: { render: true, sinks: ['alsa'], encode: ['pcm'], decode: ['pcm'], fec: ['none'], maxRate: 48000 } }
}
function group(id: string, p: Partial<GroupRecord> = {}): GroupRecord {
  return { id, name: id, memberNodeIds: ['n1'], profile: { codec: 'pcm', fec: 'xorParity', rate: 48000, framesPerChunk: 480, fecK: 8, interleave: 4 }, media: { file: '', loop: false }, playing: false, ...p }
}
function status(id: string, playing: boolean): GroupStatus {
  return { groupId: id, masterNodeId: 'n1', profile: group(id).profile, streamGen: 1, playing, members: [{ nodeId: 'n1', syncErrorUs: 0, offsetUs: 0, driftRatio: 1, underruns: 0, clockQuality: 'good', online: true }] }
}

beforeEach(() => {
  configVersion.set(7)
  statusStore.set(new Map())
  refreshGroups.mockResolvedValue(undefined)
})

afterEach(() => {
  cleanup()
  vi.clearAllMocks()
})

describe('Dashboard / GroupCard (09 §3)', () => {
  it('empty: no groups → "create one in Groups" copy', async () => {
    __setSnapshotForTest([], [])
    render(Dashboard)
    await waitFor(() => expect(screen.getByText(/No groups yet/i)).toBeTruthy())
  })

  it('no media selected → Play disabled + "select media →" affordance', async () => {
    __setSnapshotForTest([group('g1', { media: { file: '', loop: false } })], [node('n1')])
    statusStore.set(new Map([['g1', status('g1', false)]]))
    render(Dashboard)
    await waitFor(() => expect(screen.getByText('g1')).toBeTruthy())
    expect(screen.getByText(/select media →/i)).toBeTruthy()
    const play = screen.getByText('▶ Play').closest('button') as HTMLButtonElement
    expect(play.disabled).toBe(true)
  })

  it('Play flow: click calls playGroup with If-Match; 502 → offline toast + error banner', async () => {
    __setSnapshotForTest([group('g1', { media: { file: 'song.mp3', loop: true } })], [node('n1')])
    statusStore.set(new Map([['g1', status('g1', false)]]))
    playGroup.mockRejectedValueOnce(new ApiError(502, 'proxy_failed', 'Master unreachable.'))
    render(Dashboard)
    await waitFor(() => expect(screen.getByText('▶ Play')).toBeTruthy())
    await fireEvent.click(screen.getByText('▶ Play'))
    await waitFor(() => expect(playGroup).toHaveBeenCalledWith('g1', 7))
    // 502 surfaces as a per-card error banner (proxy_failed).
    await waitFor(() => expect(screen.getByText('proxy_failed')).toBeTruthy())
  })

  it('a per-card error does not stop sibling cards from rendering', async () => {
    __setSnapshotForTest(
      [group('g1', { media: { file: 'a.mp3', loop: false } }), group('g2', { media: { file: 'b.mp3', loop: false } })],
      [node('n1')],
    )
    statusStore.set(new Map([['g1', status('g1', false)], ['g2', status('g2', false)]]))
    playGroup.mockRejectedValueOnce(new ApiError(409, 'conflict', 'No media selected.'))
    render(Dashboard)
    await waitFor(() => expect(screen.getByText('g1')).toBeTruthy())
    // Both cards render side by side.
    expect(screen.getByText('g2')).toBeTruthy()
    const firstPlay = screen.getAllByText('▶ Play')[0]
    await fireEvent.click(firstPlay)
    await waitFor(() => expect(screen.getByText('conflict')).toBeTruthy())
    // g2 card still present after g1's error.
    expect(screen.getByText('g2')).toBeTruthy()
  })
})
