// App-level stores. Rewritten for Ensemble (the mpvsync derived-store pattern is
// the model; devices/playlist/CLUSTER_ID are dropped). Components read these
// rather than threading boot state through props (P0.4 §4).

import { derived, writable, type Readable, type Writable } from 'svelte/store'

// session is the authenticated identity, or null when unauthenticated. App seeds
// it from GET /api/v1/auth/session; apiFetch clears it on any 401 (session
// expiry mid-flight → redirect to /login).
export const session: Writable<{ authenticated: boolean; nodeId: string } | null> =
  writable(null)

// configVersion is the last-seen ConfigDoc ETag, used to seed If-Match on the
// next config-mutating call (08 §0.5 optimistic concurrency).
export const configVersion: Writable<number | undefined> = writable(undefined)

// clusterInfo is a light projection of the cluster identity for the header.
export const clusterInfo: Writable<{ name: string; caFingerprint: string } | null> =
  writable(null)

// liveConnected reflects whether the live status feed (polling / WS) is healthy.
// `connected` derives a single boolean for the header health chip.
export const liveConnected: Writable<boolean> = writable(false)

export const connected: Readable<boolean> = derived(
  liveConnected,
  ($c) => $c,
)
