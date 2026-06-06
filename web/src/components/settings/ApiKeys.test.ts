import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'
import ApiKeys from './ApiKeys.svelte'
import { configVersion } from '../../lib/stores'

function jsonResp(status: number, body: unknown, etag?: string): Response {
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

beforeEach(() => configVersion.set(42))
afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

describe('ApiKeys', () => {
  it('empty list renders the empty-state copy', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => jsonResp(200, { version: 42, keys: [] }, '"42"')))
    render(ApiKeys)
    await waitFor(() =>
      expect(screen.getByText('No API keys — create one for programmatic access.')).toBeTruthy(),
    )
  })

  it('renders key metadata only (no secret column)', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        jsonResp(
          200,
          {
            version: 42,
            keys: [{ id: 'k-1', label: 'home-assistant', createdAt: '2026-05-01T00:00:00Z', lastUsedAt: null }],
          },
          '"42"',
        ),
      ),
    )
    render(ApiKeys)
    await waitFor(() => expect(screen.getByText('home-assistant')).toBeTruthy())
    // Last-used null → "—".
    expect(screen.getByText('—')).toBeTruthy()
  })

  it('create shows the secret once and does not re-show it after the list refresh', async () => {
    let listCalls = 0
    const fetchSpy = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = (typeof input === 'string' ? input : input.toString()).split('?')[0]
      if (path === '/api/v1/auth/keys' && (init?.method ?? 'GET') === 'POST') {
        return jsonResp(
          201,
          { version: 43, key: { id: 'k-9', label: 'ci', secret: 'ek_live_ONCE', createdAt: 't' } },
          '"43"',
        )
      }
      // GET list: first empty (version 42), then includes the new key metadata
      // (version 43, no secret).
      listCalls++
      const ver = listCalls === 1 ? 42 : 43
      return jsonResp(
        200,
        {
          version: ver,
          keys:
            listCalls === 1
              ? []
              : [{ id: 'k-9', label: 'ci', createdAt: '2026-01-01T00:00:00Z', lastUsedAt: null }],
        },
        `"${ver}"`,
      )
    })
    vi.stubGlobal('fetch', fetchSpy)

    render(ApiKeys)
    await waitFor(() => expect(screen.getByText(/No API keys/)).toBeTruthy())
    await fireEvent.input(screen.getByLabelText(/New key label/), { target: { value: 'ci' } })
    await fireEvent.click(screen.getByText('+ Create'))

    // The once-shown secret box renders (masked) with the "won't be shown
    // again" warn note. Revealing it surfaces the plaintext exactly once.
    await waitFor(() => expect(screen.getByText(/Copy this secret now/)).toBeTruthy())
    await fireEvent.click(screen.getByText('Show'))
    expect(screen.getByText('ek_live_ONCE')).toBeTruthy()

    // The refreshed list shows metadata (the new key's label) but never the
    // secret as a table value — it lives only in the once-shown box.
    expect(screen.getByText('ci')).toBeTruthy()
    expect(screen.getAllByText('ek_live_ONCE').length).toBe(1)

    // The POST carried If-Match: 42.
    const postCall = fetchSpy.mock.calls.find(
      (c) => (c[1] as RequestInit)?.method === 'POST',
    ) as [string, RequestInit]
    expect((postCall[1].headers as Record<string, string>)['If-Match']).toBe('42')
  })
})
