import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, waitFor } from '@testing-library/svelte'
import App from './App.svelte'
import { session, clusterInfo } from './lib/stores'

// Drive the boot probe via a mocked fetch. Routes:
//   GET /bootstrap/info       → { state, nodeId, fingerprint } (first-run probe)
//   GET /api/v1/auth/session  → { authenticated, ... }
// 08 risk #1: the pre-init probe is /bootstrap/info (A.1), not the auth-gated
// /api/v1/status (G.1).
function mockFetch(handlers: Record<string, () => Response | Promise<Response>>) {
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === 'string' ? input : input.toString()
    const path = url.split('?')[0]
    const h = handlers[path]
    if (!h) throw new Error(`unexpected fetch: ${path}`)
    return h()
  })
}

function jsonResp(status: number, body: unknown): Response {
  return {
    status,
    ok: status >= 200 && status < 300,
    statusText: '',
    headers: new Headers(),
    json: async () => body,
    text: async () => JSON.stringify(body),
  } as unknown as Response
}

beforeEach(() => {
  session.set(null)
  clusterInfo.set(null)
  // jsdom provides location/history; default the path to /.
  history.replaceState({}, '', '/')
})

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

describe('App boot', () => {
  it('uninitialized renders the Setup full-page (no NavRail)', async () => {
    history.replaceState({}, '', '/groups')
    vi.stubGlobal(
      'fetch',
      mockFetch({
        '/bootstrap/info': () =>
          jsonResp(200, { state: 'uninitialized', nodeId: 'n-1', fingerprint: 'fp', name: 'x' }),
      }),
    )
    const { container } = render(App)
    await waitFor(() =>
      expect(screen.getByText('Ensemble — set up this player')).toBeTruthy(),
    )
    expect(container.querySelector('nav.rail')).toBeNull()
  })

  it('member + unauthenticated renders Login', async () => {
    vi.stubGlobal(
      'fetch',
      mockFetch({
        '/bootstrap/info': () => jsonResp(403, { error: { code: 'forbidden', message: 'member' } }),
        '/api/v1/auth/session': () =>
          jsonResp(200, { authenticated: false, method: '', nodeId: 'n-1', configVersion: 0 }),
      }),
    )
    render(App)
    await waitFor(() => expect(screen.getByText('Admin password')).toBeTruthy())
  })

  it('member + authenticated renders the shell with NavRail', async () => {
    vi.stubGlobal(
      'fetch',
      mockFetch({
        '/bootstrap/info': () => jsonResp(403, { error: { code: 'forbidden', message: 'member' } }),
        '/api/v1/auth/session': () =>
          jsonResp(200, { authenticated: true, method: 'session', nodeId: 'n-1', configVersion: 7 }),
      }),
    )
    const { container } = render(App)
    await waitFor(() => expect(container.querySelector('nav.rail')).not.toBeNull())
    // The five flat nav items are present (scoped to the rail; the routed
    // placeholder card may also echo a screen name).
    const rail = container.querySelector('nav.rail') as HTMLElement
    const labels = Array.from(rail.querySelectorAll('.label')).map((n) => n.textContent)
    expect(labels).toEqual(['Dashboard', 'Cluster', 'Groups', 'Media', 'Settings'])
  })

  it('status-probe error renders the error state with Retry', async () => {
    vi.stubGlobal(
      'fetch',
      mockFetch({
        '/bootstrap/info': () => jsonResp(503, { error: { code: 'unavailable', message: 'down' } }),
      }),
    )
    render(App)
    await waitFor(() => expect(screen.getByText('Cannot reach node')).toBeTruthy())
    expect(screen.getByText('Retry')).toBeTruthy()
  })
})
