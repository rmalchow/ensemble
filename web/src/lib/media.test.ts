import { describe, it, expect, beforeEach, vi } from 'vitest'
import { listMedia, selectAndPlay, setMedia, stop } from './media'
import { ApiError } from './api'
import { session } from './stores'

// Minimal Response-like stub matching what apiFetch reads (mirrors api.test.ts).
function resp(opts: { status: number; ok?: boolean; body?: unknown; etag?: string }): Response {
  const headers = new Headers()
  if (opts.etag) headers.set('ETag', opts.etag)
  const ok = opts.ok ?? (opts.status >= 200 && opts.status < 300)
  const text = opts.body === undefined ? '' : JSON.stringify(opts.body)
  return {
    status: opts.status,
    ok,
    statusText: '',
    headers,
    json: async () => {
      if (opts.body === undefined) throw new Error('no body')
      return opts.body
    },
    text: async () => text,
  } as unknown as Response
}

// captures the last fetch call so tests can assert method/path/body/headers.
function spyFetch(body: unknown = {}, status = 200, etag?: string) {
  const spy = vi.fn(async (_input: string, _init?: RequestInit) => resp({ status, body, etag }))
  vi.stubGlobal('fetch', spy)
  return spy
}
function lastCall(spy: ReturnType<typeof spyFetch>) {
  const call = spy.mock.calls[spy.mock.calls.length - 1]
  const input = call[0]
  const init = call[1]
  const headers = (init?.headers ?? {}) as Record<string, string>
  const parsedBody = init?.body ? JSON.parse(init.body as string) : undefined
  return { path: input, method: init?.method ?? 'GET', headers, body: parsedBody }
}

beforeEach(() => {
  session.set({ authenticated: true, nodeId: 'n-1' })
  vi.restoreAllMocks()
})

describe('listMedia (08 F.1)', () => {
  it('issues GET /api/v1/media?node=<id>', async () => {
    const spy = spyFetch({ nodeId: 'n-7', files: [{ file: 'a.mp3' }] })
    const r = await listMedia('n-7')
    const c = lastCall(spy)
    expect(c.method).toBe('GET')
    expect(c.path).toBe('/api/v1/media?node=n-7')
    expect(r.data.files).toHaveLength(1)
  })

  it('threads the browse path: GET /api/v1/media?node=<id>&path=<dir>', async () => {
    const spy = spyFetch({ nodeId: 'n-7', path: 'albums', dirs: [], files: [] })
    const r = await listMedia('n-7', 'albums')
    expect(lastCall(spy).path).toBe('/api/v1/media?node=n-7&path=albums')
    expect(r.data.path).toBe('albums')
  })

  it('omits the query when nodeId is undefined (defaults server-side)', async () => {
    const spy = spyFetch({ nodeId: 'self', files: [] })
    await listMedia()
    expect(lastCall(spy).path).toBe('/api/v1/media')
  })

  it('a 502 surfaces proxy_failed (offline target)', async () => {
    spyFetch({ error: { code: 'proxy_failed', message: 'unreachable' } }, 502)
    await expect(listMedia('n-7')).rejects.toMatchObject({ code: 'proxy_failed', status: 502 })
  })
})

describe('selectAndPlay / setMedia / stop (08 F.2–F.4)', () => {
  const cases = [
    {
      name: 'selectAndPlay → POST /play with {file, loop}',
      run: (v: number) => selectAndPlay('g1', 'song.mp3', true, v),
      path: '/api/v1/groups/g1/play',
      body: { file: 'song.mp3', loop: true },
    },
    {
      name: 'setMedia → POST /media with {file, loop}, no play',
      run: (v: number) => setMedia('g1', 'song.mp3', false, v),
      path: '/api/v1/groups/g1/media',
      body: { file: 'song.mp3', loop: false },
    },
    {
      name: 'stop → POST /stop with no body',
      run: (v: number) => stop('g1', v),
      path: '/api/v1/groups/g1/stop',
      body: undefined,
    },
  ]

  it.each(cases)('$name and sets If-Match', async ({ run, path, body }) => {
    const spy = spyFetch({ version: 51, group: { id: 'g1' } }, 200, '"51"')
    const r = await run(42)
    const c = lastCall(spy)
    expect(c.method).toBe('POST')
    expect(c.path).toBe(path)
    expect(c.headers['If-Match']).toBe('42')
    expect(c.body).toEqual(body)
    expect(r.data.version).toBe(51)
  })

  it('falls back to the ETag/seed version when the body omits it', async () => {
    spyFetch({ group: { id: 'g1' } }, 200, '"77"')
    const r = await stop('g1', 9)
    expect(r.data.version).toBe(77)
  })

  it('a 409 → ApiError.code === "version_conflict"', async () => {
    spyFetch({ error: { code: 'version_conflict', message: 'stale' } }, 409)
    await expect(selectAndPlay('g1', 'a.mp3', true, 1)).rejects.toMatchObject({
      code: 'version_conflict',
      status: 409,
    })
  })

  it('a 412 (missing If-Match) → ApiError.code === "precondition_required"', async () => {
    spyFetch({ error: { code: 'precondition_required', message: 'If-Match required' } }, 412)
    const err = await stop('g1', 1).catch((e) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err.code).toBe('precondition_required')
    expect(err.status).toBe(412)
  })
})
