import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { getNode, patchNode, calibratePlay, ApiError } from '../node'
import { session } from '../stores'

// Minimal Response-like stub matching what apiFetch reads (mirrors cluster.test).
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

function lastCall(spy: ReturnType<typeof vi.fn>): [string, RequestInit] {
  const c = spy.mock.calls[spy.mock.calls.length - 1]
  return [c[0] as string, (c[1] ?? {}) as RequestInit]
}

beforeEach(() => session.set({ authenticated: true, nodeId: 'n-self' }))
afterEach(() => vi.restoreAllMocks())

const NODE = {
  id: 'n-4d22',
  name: 'Hall',
  addrs: ['192.168.1.13'],
  channel: 'right',
  hwDelayUs: 8200,
  gainDb: -1.5,
  caps: {
    render: true,
    sinks: ['alsa', 'exec:aplay'],
    encode: ['pcm', 'opus'],
    decode: ['pcm', 'opus'],
    fec: ['none', 'xorParity', 'duplicate'],
    maxRate: 48000,
  },
}

describe('getNode (D.2)', () => {
  it('GETs /api/v1/nodes/{id}, parses {version,node}, no If-Match, structured caps', async () => {
    const spy = vi.fn(async () =>
      resp({ status: 200, etag: '"42"', body: { version: 42, node: NODE } }),
    )
    vi.stubGlobal('fetch', spy)
    const r = await getNode('n-4d22')
    const [path, init] = lastCall(spy)
    expect(path).toBe('/api/v1/nodes/n-4d22')
    expect(init.method ?? 'GET').toBe('GET')
    expect((init.headers as Record<string, string>)?.['If-Match']).toBeUndefined()
    expect(r.version).toBe(42)
    expect(r.node.caps.sinks).toEqual(['alsa', 'exec:aplay'])
    expect(r.node.caps.render).toBe(true)
  })
})

describe('patchNode (D.3)', () => {
  it('identity PATCH: body fields + If-Match + JSON content-type; returns {version,node}', async () => {
    const spy = vi.fn(async () =>
      resp({ status: 200, etag: '"47"', body: { version: 47, node: { ...NODE, name: 'Hallway' } } }),
    )
    vi.stubGlobal('fetch', spy)
    const r = await patchNode('n-4d22', { name: 'Hallway', channel: 'left', gainDb: -2, hwDelayUs: 1500 }, 46)
    const [path, init] = lastCall(spy)
    expect(path).toBe('/api/v1/nodes/n-4d22')
    expect(init.method).toBe('PATCH')
    const h = init.headers as Record<string, string>
    expect(h['If-Match']).toBe('46')
    expect(h['Content-Type']).toBe('application/json')
    expect(JSON.parse(init.body as string)).toEqual({
      name: 'Hallway',
      channel: 'left',
      gainDb: -2,
      hwDelayUs: 1500,
    })
    expect(r.version).toBe(47)
    expect(r.node.name).toBe('Hallway')
  })

  it('caps mask PATCH: carries capabilities verbatim (only changed fields)', async () => {
    const spy = vi.fn(async () => resp({ status: 200, body: { version: 48, node: NODE } }))
    vi.stubGlobal('fetch', spy)
    await patchNode('n-4d22', { capabilities: { sinks: ['alsa'], encode: ['pcm'] } }, 47)
    const body = JSON.parse(lastCall(spy)[1].body as string)
    expect(body).toEqual({ capabilities: { sinks: ['alsa'], encode: ['pcm'] } })
  })

  it('force render:false passes through unaltered', async () => {
    const spy = vi.fn(async () => resp({ status: 200, body: { version: 49, node: NODE } }))
    vi.stubGlobal('fetch', spy)
    await patchNode('n-4d22', { capabilities: { render: false } }, 48)
    expect(JSON.parse(lastCall(spy)[1].body as string).capabilities.render).toBe(false)
  })
})

describe('calibratePlay (F2.1)', () => {
  it('group variant: POST /calibrate/play {groupId,durationSec}, NO If-Match', async () => {
    const spy = vi.fn(async () =>
      resp({ status: 200, body: { playedOn: ['n-4d22'], durationSec: 10, warnings: [] } }),
    )
    vi.stubGlobal('fetch', spy)
    const r = await calibratePlay({ groupId: 'g-kitchen', durationSec: 10 })
    const [path, init] = lastCall(spy)
    expect(path).toBe('/api/v1/calibrate/play')
    expect(init.method).toBe('POST')
    expect((init.headers as Record<string, string>)['If-Match']).toBeUndefined()
    expect(JSON.parse(init.body as string)).toEqual({ groupId: 'g-kitchen', durationSec: 10 })
    expect(r.playedOn).toEqual(['n-4d22'])
  })

  it('nodes variant: {nodeIds,durationSec}', async () => {
    const spy = vi.fn(async () =>
      resp({ status: 200, body: { playedOn: ['n-1'], durationSec: 5, warnings: ['n-2 render=false'] } }),
    )
    vi.stubGlobal('fetch', spy)
    const r = await calibratePlay({ nodeIds: ['n-1', 'n-2'], durationSec: 5 })
    expect(JSON.parse(lastCall(spy)[1].body as string)).toEqual({ nodeIds: ['n-1', 'n-2'], durationSec: 5 })
    expect(r.warnings).toEqual(['n-2 render=false'])
  })

  it('rejects (client-side 400) when BOTH groupId and nodeIds set', async () => {
    const spy = vi.fn()
    vi.stubGlobal('fetch', spy)
    await expect(
      calibratePlay({ groupId: 'g', nodeIds: ['n'], durationSec: 10 }),
    ).rejects.toMatchObject({ code: 'invalid_request', status: 400 })
    expect(spy).not.toHaveBeenCalled()
  })

  it('rejects (client-side 400) when NEITHER groupId nor nodeIds set', async () => {
    const spy = vi.fn()
    vi.stubGlobal('fetch', spy)
    await expect(calibratePlay({ durationSec: 10 })).rejects.toMatchObject({
      code: 'invalid_request',
    })
    expect(spy).not.toHaveBeenCalled()
  })
})

describe('error envelope mapping', () => {
  const cases = [
    [404, 'not_found'],
    [409, 'version_conflict'],
    [422, 'unprocessable'],
    [502, 'proxy_failed'],
    [503, 'unavailable'],
  ] as const

  it.each(cases)('patchNode %i → ApiError{code:%s,status}', async (status, code) => {
    vi.stubGlobal('fetch', vi.fn(async () => resp({ status, body: { error: { code, message: `m ${code}` } } })))
    await expect(patchNode('n-x', { name: 'x' }, 1)).rejects.toMatchObject({
      name: 'ApiError',
      status,
      code,
    })
  })

  it('409 from patchNode surfaces version_conflict for reload+reapply', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => resp({ status: 409, body: { error: { code: 'version_conflict', message: 'stale' } } })),
    )
    await expect(patchNode('n', { gainDb: 0 }, 1)).rejects.toBeInstanceOf(ApiError)
    await expect(patchNode('n', { gainDb: 0 }, 1)).rejects.toMatchObject({ code: 'version_conflict' })
  })
})
