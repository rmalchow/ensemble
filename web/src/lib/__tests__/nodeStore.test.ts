import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { get, writable } from 'svelte/store'

// Drive the groupStatus multiplexer via a mocked live.ts feed (mirrors
// groupStatus.test.ts) so the liveSyncErrorUs selector can be exercised.
const stop = vi.fn()
const feeds: { id: string; status: ReturnType<typeof writable> }[] = []
const pollOne = vi.fn((id: string) => {
  const status = writable<unknown>(null)
  feeds.push({ id, status })
  return { status, connected: writable(false), stop }
})
vi.mock('../live', () => ({ pollGroupStatus: (id: string) => pollOne(id) }))

// Mock the node API so save() can be asserted without a network stub.
const patchNodeMock = vi.fn()
vi.mock('../node', async (orig) => {
  const actual = (await orig()) as Record<string, unknown>
  return { ...actual, patchNode: (...a: unknown[]) => patchNodeMock(...a) }
})

import { pollGroupStatus } from '../groupStatus'
import { configVersion } from '../stores'
import {
  setLoaded,
  draft,
  isDirty,
  isRenderEnabled,
  liveSyncErrorUs,
  setField,
  toggleCapability,
  setForceControlOnly,
  revert,
  save,
  minimalPatch,
  __resetForTest,
} from '../nodeStore'
import type { NodeDetailView } from '../node'
import type { GroupStatus } from '../types'

function node(p: Partial<NodeDetailView> = {}): NodeDetailView {
  return {
    id: 'n-1',
    name: 'Hall',
    addrs: ['10.0.0.1'],
    hwDelayUs: 0,
    channel: 'stereo',
    gainDb: 0,
    online: true,
    groupId: 'g-1',
    isMaster: false,
    caps: {
      render: true,
      sinks: ['alsa', 'exec:aplay'],
      encode: ['pcm', 'opus'],
      decode: ['pcm', 'opus'],
      fec: ['none', 'xorParity'],
      maxRate: 48000,
    },
    ...p,
  }
}

beforeEach(() => {
  __resetForTest()
  feeds.length = 0
  pollOne.mockClear()
  stop.mockClear()
  patchNodeMock.mockReset()
  configVersion.set(42)
})
afterEach(() => vi.clearAllMocks())

describe('dirty tracking', () => {
  it('setField makes isDirty true; revert restores loaded + clears dirty', () => {
    setLoaded(node())
    expect(get(isDirty)).toBe(false)
    setField('name', 'Hallway')
    expect(get(isDirty)).toBe(true)
    revert()
    expect(get(isDirty)).toBe(false)
    expect(get(draft)).toEqual({})
  })

  it('setting a field back to the loaded value is NOT dirty', () => {
    setLoaded(node({ gainDb: -1.5 }))
    setField('gainDb', -1.5)
    expect(get(isDirty)).toBe(false)
  })

  it('minimalPatch only emits changed fields', () => {
    const n = node({ name: 'Hall', gainDb: 0 })
    expect(minimalPatch(n, { name: 'Hall', gainDb: -2 })).toEqual({ gainDb: -2 })
  })
})

describe('A↔B flip on force-control-only', () => {
  it('setForceControlOnly(true) sets render:false and isRenderEnabled→false (live)', () => {
    setLoaded(node())
    expect(get(isRenderEnabled)).toBe(true)
    setForceControlOnly(true)
    expect(get(draft).capabilities?.render).toBe(false)
    expect(get(isRenderEnabled)).toBe(false)
    expect(get(isDirty)).toBe(true)
  })

  it('clearing force-control-only restores render when a probed sink exists', () => {
    setLoaded(node())
    setForceControlOnly(true)
    expect(get(isRenderEnabled)).toBe(false)
    setForceControlOnly(false)
    expect(get(isRenderEnabled)).toBe(true)
  })
})

describe('capability mask edit', () => {
  it('toggleCapability removes a sink from the enabled set; re-enable restores', () => {
    setLoaded(node())
    toggleCapability('sinks', 'exec:aplay', false)
    expect(get(draft).capabilities?.sinks).toEqual(['alsa'])
    toggleCapability('sinks', 'exec:aplay', true)
    expect(get(draft).capabilities?.sinks?.sort()).toEqual(['alsa', 'exec:aplay'])
  })

  it('disabling the last sink flips isRenderEnabled false (previewRender)', () => {
    setLoaded(node({ caps: { ...node().caps, sinks: ['alsa'] } }))
    toggleCapability('sinks', 'alsa', false)
    expect(get(isRenderEnabled)).toBe(false)
  })
})

describe('liveSyncErrorUs selector', () => {
  function status(over: Partial<GroupStatus> = {}): GroupStatus {
    return {
      groupId: 'g-1',
      masterNodeId: 'm',
      profile: { codec: 'pcm', fec: 'xorParity', rate: 48000, framesPerChunk: 480, fecK: 8, interleave: 4 },
      streamGen: 1,
      playing: true,
      members: [
        { nodeId: 'n-1', syncErrorUs: 380, offsetUs: 0, driftRatio: 1, underruns: 0, clockQuality: 'good', online: true },
        { nodeId: 'm', syncErrorUs: 0, offsetUs: 0, driftRatio: 1, underruns: 0, clockQuality: 'good', online: true },
      ],
      ...over,
    }
  }

  it('selects this node’s members[].syncErrorUs from its group status', () => {
    setLoaded(node())
    const dispose = pollGroupStatus('g-1')
    feeds[0].status.set(status())
    expect(get(liveSyncErrorUs)).toBe(380)
    dispose()
  })

  it('master → null (it is the reference)', () => {
    setLoaded(node({ id: 'm' }))
    const dispose = pollGroupStatus('g-1')
    feeds[0].status.set(status())
    expect(get(liveSyncErrorUs)).toBeNull()
    dispose()
  })

  it('offline / no-group → null', () => {
    setLoaded(node({ online: false }))
    expect(get(liveSyncErrorUs)).toBeNull()
    setLoaded(node({ groupId: undefined }))
    expect(get(liveSyncErrorUs)).toBeNull()
  })
})

describe('save', () => {
  it('calls patchNode(id, minimal draft, configVersion); returns version; clears dirty', async () => {
    setLoaded(node())
    setField('name', 'Hallway')
    patchNodeMock.mockResolvedValue({ version: 43, node: node({ name: 'Hallway' }) })
    const v = await save()
    expect(patchNodeMock).toHaveBeenCalledWith('n-1', { name: 'Hallway' }, 42)
    expect(v).toBe(43)
    expect(get(isDirty)).toBe(false)
    expect(get(configVersion)).toBe(43)
  })

  it('save threads the capabilities mask through', async () => {
    setLoaded(node())
    toggleCapability('encode', 'opus', false)
    patchNodeMock.mockResolvedValue({ version: 44, node: node() })
    await save()
    expect(patchNodeMock).toHaveBeenCalledWith('n-1', { capabilities: { encode: ['pcm'] } }, 42)
  })
})
