import { describe, it, expect, beforeEach, vi } from 'vitest'
import { get } from 'svelte/store'
import { authGuard, matchRoute, navigate, currentRoute, startRouter } from './router'

describe('authGuard', () => {
  type Row = [boolean | undefined, boolean | undefined, string, string | null]
  // [initialized, authenticated, path, expectedRedirect]
  const rows: Row[] = [
    // Uninitialized ⇒ everything but /setup redirects to /setup.
    [false, false, '/', '/setup'],
    [false, false, '/cluster', '/setup'],
    [false, false, '/setup', null],
    [false, true, '/groups', '/setup'],
    // Initialized + unauthenticated ⇒ everything but /login redirects to /login.
    [true, false, '/', '/login'],
    [true, false, '/groups', '/login'],
    [true, false, '/login', null],
    // Initialized + authenticated ⇒ allow screens; bounce off setup/login.
    [true, true, '/', null],
    [true, true, '/cluster', null],
    [true, true, '/nodes/n-1', null],
    [true, true, '/setup', '/'],
    [true, true, '/login', '/'],
    // Query string is ignored when matching the full-page routes.
    [true, false, '/login?next=%2Fgroups', null],
    [true, true, '/login?next=%2Fgroups', '/'],
  ]
  it.each(rows)('authGuard(%s, %s, %s) === %s', (init, auth, path, want) => {
    expect(authGuard(init, auth, path)).toBe(want)
  })
})

describe('post-login return route', () => {
  it('preserves the originally-requested route in ?next', () => {
    // The 401 path / login screen encodes the requested route as ?next so the
    // login screen can navigate back. authGuard allows /login regardless.
    const requested = '/groups'
    const loginPath = `/login?next=${encodeURIComponent(requested)}`
    expect(authGuard(true, false, loginPath)).toBeNull()
    const next = new URLSearchParams(loginPath.split('?')[1]).get('next')
    expect(next).toBe(requested)
  })
})

describe('matchRoute', () => {
  it('parses /nodes/:id params', () => {
    const r = matchRoute('/nodes/n-1')
    expect(r.name).toBe('node')
    expect(r.params.id).toBe('n-1')
  })
  it('maps / to dashboard', () => {
    expect(matchRoute('/').name).toBe('dashboard')
  })
  it('unknown path is notfound', () => {
    expect(matchRoute('/nope/deep').name).toBe('notfound')
  })
})

describe('navigate', () => {
  beforeEach(() => {
    vi.stubGlobal('history', { pushState: vi.fn(), replaceState: vi.fn() })
    vi.stubGlobal('location', { pathname: '/', search: '' })
    vi.stubGlobal('window', { addEventListener: vi.fn(), removeEventListener: vi.fn() })
  })

  it('navigate("/nodes/n-1") sets params.id', () => {
    // No guard installed → navigate commits as-is.
    startRouter(() => null)
    navigate('/nodes/n-1')
    expect(get(currentRoute).params.id).toBe('n-1')
    expect((history.pushState as ReturnType<typeof vi.fn>)).toHaveBeenCalled()
  })

  it('replace uses replaceState, not pushState', () => {
    startRouter(() => null)
    const push = history.pushState as ReturnType<typeof vi.fn>
    const replace = history.replaceState as ReturnType<typeof vi.fn>
    push.mockClear()
    replace.mockClear()
    navigate('/cluster', true)
    expect(replace).toHaveBeenCalled()
    expect(push).not.toHaveBeenCalled()
  })
})
