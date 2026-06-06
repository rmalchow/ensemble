import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'
import { writable } from 'svelte/store'
import { ApiError } from '../../../lib/api'

// Mock the node API (getNode / patchNode) so the screen state machine can be
// driven without a network stub. calibratePlay is kept from the real module.
const getNodeMock = vi.fn()
const saveMock = vi.fn()

vi.mock('../../../lib/node', async (orig) => {
  const actual = (await orig()) as Record<string, unknown>
  return { ...actual, getNode: (...a: unknown[]) => getNodeMock(...a) }
})

// Stub the live cluster/group enrichment refreshes (non-fatal background reads).
vi.mock('../../../lib/clusterStore', () => ({
  members: writable([]),
  refreshCluster: () => Promise.resolve(),
}))
vi.mock('../../../lib/groups', () => ({
  groups: writable([]),
  refreshGroups: () => Promise.resolve(),
}))
// Keep group-status polling inert.
vi.mock('../../../lib/groupStatus', () => ({
  pollGroupStatus: () => () => {},
  groupStatus: writable(new Map()),
}))

import NodeDetail from '../../../routes/NodeDetail.svelte'
import { configVersion } from '../../../lib/stores'
import { __resetForTest, save as realSave } from '../../../lib/nodeStore'
import type { NodeDetailView } from '../../../lib/node'

function node(p: Partial<NodeDetailView> = {}): NodeDetailView {
  return {
    id: 'n-1',
    name: 'Hall',
    addrs: ['10.0.0.1'],
    hwDelayUs: 0,
    channel: 'stereo',
    gainDb: 0,
    online: true,
    groupId: 'g-1',
    isMaster: false,
    fingerprint: 'sha256:aa11',
    certSignedByCa: true,
    caps: { render: true, sinks: ['alsa'], encode: ['pcm', 'opus'], decode: ['pcm', 'opus'], fec: ['none', 'xorParity'], maxRate: 48000 },
    ...p,
  }
}

beforeEach(() => {
  __resetForTest()
  getNodeMock.mockReset()
  configVersion.set(42)
})
afterEach(cleanup)

describe('NodeDetail state machine (09 §6)', () => {
  it('variant A: caps.render=true → channel/gain/HwDelay + calibration helper present', async () => {
    getNodeMock.mockResolvedValue({ version: 42, node: node() })
    render(NodeDetail, { props: { id: 'n-1' } })
    await waitFor(() => expect(screen.getByText('Audio output')).toBeTruthy())
    expect(screen.getByLabelText('Channel role')).toBeTruthy()
    expect(screen.getByLabelText('Gain in decibels')).toBeTruthy()
    expect(screen.getByLabelText('Hardware delay in microseconds')).toBeTruthy()
    expect(screen.getByText('Calibration helper')).toBeTruthy()
  })

  it('variant B: caps.render=false → audio controls hidden; control-media-only + caps panel shown', async () => {
    getNodeMock.mockResolvedValue({ version: 42, node: node({ caps: { ...node().caps, render: false } }) })
    render(NodeDetail, { props: { id: 'n-1' } })
    await waitFor(() =>
      expect(screen.getByText(/Control \/ media only — no audio output/i)).toBeTruthy(),
    )
    expect(screen.queryByLabelText('Channel role')).toBeNull()
    expect(screen.queryByText('Calibration helper')).toBeNull()
    // Capabilities panel is always shown (Card title + a deep-link reference).
    expect(screen.getAllByText('Capabilities & audio backends').length).toBeGreaterThan(0)
  })

  it('offline: read-only banner; rename allowed + "applies when node returns"', async () => {
    getNodeMock.mockResolvedValue({ version: 42, node: node({ online: false }) })
    render(NodeDetail, { props: { id: 'n-1' } })
    await waitFor(() => expect(screen.getByText(/this node is offline/i)).toBeTruthy())
    expect(screen.getByText(/applies when node returns/i)).toBeTruthy()
    // The audio HW-delay control is disabled while offline.
    const hw = screen.getByLabelText('Hardware delay in microseconds') as HTMLInputElement
    expect(hw.disabled).toBe(true)
    // Rename input remains enabled.
    const name = screen.getByPlaceholderText('n-1') as HTMLInputElement
    expect(name.disabled).toBe(false)
  })

  it('not_found: 404 → message + back-to-Cluster affordance', async () => {
    getNodeMock.mockRejectedValue(new ApiError(404, 'not_found', 'unknown node'))
    render(NodeDetail, { props: { id: 'gone' } })
    await waitFor(() => expect(screen.getByText('unknown node')).toBeTruthy())
    expect(screen.getByText(/back to Cluster/i)).toBeTruthy()
  })

  it('409 on save → Reload & reapply (never silent overwrite)', async () => {
    // Drive the store directly: load, edit, then make save reject 409 via patchNode.
    getNodeMock.mockResolvedValue({ version: 42, node: node() })
    render(NodeDetail, { props: { id: 'n-1' } })
    await waitFor(() => expect(screen.getByText('Identity')).toBeTruthy())
    // Edit the name to dirty the form.
    const name = screen.getByPlaceholderText('n-1') as HTMLInputElement
    await fireEvent.input(name, { target: { value: 'Hallway' } })
    // The real save() will call patchNode → fetch; stub fetch to 409.
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        ({
          status: 409,
          ok: false,
          statusText: '',
          headers: new Headers(),
          json: async () => ({ error: { code: 'version_conflict', message: 'stale' } }),
          text: async () => JSON.stringify({ error: { code: 'version_conflict', message: 'stale' } }),
        }) as unknown as Response,
      ),
    )
    await fireEvent.click(screen.getByText('Save changes'))
    await waitFor(() => expect(screen.getByText(/Reload & reapply/i)).toBeTruthy())
    expect(realSave).toBeTypeOf('function')
    vi.unstubAllGlobals()
  })

  it('loading: shows a skeleton before the record lands', async () => {
    let resolve!: (v: { version: number; node: NodeDetailView }) => void
    getNodeMock.mockReturnValue(new Promise((r) => (resolve = r)))
    const { container } = render(NodeDetail, { props: { id: 'n-1' } })
    expect(container.querySelector('.skeleton')).toBeTruthy()
    resolve({ version: 42, node: node() })
    await waitFor(() => expect(screen.getByText('Identity')).toBeTruthy())
  })
})
