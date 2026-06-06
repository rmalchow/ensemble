import { describe, it, expect, vi, beforeEach } from 'vitest'
import { getStatus, changePassword, createKey, revokeKey } from './api'

// Probe + settings If-Match behavior (08 risk #1 resolution; 08 §0.5).

function resp(opts: { status: number; body?: unknown; etag?: string }): Response {
  const headers = new Headers()
  if (opts.etag) headers.set('ETag', opts.etag)
  const ok = opts.status >= 200 && opts.status < 300
  return {
    status: opts.status,
    ok,
    statusText: '',
    headers,
    json: async () => {
      if (opts.body === undefined) throw new Error('no body')
      return opts.body
    },
    text: async () => (opts.body === undefined ? '' : JSON.stringify(opts.body)),
  } as unknown as Response
}

beforeEach(() => vi.restoreAllMocks())

describe('getStatus first-run probe (/bootstrap/info)', () => {
  it('state "uninitialized" → initialized=false', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        resp({ status: 200, body: { state: 'uninitialized', nodeId: 'n-1', fingerprint: 'fp' } }),
      ),
    )
    const { data } = await getStatus()
    expect(data.initialized).toBe(false)
    expect(data.nodeId).toBe('n-1')
    expect(data.fingerprint).toBe('fp')
  })

  it('state "foreign" → initialized=false (needs takeover)', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => resp({ status: 200, body: { state: 'foreign', nodeId: 'n-2', fingerprint: 'g' } })),
    )
    expect((await getStatus()).data.initialized).toBe(false)
  })

  it('state "member" → initialized=true', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => resp({ status: 200, body: { state: 'member', nodeId: 'n-3', fingerprint: 'h' } })),
    )
    expect((await getStatus()).data.initialized).toBe(true)
  })

  it('403 (bootstrap closed) → initialized=true', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => resp({ status: 403, body: { error: { code: 'forbidden', message: 'm' } } })),
    )
    expect((await getStatus()).data.initialized).toBe(true)
  })

  it('other error propagates (never guesses)', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => resp({ status: 503, body: { error: { code: 'unavailable', message: 'd' } } })),
    )
    await expect(getStatus()).rejects.toMatchObject({ status: 503 })
  })
})

describe('settings writes attach If-Match', () => {
  function captureFetch(status: number, body: unknown, etag?: string) {
    const spy = vi.fn(
      async (_input: RequestInfo | URL, _init?: RequestInit) => resp({ status, body, etag }),
    )
    vi.stubGlobal('fetch', spy)
    return spy
  }

  it('changePassword sends If-Match: configVersion and the POST body', async () => {
    const spy = captureFetch(200, { version: 48 }, '"48"')
    const r = await changePassword('old', 'new-strong-passphrase', 47)
    const init = spy.mock.calls[0][1] as RequestInit
    expect((init.headers as Record<string, string>)['If-Match']).toBe('47')
    expect(init.method).toBe('POST')
    expect(JSON.parse(init.body as string)).toEqual({
      currentPassword: 'old',
      newPassword: 'new-strong-passphrase',
    })
    expect(r.version).toBe(48)
  })

  it('createKey sends If-Match and returns the once-shown secret', async () => {
    const spy = captureFetch(
      201,
      { version: 43, key: { id: 'k-2', label: 'ha', secret: 'ek_live_X', createdAt: 't' } },
      '"43"',
    )
    const r = await createKey('ha', 42)
    expect((spy.mock.calls[0][1] as RequestInit).headers as Record<string, string>).toMatchObject({
      'If-Match': '42',
    })
    expect(r.key.secret).toBe('ek_live_X')
    expect(r.version).toBe(43)
  })

  it('revokeKey sends DELETE with If-Match', async () => {
    const spy = captureFetch(200, { version: 44 }, '"44"')
    const r = await revokeKey('k-2', 43)
    const init = spy.mock.calls[0][1] as RequestInit
    expect(init.method).toBe('DELETE')
    expect((init.headers as Record<string, string>)['If-Match']).toBe('43')
    expect(r.version).toBe(44)
  })

  it('a 409 on a settings write surfaces version_conflict', async () => {
    captureFetch(409, { error: { code: 'version_conflict', message: 'stale' } })
    await expect(changePassword('a', 'b', 1)).rejects.toMatchObject({ code: 'version_conflict' })
  })
})
