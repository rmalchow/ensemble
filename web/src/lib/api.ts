// Typed client for the /api/v1 surface (08). The mpvsync api.ts targeted the
// wrong surface (devices/send/upload/transcode) — only its thin-fetch +
// throw-on-non-2xx idiom is reused here. Every mutating call in every screen
// piece MUST route through apiFetch so error-envelope (08 §0.4) and If-Match
// (08 §0.5) handling stay uniform.

import { session } from './stores'
import { navigate } from './router'
import type { GroupStatus, NodeRecord, GroupRecord } from './types'

// ApiError carries the canonical envelope `code` plus the HTTP `status`. The
// `code`/`message` are shown verbatim by ErrorBanner (09 §0 error state).
export class ApiError extends Error {
  code: string
  status: number
  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

// Read is the result of a fetch; `version` is the ETag (ConfigDoc.Version) to
// seed a follow-up If-Match on a mutating call.
export interface Read<T> {
  data: T
  version?: number
}

interface FetchOpts {
  method?: string
  body?: unknown
  ifMatch?: number
  signal?: AbortSignal
}

function parseETag(resp: Response): number | undefined {
  const raw = resp.headers.get('ETag')
  if (raw == null) return undefined
  // ETags may be quoted/weak ("\"42\"" or "W/\"42\""). Extract the integer.
  const m = raw.match(/\d+/)
  if (!m) return undefined
  const n = Number(m[0])
  return Number.isFinite(n) ? n : undefined
}

// apiFetch is the core call. It decodes the §0.4 error envelope on every
// non-2xx and throws ApiError{status,code,message}; on 2xx it returns
// {data, version} with `version` lifted from the ETag header. A 401 clears the
// session and redirects to /login (session expiry, 09 screen 2).
export async function apiFetch<T>(path: string, opts: FetchOpts = {}): Promise<Read<T>> {
  const headers: Record<string, string> = {}
  let body: BodyInit | undefined
  if (opts.body !== undefined) {
    headers['Content-Type'] = 'application/json'
    body = JSON.stringify(opts.body)
  }
  if (opts.ifMatch !== undefined) {
    headers['If-Match'] = String(opts.ifMatch)
  }

  const resp = await fetch(path, {
    method: opts.method ?? 'GET',
    headers,
    body,
    signal: opts.signal,
    credentials: 'same-origin',
  })

  if (resp.status === 401) {
    // Session expired / never authenticated. Clear and bounce to login,
    // preserving the requested route for post-login return.
    session.set(null)
    const back = location.pathname + location.search
    navigate(`/login?next=${encodeURIComponent(back)}`, true)
    const { code, message } = await readEnvelope(resp)
    throw new ApiError(401, code || 'unauthenticated', message || 'unauthenticated')
  }

  if (!resp.ok) {
    const { code, message } = await readEnvelope(resp)
    throw new ApiError(resp.status, code, message)
  }

  const version = parseETag(resp)
  const data = await readJSON<T>(resp)
  return { data, version }
}

async function readEnvelope(
  resp: Response,
): Promise<{ code: string; message: string }> {
  // Fallbacks keep the error meaningful even if the body is empty / not the
  // envelope shape. Default codes map status → a recognisable code so the UI
  // can branch (e.g. version_conflict, proxy_failed) without a body.
  const fallback = statusCode(resp.status)
  try {
    const j = (await resp.json()) as Partial<{
      error: { code?: string; message?: string }
    }>
    const code = j?.error?.code || fallback
    const message = j?.error?.message || resp.statusText || fallback
    return { code, message }
  } catch {
    return { code: fallback, message: resp.statusText || fallback }
  }
}

function statusCode(status: number): string {
  switch (status) {
    case 400:
      return 'bad_request'
    case 401:
      return 'unauthenticated'
    case 403:
      return 'forbidden'
    case 404:
      return 'not_found'
    case 409:
      return 'version_conflict'
    case 412:
      return 'precondition_required'
    case 422:
      return 'unprocessable'
    case 502:
      return 'proxy_failed'
    case 503:
      return 'unavailable'
    default:
      return 'error'
  }
}

async function readJSON<T>(resp: Response): Promise<T> {
  if (resp.status === 204) return undefined as T
  const text = await resp.text()
  if (!text) return undefined as T
  return JSON.parse(text) as T
}

// ---- Boot / first-run probe -----------------------------------------------

// StatusProbe is the normalised first-run probe shape the boot machine consumes
// (09 §1 "First-run detection"). It is derived from /bootstrap/info while the
// node is uninitialized (the only pre-cert/pre-session surface, 08 A.1) and from
// /api/v1/auth/session once the node is a member (08 B.4). `initialized` is the
// union of those two probes; `fingerprint`/`clusterName` come from bootstrap.
export interface StatusProbe {
  initialized: boolean
  nodeId: string
  fingerprint: string
  clusterName?: string
}

interface BootstrapInfo {
  nodeId: string
  name: string
  fingerprint: string
  // "uninitialized" (never adopted) | "foreign" (other cluster) | "member".
  state: 'uninitialized' | 'foreign' | 'member'
  softwareVersion?: string
}

// getStatus resolves the first-run probe (09 §1). 08 risk #1: GET /api/v1/status
// (G.1) is auth-gated runtime telemetry, NOT a first-run probe; the only surface
// reachable before any cert/session exists is the unauthenticated
// GET /bootstrap/info (A.1). So we probe bootstrap first:
//   - 200 state=="member"   → the node is already in a cluster (initialized).
//   - 200 state=="uninitialized"|"foreign" → NOT initialized → Setup Wizard.
//   - 403 forbidden         → bootstrap is closed because the node is a healthy
//                             member (A.1) → initialized.
// Any other error propagates so the boot machine shows the resilient error state
// (never guess — 09 §1).
export async function getStatus(): Promise<Read<StatusProbe>> {
  try {
    const { data } = await apiFetch<BootstrapInfo>('/bootstrap/info')
    return {
      data: {
        initialized: data.state === 'member',
        nodeId: data.nodeId,
        fingerprint: data.fingerprint,
      },
    }
  } catch (e) {
    if (e instanceof ApiError && e.status === 403) {
      // Bootstrap closed → already a member. Identity is filled in post-login
      // from the session/cluster-info reads; the boot machine only needs the
      // initialized flag here.
      return { data: { initialized: true, nodeId: '', fingerprint: '' } }
    }
    throw e
  }
}

// ---- Auth ------------------------------------------------------------------

export interface SessionInfo {
  authenticated: boolean
  method: string
  nodeId: string
  configVersion: number
}

export function getSession(): Promise<Read<SessionInfo>> {
  return apiFetch('/api/v1/auth/session')
}

export interface ClusterInfoBody {
  name: string
  caFingerprint: string
  created: string
}

export interface SetupResult {
  cluster: ClusterInfoBody
  node: { id: string; name: string }
  version: number
}

// setup performs the genesis first-init (08 B.1): creates the cluster CA, sets
// the admin password, and logs the operator in (session cookie set server-side).
// No If-Match — there is no prior version (genesis write).
export async function setup(req: {
  clusterName: string
  adminPassword: string
  nodeName?: string
}): Promise<SetupResult> {
  const { data } = await apiFetch<SetupResult>('/api/v1/setup', {
    method: 'POST',
    body: req,
  })
  return data
}

// login exchanges the admin password for a session cookie (08 B.2). `keep` is
// passed through to request a longer-lived session (P1.3 may honour or ignore
// it; the toggle is otherwise cosmetic — spec risk #2).
export async function login(
  password: string,
  keep = false,
): Promise<{ session: { expiresAt: string } }> {
  const { data } = await apiFetch<{ session: { expiresAt: string } }>(
    '/api/v1/auth/login',
    { method: 'POST', body: { password, keep } },
  )
  return data
}

export async function logout(): Promise<void> {
  await apiFetch('/api/v1/auth/logout', { method: 'POST' })
}

// ---- Settings: admin password (08 B.3a) -----------------------------------

// changePassword mutates ConfigDoc.Auth; If-Match REQUIRED (08 §0.5). Wrong
// `currentPassword` surfaces as 401; a stale version as 409 version_conflict.
export async function changePassword(
  currentPassword: string,
  newPassword: string,
  ifMatch: number,
): Promise<{ version: number }> {
  const { data, version } = await apiFetch<{ version: number }>(
    '/api/v1/auth/password',
    { method: 'POST', body: { currentPassword, newPassword }, ifMatch },
  )
  return { version: data?.version ?? version ?? ifMatch }
}

// ---- Settings: API keys (08 B.5/B.6/B.7) ----------------------------------

export interface ApiKeyMeta {
  id: string
  label: string
  createdAt: string
  lastUsedAt: string | null
}

export interface NewApiKey {
  id: string
  label: string
  secret: string
  createdAt: string
}

export function listKeys(): Promise<Read<{ version: number; keys: ApiKeyMeta[] }>> {
  return apiFetch('/api/v1/auth/keys')
}

// createKey mints a key; the plaintext `secret` is returned EXACTLY ONCE
// (08 B.6). If-Match REQUIRED.
export async function createKey(
  label: string,
  ifMatch: number,
): Promise<{ version: number; key: NewApiKey }> {
  const { data, version } = await apiFetch<{ version: number; key: NewApiKey }>(
    '/api/v1/auth/keys',
    { method: 'POST', body: { label }, ifMatch },
  )
  return { version: data.version ?? version ?? ifMatch, key: data.key }
}

// revokeKey drops a key's hash from ConfigDoc.Auth (08 B.7). If-Match REQUIRED.
export async function revokeKey(
  id: string,
  ifMatch: number,
): Promise<{ version: number }> {
  const { data, version } = await apiFetch<{ version: number }>(
    `/api/v1/auth/keys/${encodeURIComponent(id)}`,
    { method: 'DELETE', ifMatch },
  )
  return { version: data?.version ?? version ?? ifMatch }
}

// ---- Settings: cluster info + leave (08 C.1 / C.6) ------------------------

export function clusterInfoFull(): Promise<
  Read<{
    version: number
    cluster: ClusterInfoBody
    counts: { nodes: number; groups: number }
  }>
> {
  return apiFetch('/api/v1/cluster/info')
}

// leaveCluster is the coordinated self-forget (08 C.6). If-Match REQUIRED for
// the coordinated path; `coordinated:false` signals the unreachable-cluster
// local-wipe fallback (warn the operator).
export async function leaveCluster(
  ifMatch: number,
): Promise<{ version: number; leftNodeId: string; coordinated: boolean }> {
  const { data } = await apiFetch<{
    version: number
    leftNodeId: string
    coordinated: boolean
  }>('/api/v1/cluster/leave', { method: 'POST', ifMatch })
  return data
}

// ---- Read-mostly ConfigDoc projections (08 C.1/D.1/E.1) -------------------

export function getClusterInfo(): Promise<
  Read<{
    cluster: { name: string; caFingerprint: string; created: string }
    counts: { nodes: number; groups: number }
  }>
> {
  return apiFetch('/api/v1/cluster')
}

export function listNodes(): Promise<Read<{ nodes: NodeRecord[] }>> {
  return apiFetch('/api/v1/nodes')
}

export function listGroups(): Promise<Read<{ groups: GroupRecord[] }>> {
  return apiFetch('/api/v1/groups')
}

// ---- Live (08 G.2) --------------------------------------------------------

export async function getGroupStatus(
  id: string,
  signal?: AbortSignal,
): Promise<GroupStatus> {
  const { data } = await apiFetch<GroupStatus>(
    `/api/v1/groups/${encodeURIComponent(id)}/status`,
    { signal },
  )
  return data
}
