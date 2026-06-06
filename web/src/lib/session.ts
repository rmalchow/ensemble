// Session store + auth-guard helpers (P1.4 §4.3). This is the canonical
// session/configVersion surface the auth screens import. The underlying writable
// stores live in stores.ts (seeded by App's boot probe; cleared by apiFetch on
// any 401); session.ts adds the typed SessionState and the refreshSession()
// helper that re-reads GET /api/v1/auth/session (08 B.4) and re-seeds configVersion
// for If-Match (08 §0.5).

import { get } from 'svelte/store'
import { session, configVersion } from './stores'
import { getSession, ApiError } from './api'

export { session, configVersion }

export type AuthMethod = 'session' | 'apiKey' | 'node'

export interface SessionState {
  authenticated: boolean
  method: AuthMethod
  nodeId: string
}

// refreshSession re-reads B.4, updates the session + configVersion stores, and
// returns the resolved state. On 401 it nulls the session and returns an
// unauthenticated state (the central apiFetch 401 handler also redirects to
// /login). Any other error propagates so callers can show the error state.
export async function refreshSession(): Promise<SessionState> {
  try {
    const { data } = await getSession()
    if (data.authenticated) {
      session.set({ authenticated: true, nodeId: data.nodeId })
      configVersion.set(data.configVersion)
    } else {
      session.set(null)
    }
    return {
      authenticated: data.authenticated,
      method: (data.method as AuthMethod) || 'session',
      nodeId: data.nodeId,
    }
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) {
      session.set(null)
      return { authenticated: false, method: 'session', nodeId: '' }
    }
    throw e
  }
}

// currentConfigVersion reads the last-seen ConfigDoc version to seed If-Match on
// a mutating call. Returns undefined if no read has populated it yet.
export function currentConfigVersion(): number | undefined {
  return get(configVersion)
}
