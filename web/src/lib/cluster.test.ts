import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import {
  getClusterInfo,
  getDiscovery,
  getNodes,
  adopt,
  takeover,
  forget,
  ApiError,
} from './cluster'
import { session } from './stores'

// Minimal Response-like stub matching what apiFetch reads.
function resp(opts: {
  status: number
  body?: unknown
  etag?: string
  statusText?: string
}): Response {
  const headers = new Headers()
  if (opts.etag) headers.set('ETag', opts.etag)
  const ok = opts.status >= 200 && opts.status < 300
  const text = opts.body === undefined ? '' : JSON.stringify(opts.body)
  return {
    status: opts.status,
    ok,
    statusText: opts.statusText ?? '',
    headers,
    json: async () => {
      if (opts.body === undefined) throw new Error('no body')
      return opts.body
    },
    text: async () => text,
  } as unknown as Response
}

// lastCall returns the [path, init] of the most recent fetch call.
function lastCall(spy: ReturnType<typeof vi.fn>): [string, RequestInit] {
  const c = spy.mock.calls[spy.mock.calls.length - 1]
  return [c[0] as string, (c[1] ?? {}) as RequestInit]
}

beforeEach(() => session.set({ authenticated: true, nodeId: 'n-self' }))
afterEach(() => vi.restoreAllMocks())

const NODE = {
  id: 'n-9b1d',
  name: 'Bedroom',
  addrs: ['192.168.1.55'],
  channel: 'stereo',
  hwDelayUs: 0,
  gainDb: 0,
  caps: {
    render: true,
    sinks: ['alsa'],
    encode: ['pcm'],
    decode: ['pcm'],
    fec: ['none', 'xorParity'],
    maxRate: 48000,
  },
}

describe('reads', () => {
  it('getClusterInfo GETs C.1 and parses cluster + counts', async () => {
    const spy = vi.fn(async () =>
      resp({
        status: 200,
        etag: '"42"',
        body: {
          version: 42,
          cluster: { name: 'home', caFingerprint: 'sha256:1b2c', created: 't' },
          counts: { nodes: 4, groups: 2 },
        },
      }),
    )
    vi.stubGlobal('fetch', spy)
    const info = await getClusterInfo()
    expect(lastCall(spy)[0]).toBe('/api/v1/cluster/info')
    expect((lastCall(spy)[1].method ?? 'GET')).toBe('GET')
    expect(info.cluster.caFingerprint).toBe('sha256:1b2c')
    expect(info.version).toBe(42)
    // Read-only: no If-Match.
    expect((lastCall(spy)[1].headers as Record<string, string>)?.['If-Match']).toBeUndefined()
  })

  it('getDiscovery GETs C.2 and parses members[] + discovered[]', async () => {
    const spy = vi.fn(async () =>
      resp({
        status: 200,
        body: {
          members: [{ id: 'n-7a3f', name: 'LR', addrs: ['192.168.1.21'], state: 'member', online: true }],
          discovered: [
            {
              nodeId: 'n-9b1d',
              name: 'ensemble-9b1d',
              addrs: ['192.168.1.55'],
              fingerprint: 'sha256:aa11',
              state: 'uninitialized',
              softwareVersion: '0.1.0',
            },
          ],
        },
      }),
    )
    vi.stubGlobal('fetch', spy)
    const d = await getDiscovery()
    expect(lastCall(spy)[0]).toBe('/api/v1/discovery')
    expect(d.members).toHaveLength(1)
    expect(d.discovered[0].nodeId).toBe('n-9b1d')
  })

  it('getNodes GETs D.1 and returns version + nodes[]', async () => {
    const spy = vi.fn(async () =>
      resp({ status: 200, etag: '"42"', body: { version: 42, nodes: [NODE] } }),
    )
    vi.stubGlobal('fetch', spy)
    const r = await getNodes()
    expect(lastCall(spy)[0]).toBe('/api/v1/nodes')
    expect(r.version).toBe(42)
    expect(r.nodes[0].id).toBe('n-9b1d')
  })
})

describe('writes', () => {
  it('adopt POSTs C.3 with body + If-Match + JSON content-type', async () => {
    const spy = vi.fn(async () =>
      resp({ status: 200, etag: '"45"', body: { version: 45, node: NODE } }),
    )
    vi.stubGlobal('fetch', spy)
    const r = await adopt(
      { nodeId: 'n-9b1d', addr: '192.168.1.55', fingerprint: 'sha256:aa11', pin: '1234', name: 'Bedroom' },
      44,
    )
    const [path, init] = lastCall(spy)
    expect(path).toBe('/api/v1/cluster/adopt')
    expect(init.method).toBe('POST')
    const h = init.headers as Record<string, string>
    expect(h['If-Match']).toBe('44')
    expect(h['Content-Type']).toBe('application/json')
    expect(JSON.parse(init.body as string)).toEqual({
      nodeId: 'n-9b1d',
      addr: '192.168.1.55',
      fingerprint: 'sha256:aa11',
      pin: '1234',
      name: 'Bedroom',
    })
    expect(r.version).toBe(45)
    expect(r.node.id).toBe('n-9b1d')
  })

  it('adopt sends the default PIN "0000" verbatim', async () => {
    const spy = vi.fn(async () => resp({ status: 200, body: { version: 1, node: NODE } }))
    vi.stubGlobal('fetch', spy)
    await adopt({ nodeId: 'n-9b1d', addr: '192.168.1.55', fingerprint: 'fp', pin: '0000' }, 1)
    expect(JSON.parse(lastCall(spy)[1].body as string).pin).toBe('0000')
  })

  it('takeover POSTs C.4 with force:true + If-Match', async () => {
    const spy = vi.fn(async () => resp({ status: 200, etag: '"50"', body: { version: 50, node: NODE } }))
    vi.stubGlobal('fetch', spy)
    const r = await takeover(
      { nodeId: 'n-9b1d', addr: '192.168.1.55', fingerprint: 'fp', pin: '0000', force: true },
      49,
    )
    const [path, init] = lastCall(spy)
    expect(path).toBe('/api/v1/cluster/takeover')
    expect((init.headers as Record<string, string>)['If-Match']).toBe('49')
    expect(JSON.parse(init.body as string).force).toBe(true)
    expect(r.version).toBe(50)
  })

  it('forget POSTs C.5 with If-Match and returns affectedGroups', async () => {
    const spy = vi.fn(async () =>
      resp({
        status: 200,
        etag: '"46"',
        body: { version: 46, removedNodeId: 'n-9b1d', affectedGroups: ['g-kitchen'] },
      }),
    )
    vi.stubGlobal('fetch', spy)
    const r = await forget('n-9b1d', 45)
    const [path, init] = lastCall(spy)
    expect(path).toBe('/api/v1/nodes/n-9b1d/forget')
    expect(init.method).toBe('POST')
    expect((init.headers as Record<string, string>)['If-Match']).toBe('45')
    expect(r.removedNodeId).toBe('n-9b1d')
    expect(r.affectedGroups).toEqual(['g-kitchen'])
  })
})

describe('error envelope mapping', () => {
  const cases = [
    [401, 'unauthenticated'],
    [403, 'forbidden'],
    [404, 'not_found'],
    [409, 'version_conflict'],
    [422, 'unprocessable'],
    [502, 'proxy_failed'],
  ] as const

  it.each(cases)('forget %i → ApiError{code:%s,status}', async (status, code) => {
    // 401 redirects; stub history/location so apiFetch's redirect path is inert.
    vi.stubGlobal('fetch', vi.fn(async () => resp({ status, body: { error: { code, message: `m ${code}` } } })))
    await expect(forget('n-x', 1)).rejects.toMatchObject({ name: 'ApiError', status, code })
  })

  it('409 from a write surfaces version_conflict for reload+reapply', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => resp({ status: 409, body: { error: { code: 'version_conflict', message: 'stale' } } })),
    )
    await expect(adopt({ nodeId: 'n', addr: 'a', fingerprint: 'f', pin: '0000' }, 1)).rejects.toMatchObject({
      code: 'version_conflict',
    })
  })

  it('non-JSON body falls back to statusText-based ApiError', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => resp({ status: 502, statusText: 'Bad Gateway' })),
    )
    const err = await forget('n-x', 1).catch((e) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err.code).toBe('proxy_failed')
    expect(err.message).toBe('Bad Gateway')
  })
})
