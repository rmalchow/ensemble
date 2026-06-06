import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'
import Login from './Login.svelte'
import { session } from '../lib/stores'

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

// Route a mocked fetch by path so login + session refresh can be staged.
function mockFetch(handlers: Record<string, () => Response>) {
  return vi.fn(async (input: RequestInfo | URL) => {
    const path = (typeof input === 'string' ? input : input.toString()).split('?')[0]
    const h = handlers[path]
    if (!h) throw new Error(`unexpected fetch ${path}`)
    return h()
  })
}

beforeEach(() => {
  session.set(null)
  history.replaceState({}, '', '/login')
})
afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

describe('Login', () => {
  it('submit calls POST /api/v1/auth/login and navigates to intended route', async () => {
    history.replaceState({}, '', '/login?next=%2Fgroups')
    const fetchSpy = mockFetch({
      '/api/v1/auth/login': () => jsonResp(200, { session: { expiresAt: 't' } }),
      '/api/v1/auth/session': () =>
        jsonResp(200, { authenticated: true, method: 'session', nodeId: 'n-1', configVersion: 5 }),
    })
    vi.stubGlobal('fetch', fetchSpy)
    const pushSpy = vi.spyOn(history, 'pushState')

    render(Login, {})
    await fireEvent.input(screen.getByLabelText(/Admin password/), { target: { value: 'pw' } })
    await fireEvent.click(screen.getByText('Sign in'))

    await waitFor(() =>
      expect(fetchSpy).toHaveBeenCalledWith('/api/v1/auth/login', expect.anything()),
    )
    await waitFor(() => expect(pushSpy).toHaveBeenCalledWith(expect.anything(), '', '/groups'))
  })

  it('success fires onAuthenticated BEFORE navigating (guard flag flip)', async () => {
    const fetchSpy = mockFetch({
      '/api/v1/auth/login': () => jsonResp(200, { session: { expiresAt: 't' } }),
      '/api/v1/auth/session': () =>
        jsonResp(200, { authenticated: true, method: 'session', nodeId: 'n-1', configVersion: 5 }),
    })
    vi.stubGlobal('fetch', fetchSpy)
    const order: string[] = []
    const onAuthenticated = vi.fn(() => order.push('auth'))
    const pushSpy = vi
      .spyOn(history, 'pushState')
      .mockImplementation(() => order.push('navigate'))

    render(Login, { onAuthenticated })
    await fireEvent.input(screen.getByLabelText(/Admin password/), { target: { value: 'pw' } })
    await fireEvent.click(screen.getByText('Sign in'))

    await waitFor(() => expect(pushSpy).toHaveBeenCalled())
    // The guard flag must flip before the navigate runs, or the stale guard
    // bounces the redirect straight back to /login.
    expect(order).toEqual(['auth', 'navigate'])
  })

  it('401 shows generic "Wrong password." (no enumeration)', async () => {
    vi.stubGlobal(
      'fetch',
      mockFetch({
        '/api/v1/auth/login': () =>
          jsonResp(401, { error: { code: 'unauthenticated', message: 'whatever' } }),
        '/api/v1/auth/session': () => jsonResp(401, {}),
      }),
    )
    render(Login, {})
    await fireEvent.input(screen.getByLabelText(/Admin password/), { target: { value: 'bad' } })
    await fireEvent.click(screen.getByText('Sign in'))
    await waitFor(() => expect(screen.getByText('Wrong password.')).toBeTruthy())
  })

  it('rate_limited envelope shows the throttle message', async () => {
    vi.stubGlobal(
      'fetch',
      mockFetch({
        '/api/v1/auth/login': () =>
          jsonResp(429, { error: { code: 'rate_limited', message: 'slow down' } }),
        '/api/v1/auth/session': () => jsonResp(401, {}),
      }),
    )
    render(Login, {})
    await fireEvent.input(screen.getByLabelText(/Admin password/), { target: { value: 'x' } })
    await fireEvent.click(screen.getByText('Sign in'))
    await waitFor(() => expect(screen.getByText(/Too many attempts/)).toBeTruthy())
  })

  it('503 not_ready (uninitialized) redirects to /setup', async () => {
    vi.stubGlobal(
      'fetch',
      mockFetch({
        '/api/v1/auth/login': () =>
          jsonResp(503, { error: { code: 'not_ready', message: 'uninit' } }),
      }),
    )
    const pushSpy = vi.spyOn(history, 'pushState')
    render(Login, {})
    await fireEvent.input(screen.getByLabelText(/Admin password/), { target: { value: 'x' } })
    await fireEvent.click(screen.getByText('Sign in'))
    await waitFor(() => expect(pushSpy).toHaveBeenCalledWith(expect.anything(), '', '/setup'))
  })
})
