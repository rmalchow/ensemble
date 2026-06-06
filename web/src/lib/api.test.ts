import { describe, it, expect, beforeEach, vi } from 'vitest'
import { get } from 'svelte/store'
import { apiFetch, ApiError } from './api'
import { session } from './stores'

// Minimal Response-like stub matching what apiFetch reads.
function resp(opts: {
  status: number
  ok?: boolean
  body?: unknown
  etag?: string
  statusText?: string
}): Response {
  const headers = new Headers()
  if (opts.etag) headers.set('ETag', opts.etag)
  const ok = opts.ok ?? (opts.status >= 200 && opts.status < 300)
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

beforeEach(() => {
  session.set(null)
  vi.restoreAllMocks()
})

describe('apiFetch envelope decode', () => {
  const cases = [
    [400, 'bad_request'],
    [403, 'forbidden'],
    [404, 'not_found'],
    [409, 'version_conflict'],
    [412, 'precondition_required'],
    [422, 'unprocessable'],
    [502, 'proxy_failed'],
    [503, 'unavailable'],
  ] as const

  it.each(cases)('status %i with envelope → ApiError code %s', async (status, code) => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        resp({ status, body: { error: { code, message: `msg ${code}` } } }),
      ),
    )
    await expect(apiFetch('/api/v1/x')).rejects.toMatchObject({
      name: 'ApiError',
      status,
      code,
      message: `msg ${code}`,
    })
  })

  it('falls back to a status-derived code when the body lacks an envelope', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => resp({ status: 404, statusText: 'Not Found' })))
    const err = await apiFetch('/api/v1/x').catch((e) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err.code).toBe('not_found')
    expect(err.status).toBe(404)
  })

  it('2xx returns {data} and parses ETag → version', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => resp({ status: 200, body: { hi: 1 }, etag: '"42"' })),
    )
    const r = await apiFetch<{ hi: number }>('/api/v1/x')
    expect(r.data).toEqual({ hi: 1 })
    expect(r.version).toBe(42)
  })

  it('2xx without ETag leaves version undefined', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => resp({ status: 200, body: { hi: 1 } })))
    const r = await apiFetch('/api/v1/x')
    expect(r.version).toBeUndefined()
  })
})

describe('apiFetch If-Match', () => {
  it('sets the If-Match header from ifMatch', async () => {
    const spy = vi.fn(
      async (_input: RequestInfo | URL, _init?: RequestInit) =>
        resp({ status: 200, body: {} }),
    )
    vi.stubGlobal('fetch', spy)
    await apiFetch('/api/v1/x', { method: 'PATCH', body: { a: 1 }, ifMatch: 42 })
    const init = spy.mock.calls[0][1] as RequestInit
    const headers = init.headers as Record<string, string>
    expect(headers['If-Match']).toBe('42')
    expect(headers['Content-Type']).toBe('application/json')
  })

  it('a 409 surfaces ApiError.code === "version_conflict"', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        resp({ status: 409, body: { error: { code: 'version_conflict', message: 'stale' } } }),
      ),
    )
    await expect(apiFetch('/api/v1/x', { method: 'PATCH', ifMatch: 1 })).rejects.toMatchObject({
      code: 'version_conflict',
      status: 409,
    })
  })

  it('a 401 clears session and redirects to /login', async () => {
    session.set({ authenticated: true, nodeId: 'n-1' })
    const replaceSpy = vi.fn()
    // history.replaceState is what navigate() calls under replace.
    vi.stubGlobal('history', { replaceState: replaceSpy, pushState: vi.fn() })
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        resp({ status: 401, body: { error: { code: 'unauthenticated', message: 'expired' } } }),
      ),
    )
    await expect(apiFetch('/api/v1/x')).rejects.toMatchObject({ status: 401 })
    expect(get(session)).toBeNull()
    expect(replaceSpy).toHaveBeenCalled()
    const dest = replaceSpy.mock.calls[0][2] as string
    expect(dest.startsWith('/login')).toBe(true)
  })
})
