import { describe, it, expect, beforeEach, vi } from 'vitest'
import { loadDashboard, quickPlay, quickStop } from './dashboard'
import { session } from './stores'
import type { GroupRecord, NodeRecord } from './types'

function resp(opts: { status?: number; body: unknown; etag?: string }): Response {
  const headers = new Headers()
  if (opts.etag) headers.set('ETag', opts.etag)
  const status = opts.status ?? 200
  return {
    status,
    ok: status >= 200 && status < 300,
    statusText: '',
    headers,
    json: async () => opts.body,
    text: async () => JSON.stringify(opts.body),
  } as unknown as Response
}

function node(id: string, p: Partial<NodeRecord> = {}): NodeRecord {
  return {
    id,
    name: id.toUpperCase(),
    addrs: [],
    hwDelayUs: 0,
    channel: 'stereo',
    gainDb: 0,
    caps: { render: true, sinks: ['alsa'], encode: ['pcm'], decode: ['pcm'], fec: ['none'], maxRate: 48000 },
    ...p,
  }
}
function group(id: string, members: string[], media = { file: '', loop: true }): GroupRecord {
  return {
    id,
    name: id,
    memberNodeIds: members,
    profile: { codec: 'pcm', fec: 'none', rate: 48000, framesPerChunk: 480, fecK: 8, interleave: 4 },
    media,
    playing: false,
  }
}

// Route a fetch by path to the right canned body. The four reads loadDashboard
// composes: cluster/info (C.1), nodes (D.1), groups (E.1), discovery (C.2).
function routedFetch(opts: {
  members?: { id: string; online: boolean }[]
  nodes: NodeRecord[]
  groups: GroupRecord[]
  versions?: { info?: number; nodes?: number; groups?: number }
}) {
  return vi.fn(async (input: string) => {
    const path = String(input)
    if (path.includes('/cluster/info'))
      return resp({ body: { version: opts.versions?.info ?? 10, cluster: { name: 'Home' }, counts: {} }, etag: `"${opts.versions?.info ?? 10}"` })
    if (path.includes('/discovery'))
      return resp({ body: { members: opts.members ?? [], discovered: [] } })
    if (path.includes('/nodes'))
      return resp({ body: { nodes: opts.nodes }, etag: `"${opts.versions?.nodes ?? 10}"` })
    if (path.includes('/groups'))
      return resp({ body: { groups: opts.groups }, etag: `"${opts.versions?.groups ?? 12}"` })
    throw new Error(`unexpected path ${path}`)
  })
}

beforeEach(() => {
  session.set({ authenticated: true, nodeId: 'n-1' })
  vi.restoreAllMocks()
})

describe('loadDashboard (C.1 + D.1 + E.1 + C.2)', () => {
  it('composes the model and captures the freshest version for If-Match', async () => {
    vi.stubGlobal(
      'fetch',
      routedFetch({
        nodes: [node('n1'), node('n2')],
        groups: [group('g1', ['n1', 'n2'])],
        members: [
          { id: 'n1', online: true },
          { id: 'n2', online: false },
        ],
        versions: { info: 10, nodes: 11, groups: 14 },
      }),
    )
    const { model, version } = await loadDashboard()
    expect(model.clusterName).toBe('Home')
    expect(Object.keys(model.nodesById)).toEqual(['n1', 'n2'])
    expect(model.groups).toHaveLength(1)
    expect(version).toBe(14) // max(10,11,14)
  })

  it('offlineMembers = nodes not in the online set, with last-known group', async () => {
    vi.stubGlobal(
      'fetch',
      routedFetch({
        nodes: [node('n1'), node('n2'), node('n3')],
        groups: [group('g1', ['n1', 'n2'])],
        members: [
          { id: 'n1', online: true },
          { id: 'n2', online: false },
          { id: 'n3', online: false },
        ],
      }),
    )
    const { model } = await loadDashboard()
    expect(model.onlineNodeIds).toEqual(new Set(['n1']))
    const off = model.offlineMembers.map((m) => [m.node.id, m.lastKnownGroupId])
    expect(off).toEqual([
      ['n2', 'g1'], // last-known group from membership
      ['n3', null], // unassigned
    ])
  })

  it('empty discovery → every node treated online (no false offline strip)', async () => {
    vi.stubGlobal(
      'fetch',
      routedFetch({ nodes: [node('n1')], groups: [group('g1', ['n1'])], members: [] }),
    )
    const { model } = await loadDashboard()
    expect(model.offlineMembers).toHaveLength(0)
    expect(model.onlineNodeIds.has('n1')).toBe(true)
  })
})

describe('quickPlay / quickStop', () => {
  it('quickPlay issues F.3 with stored {file, loop} and returns the new version', async () => {
    const spy = vi.fn(async (_input: string, _init?: RequestInit) => resp({ body: { version: 20, group: {} }, etag: '"20"' }))
    vi.stubGlobal('fetch', spy)
    const v = await quickPlay(group('g1', ['n1'], { file: 'a.mp3', loop: true }), 19)
    expect(v).toBe(20)
    const init = spy.mock.calls[0][1] as RequestInit
    expect(String(spy.mock.calls[0][0])).toBe('/api/v1/groups/g1/play')
    expect(JSON.parse(init.body as string)).toEqual({ file: 'a.mp3', loop: true })
    expect((init.headers as Record<string, string>)['If-Match']).toBe('19')
  })

  it('quickStop issues F.4 and returns the new version', async () => {
    const spy = vi.fn(async (_input: string, _init?: RequestInit) => resp({ body: { version: 21 }, etag: '"21"' }))
    vi.stubGlobal('fetch', spy)
    const v = await quickStop('g1', 20)
    expect(v).toBe(21)
    expect(String(spy.mock.calls[0][0])).toBe('/api/v1/groups/g1/stop')
  })
})
