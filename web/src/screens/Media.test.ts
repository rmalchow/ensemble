import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'
import Media from './Media.svelte'
import { session, configVersion } from '../lib/stores'
import { __setSnapshotForTest } from '../lib/groups'
import type { GroupRecord, NodeRecord } from '../lib/types'

function resp(opts: { status?: number; body: unknown; etag?: string }): Response {
  const headers = new Headers()
  if (opts.etag) headers.set('ETag', opts.etag)
  const status = opts.status ?? 200
  return {
    status,
    ok: status >= 200 && status < 300,
    statusText: '',
    headers,
    json: async () => opts.body,
    text: async () => JSON.stringify(opts.body),
  } as unknown as Response
}

function node(id: string, p: Partial<NodeRecord> = {}): NodeRecord {
  return {
    id,
    name: id.toUpperCase(),
    addrs: [],
    hwDelayUs: 0,
    channel: 'stereo',
    gainDb: 0,
    caps: { render: true, sinks: ['alsa'], encode: ['pcm'], decode: ['pcm'], fec: ['none'], maxRate: 48000 },
    ...p,
  }
}
function group(id: string, members: string[], media = { file: '', loop: true }, playing = false): GroupRecord {
  return {
    id,
    name: id,
    memberNodeIds: members,
    profile: { codec: 'pcm', fec: 'none', rate: 48000, framesPerChunk: 480, fecK: 8, interleave: 4 },
    media,
    playing,
  }
}

const NODES = [node('n1'), node('n2')]
const GROUPS = [group('g1', ['n1', 'n2'])]

// Routed fetch handling every endpoint the Media screen touches. `mediaFiles`
// and `mediaStatus` are mutable so tests can vary the F.1 result / 502.
let mediaResult: { status?: number; body: unknown } = { body: { nodeId: 'n1', files: [{ file: 'jazz-loop.mp3', durationMs: 192_000 }, { file: 'ocean.mp3', durationMs: 588_000 }] } }
let writeSpy = vi.fn()

function installFetch(groupsArg: GroupRecord[] = GROUPS) {
  const f = vi.fn(async (input: string, init?: RequestInit) => {
    const path = String(input)
    if (path.includes('/api/v1/media')) return resp(mediaResult)
    if (path.endsWith('/status')) return resp({ body: { groupId: 'g1', masterNodeId: 'n1', profile: groupsArg[0].profile, streamGen: 1, playing: groupsArg[0].playing, members: [{ nodeId: 'n1', syncErrorUs: 0, offsetUs: 0, driftRatio: 1, underruns: 0, clockQuality: 'good', online: true }] } })
    if (path.includes('/play') || path.includes('/media') || path.includes('/stop')) {
      writeSpy(path, init)
      return resp({ body: { version: 99, group: groupsArg[0] }, etag: '"99"' })
    }
    if (path.includes('/api/v1/nodes')) return resp({ body: { nodes: NODES }, etag: '"5"' })
    if (path.includes('/api/v1/groups')) return resp({ body: { groups: groupsArg }, etag: '"5"' })
    throw new Error(`unexpected ${path}`)
  })
  vi.stubGlobal('fetch', f)
  return f
}

beforeEach(() => {
  session.set({ authenticated: true, nodeId: 'n1' })
  configVersion.set(5)
  writeSpy = vi.fn()
  mediaResult = { body: { nodeId: 'n1', files: [{ file: 'jazz-loop.mp3', durationMs: 192_000 }, { file: 'ocean.mp3', durationMs: 588_000 }] } }
  __setSnapshotForTest([], [])
  history.replaceState({}, '', '/media?group=g1')
})
afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

describe('Media screen (09 §7)', () => {
  it('lists the master data/ and shows the now-playing group', async () => {
    installFetch()
    render(Media)
    await waitFor(() => expect(screen.getByText('jazz-loop.mp3')).toBeTruthy())
    expect(screen.getByText('3:12')).toBeTruthy()
    expect(screen.getByText('data/ on N1')).toBeTruthy()
  })

  it('Select & play issues F.3 with {file, loop} and If-Match', async () => {
    installFetch()
    render(Media)
    await waitFor(() => expect(screen.getAllByText(/Select & play/).length).toBeGreaterThan(0))
    await fireEvent.click(screen.getAllByText(/Select & play/)[0])
    await waitFor(() => expect(writeSpy).toHaveBeenCalled())
    const [path, init] = writeSpy.mock.calls[0]
    expect(path).toBe('/api/v1/groups/g1/play')
    const playBody = JSON.parse((init as RequestInit).body as string)
    expect(playBody).toMatchObject({ file: 'jazz-loop.mp3', loop: true })
    // Master-follows-source: the browsed library's node rides along as nodeId.
    expect(typeof playBody.nodeId).toBe('string')
    expect(((init as RequestInit).headers as Record<string, string>)['If-Match']).toBe('5')
  })

  it('loop toggle while stopped → F.2 (POST /media), no play', async () => {
    installFetch([group('g1', ['n1', 'n2'], { file: 'jazz-loop.mp3', loop: true }, false)])
    render(Media)
    await waitFor(() => expect(screen.getByText(/loop ON/)).toBeTruthy())
    await fireEvent.click(screen.getByText(/loop ON/))
    await waitFor(() => expect(writeSpy).toHaveBeenCalled())
    const [path, init] = writeSpy.mock.calls[0]
    expect(path).toBe('/api/v1/groups/g1/media')
    const loopBody = JSON.parse((init as RequestInit).body as string)
    expect(loopBody).toMatchObject({ file: 'jazz-loop.mp3', loop: false })
    expect(typeof loopBody.nodeId).toBe('string')
  })

  it('empty data/ → empty state', async () => {
    mediaResult = { body: { nodeId: 'n1', files: [] } }
    installFetch()
    render(Media)
    await waitFor(() => expect(screen.getByText(/No media in/)).toBeTruthy())
  })

  it('offline master (502 on F.1) → offline banner + disabled', async () => {
    mediaResult = { status: 502, body: { error: { code: 'proxy_failed', message: 'unreachable' } } }
    installFetch([group('g1', ['n1', 'n2'], { file: 'jazz-loop.mp3', loop: true }, false)])
    render(Media)
    await waitFor(() => expect(screen.getByText(/scoped master is unreachable/i)).toBeTruthy())
    expect(screen.getByText('offline')).toBeTruthy()
  })
})
