import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { get } from 'svelte/store'
import {
  refreshCluster,
  caFingerprint,
  clusterName,
  configVersion,
  members,
  discovered,
  onlineCounts,
  rowState,
  setRowBusy,
  setRowError,
  clearRow,
  isSinkless,
  isMasterNoAudio,
} from './clusterStore'
import { ApiError, type MemberNode, type Capabilities } from './cluster'
import { session } from './stores'

function resp(status: number, body: unknown, etag?: string): Response {
  const headers = new Headers()
  if (etag) headers.set('ETag', etag)
  return {
    status,
    ok: status >= 200 && status < 300,
    statusText: '',
    headers,
    json: async () => body,
    text: async () => JSON.stringify(body),
  } as unknown as Response
}

function caps(render: boolean): Capabilities {
  return {
    render,
    sinks: render ? ['alsa'] : [],
    encode: ['pcm'],
    decode: ['pcm'],
    fec: ['none'],
    maxRate: 48000,
  }
}

// stubReads wires fetch to answer the three refreshCluster reads by path.
function stubReads(opts: {
  info?: unknown
  nodes?: unknown
  discovery?: unknown
}) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const path = (typeof input === 'string' ? input : input.toString()).split('?')[0]
      if (path === '/api/v1/cluster/info')
        return resp(200, opts.info ?? { version: 7, cluster: { name: 'home', caFingerprint: 'sha256:cafe', created: 't' }, counts: { nodes: 2, groups: 1 } }, '"7"')
      if (path === '/api/v1/nodes') return resp(200, opts.nodes ?? { version: 7, nodes: [] }, '"7"')
      if (path === '/api/v1/discovery') return resp(200, opts.discovery ?? { members: [], discovered: [] })
      throw new Error('unexpected path ' + path)
    }),
  )
}

beforeEach(() => {
  session.set({ authenticated: true, nodeId: 'n-self' })
  configVersion.set(undefined)
  // reset row state.
  for (const id of Object.keys(get(rowState))) clearRow(id)
})
afterEach(() => vi.restoreAllMocks())

describe('sink-less / master helpers', () => {
  const sink: MemberNode = { id: 'a', name: 'A', addrs: [], online: true, caps: caps(true) }
  const sinkless: MemberNode = { id: 'b', name: 'B', addrs: [], online: true, caps: caps(false) }
  it('isSinkless flags render===false only', () => {
    expect(isSinkless(sinkless)).toBe(true)
    expect(isSinkless(sink)).toBe(false)
  })
  it('isMasterNoAudio requires isMaster && !render', () => {
    expect(isMasterNoAudio({ ...sinkless, isMaster: true })).toBe(true)
    expect(isMasterNoAudio({ ...sinkless, isMaster: false })).toBe(false)
    expect(isMasterNoAudio({ ...sink, isMaster: true })).toBe(false)
  })
})

describe('refreshCluster derivations', () => {
  it('populates caFingerprint / clusterName / configVersion from C.1', async () => {
    stubReads({})
    await refreshCluster()
    expect(get(caFingerprint)).toBe('sha256:cafe')
    expect(get(clusterName)).toBe('home')
    expect(get(configVersion)).toBe(7)
  })

  it('joins liveness from discovery members[] into members[]', async () => {
    stubReads({
      nodes: {
        version: 7,
        nodes: [
          { id: 'n1', name: 'N1', addrs: ['ip1'], caps: caps(true) },
          { id: 'n2', name: 'N2', addrs: ['ip2'], caps: caps(false) },
        ],
      },
      discovery: {
        members: [
          { id: 'n1', name: 'N1', addrs: ['ip1'], state: 'member', online: true },
          { id: 'n2', name: 'N2', addrs: ['ip2'], state: 'member', online: false },
        ],
        discovered: [],
      },
    })
    await refreshCluster()
    const m = get(members)
    expect(m.find((x) => x.id === 'n1')!.online).toBe(true)
    expect(m.find((x) => x.id === 'n2')!.online).toBe(false)
  })

  it('onlineCounts = (#online, total)', async () => {
    stubReads({
      nodes: { version: 7, nodes: [{ id: 'n1', name: 'N1', addrs: [], caps: caps(true) }, { id: 'n2', name: 'N2', addrs: [], caps: caps(true) }] },
      discovery: {
        members: [
          { id: 'n1', name: 'N1', addrs: [], state: 'member', online: true },
          { id: 'n2', name: 'N2', addrs: [], state: 'member', online: false },
        ],
        discovered: [],
      },
    })
    await refreshCluster()
    expect(get(onlineCounts)).toEqual({ online: 1, total: 2 })
  })

  it('exposes discovered[]', async () => {
    stubReads({
      discovery: {
        members: [],
        discovered: [{ nodeId: 'd1', name: 'd', addrs: ['ip'], fingerprint: 'fp', state: 'uninitialized' }],
      },
    })
    await refreshCluster()
    expect(get(discovered)).toHaveLength(1)
    expect(get(discovered)[0].nodeId).toBe('d1')
  })
})

describe('rowState', () => {
  it('sets busy, clears on success, sets error, keyed by id (no bleed)', () => {
    setRowBusy('a', true)
    expect(get(rowState).a.busy).toBe(true)
    expect(get(rowState).b).toBeUndefined()

    setRowBusy('a', false)
    expect(get(rowState).a.busy).toBe(false)

    const e = new ApiError(401, 'unauthenticated', 'bad pin')
    setRowError('b', e)
    expect(get(rowState).b.error).toBe(e)
    // 'a' untouched by 'b' error.
    expect(get(rowState).a.error).toBeUndefined()
  })
})
