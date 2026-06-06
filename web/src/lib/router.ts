// Minimal history-based SPA router (P0.4 §4). A flat route table with one
// parameterised contextual route (/nodes/:id); a current-route store; navigate()
// for programmatic moves; and an auth/init guard hook installed by App. No
// dependency on Svelte components — screens read `currentRoute` and switch on
// `name`.

import { writable, type Readable } from 'svelte/store'

export interface Route {
  path: string
  name: string
}

export interface CurrentRoute {
  name: string
  params: Record<string, string>
  path: string
}

// The route table. Order matters only for readability; matching is exact except
// for the single :id segment on /nodes/:id. Full-page routes (setup/login) and
// the five flat nav screens (09 §0) plus the contextual node-detail route.
export const routes: Route[] = [
  { path: '/setup', name: 'setup' },
  { path: '/login', name: 'login' },
  { path: '/', name: 'dashboard' },
  { path: '/cluster', name: 'cluster' },
  { path: '/groups', name: 'groups' },
  { path: '/media', name: 'media' },
  { path: '/settings', name: 'settings' },
  { path: '/nodes/:id', name: 'node' },
]

const NOT_FOUND: CurrentRoute = { name: 'notfound', params: {}, path: '' }

// authGuard is the pure auth/init redirect rule (P0.4 §5, test matrix). Returns
// the path to redirect to, or null to allow the requested path. `initialized`
// and `authenticated` may be undefined while the boot probe is still running.
//   - uninitialized                ⇒ only /setup; everything else ⇒ /setup
//   - initialized + unauthenticated ⇒ only /login; everything else ⇒ /login
//   - initialized + authenticated   ⇒ allow, but bounce off /setup and /login
export function authGuard(
  initialized: boolean | undefined,
  authenticated: boolean | undefined,
  path: string,
): string | null {
  const clean = path.split('?')[0].split('#')[0]
  if (initialized === false) {
    return clean === '/setup' ? null : '/setup'
  }
  if (authenticated === false) {
    return clean === '/login' ? null : '/login'
  }
  if (clean === '/setup' || clean === '/login') return '/'
  return null
}

// matchRoute resolves a path string to a CurrentRoute, extracting :id-style
// params. Exported for the guard + tests.
export function matchRoute(path: string): CurrentRoute {
  const clean = path.split('?')[0].split('#')[0] || '/'
  for (const r of routes) {
    const params = matchPattern(r.path, clean)
    if (params) return { name: r.name, params, path: clean }
  }
  return { ...NOT_FOUND, path: clean }
}

function matchPattern(
  pattern: string,
  path: string,
): Record<string, string> | null {
  const ps = pattern.split('/')
  const us = path.split('/')
  if (ps.length !== us.length) return null
  const params: Record<string, string> = {}
  for (let i = 0; i < ps.length; i++) {
    if (ps[i].startsWith(':')) {
      if (us[i] === '') return null
      params[ps[i].slice(1)] = decodeURIComponent(us[i])
    } else if (ps[i] !== us[i]) {
      return null
    }
  }
  return params
}

const store = writable<CurrentRoute>(matchRoute(currentPath()))

// currentRoute is the read-only current-route store screens subscribe to.
export const currentRoute: Readable<CurrentRoute> = store

function currentPath(): string {
  return typeof location !== 'undefined' ? location.pathname : '/'
}

let activeGuard: ((path: string) => string | null) | null = null

// navigate moves to `to`. `replace` swaps the history entry instead of pushing.
// The guard (if installed) may rewrite the destination before it commits.
export function navigate(to: string, replace = false): void {
  let dest = to
  if (activeGuard) {
    const redirect = activeGuard(dest)
    if (redirect && redirect !== dest) {
      dest = redirect
      replace = true
    }
  }
  if (typeof history !== 'undefined') {
    if (replace) history.replaceState({}, '', dest)
    else history.pushState({}, '', dest)
  }
  store.set(matchRoute(dest))
}

// startRouter installs the guard, applies it to the current location, and wires
// popstate (back/forward). Returns a disposer that removes the listener.
export function startRouter(guard: (path: string) => string | null): () => void {
  activeGuard = guard
  const onPop = () => {
    const path = currentPath()
    const redirect = activeGuard ? activeGuard(path) : null
    if (redirect && redirect !== path) {
      navigate(redirect, true)
    } else {
      store.set(matchRoute(path))
    }
  }
  // Apply the guard to the initial location.
  onPop()
  if (typeof window !== 'undefined') {
    window.addEventListener('popstate', onPop)
  }
  return () => {
    if (typeof window !== 'undefined') {
      window.removeEventListener('popstate', onPop)
    }
    activeGuard = null
  }
}
