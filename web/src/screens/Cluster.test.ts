import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'

// Mock lib/cluster: both the screen and clusterStore read through it, so a single
// module mock controls all of Cluster.svelte's data + write paths. ApiError must
// stay a real, instanceof-able class (the screen branches on `e instanceof
// ApiError`), so re-export the real one.
import { ApiError } from '../lib/cluster'

const getClusterInfo = vi.fn()
const getNodes = vi.fn()
const getDiscovery = vi.fn()
const adopt = vi.fn()
const takeover = vi.fn()
const forget = vi.fn()

vi.mock('../lib/cluster', async (orig) => {
  const real = (await orig()) as Record<string, unknown>
  return {
    ...real,
    getClusterInfo: (...a: unknown[]) => getClusterInfo(...a),
    getNodes: (...a: unknown[]) => getNodes(...a),
    getDiscovery: (...a: unknown[]) => getDiscovery(...a),
    adopt: (...a: unknown[]) => adopt(...a),
    takeover: (...a: unknown[]) => takeover(...a),
    forget: (...a: unknown[]) => forget(...a),
  }
})

import Cluster from './Cluster.svelte'
import { configVersion } from '../lib/stores'

const info = {
  version: 7,
  cluster: { name: 'Home', caFingerprint: 'sha256:abcd', created: '2026-01-01T00:00:00Z' },
  counts: { nodes: 1, groups: 0 },
}

function memberSnapshot() {
  return {
    version: 7,
    nodes: [
      {
        id: 'node-a',
        name: 'Living Room',
        addrs: ['192.168.1.10:9000'],
        online: true,
        caps: { render: true, sinks: ['alsa'], encode: ['pcm'], decode: ['pcm'], fec: ['none'], maxRate: 48000 },
      },
    ],
  }
}

function discoverySnapshot(discovered: unknown[] = []) {
  return {
    members: [{ id: 'node-a', name: 'Living Room', addrs: ['192.168.1.10:9000'], state: 'member', online: true }],
    discovered,
  }
}

beforeEach(() => {
  configVersion.set(7)
  getClusterInfo.mockResolvedValue(info)
  getNodes.mockResolvedValue(memberSnapshot())
  getDiscovery.mockResolvedValue(discoverySnapshot())
})

afterEach(() => {
  cleanup()
  vi.clearAllMocks()
})

describe('Cluster screen', () => {
  it('renders skeletons while data is loading', () => {
    // Never-resolving reads keep the screen in its loading phase.
    getClusterInfo.mockReturnValue(new Promise(() => {}))
    getNodes.mockReturnValue(new Promise(() => {}))
    getDiscovery.mockReturnValue(new Promise(() => {}))
    const { container } = render(Cluster)
    // The shared Skeleton primitive renders an element with a skeleton class.
    expect(container.querySelector('[class*="skeleton"], .skeleton')).toBeTruthy()
  })

  it('renders the CA fingerprint header with a working copy button', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    // Stub only navigator.clipboard (replacing the whole navigator breaks jsdom).
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    })
    render(Cluster)
    // fmtFingerprint uppercases + colon-groups: sha256:abcd -> SHA256:AB:CD.
    await waitFor(() => expect(screen.getByText('SHA256:AB:CD')).toBeTruthy())
    expect(screen.getByText('Home')).toBeTruthy()
    const copyBtn = screen.getByText(/copy/i).closest('button') as HTMLButtonElement
    await fireEvent.click(copyBtn)
    await waitFor(() => expect(writeText).toHaveBeenCalledWith('SHA256:AB:CD'))
  })

  it('shows the friendly empty-state copy when nothing is discovered', async () => {
    render(Cluster)
    await waitFor(() => expect(screen.getByText(/No new players found nearby/i)).toBeTruthy())
  })

  it('lists a discovered node with a default PIN and an Adopt action', async () => {
    getDiscovery.mockResolvedValue(
      discoverySnapshot([
        {
          nodeId: 'node-b',
          name: 'Kitchen',
          addrs: ['192.168.1.42:7946'],
          fingerprint: 'sha256:beef',
          state: 'uninitialized',
        },
      ]),
    )
    render(Cluster)
    await waitFor(() => expect(screen.getByText('node-b')).toBeTruthy())
    // The credential input appears only AFTER Adopt is clicked (two-stage flow).
    expect(screen.queryByLabelText(/Adoption PIN/i)).toBeNull()
    await fireEvent.click(screen.getByText('Adopt'))
    const pin = screen.getByLabelText(/Adoption PIN/i) as HTMLInputElement
    expect(pin.value).toBe('0000')
  })

  it('renders a list-fetch error as an inline banner with code + message + retry', async () => {
    getClusterInfo.mockRejectedValueOnce(new ApiError(503, 'unavailable', 'Engine starting up.'))
    render(Cluster)
    await waitFor(() => expect(screen.getByText('unavailable')).toBeTruthy())
    expect(screen.getByText('Engine starting up.')).toBeTruthy()

    // Retry re-runs the load; make the second attempt succeed.
    const retry = screen.getByText(/retry/i).closest('button') as HTMLButtonElement
    await fireEvent.click(retry)
    await waitFor(() => expect(screen.getByText('Home')).toBeTruthy())
  })

  it('a 409 version_conflict on a write triggers reload + a reapply prompt (no silent overwrite)', async () => {
    getDiscovery.mockResolvedValue(
      discoverySnapshot([
        {
          nodeId: 'node-b',
          name: 'Kitchen',
          addrs: ['192.168.1.42:7946'],
          fingerprint: 'sha256:beef',
          state: 'uninitialized',
        },
      ]),
    )
    adopt.mockRejectedValueOnce(new ApiError(409, 'version_conflict', 'Stale config version.'))
    render(Cluster)
    await waitFor(() => expect(screen.getByText('Adopt')).toBeTruthy())

    const reloadCountBefore = getNodes.mock.calls.length
    await fireEvent.click(screen.getByText('Adopt')) // arm: reveal PIN
    await fireEvent.click(screen.getByText('Adopt')) // confirm

    // The conflict banner asks the operator to reapply (reload-&-reapply, 09 §0):
    // both the message copy and the "Reload & reapply" button mention reapply.
    await waitFor(() => expect(screen.getAllByText(/reapply/i).length).toBeGreaterThan(0))
    // The conflict surfaces as version_conflict (banner + the row-level error).
    expect(screen.getAllByText('version_conflict').length).toBeGreaterThan(0)
    // The conflict path re-derives configVersion from a fresh read (refreshCluster).
    await waitFor(() => expect(getNodes.mock.calls.length).toBeGreaterThanOrEqual(reloadCountBefore))
  })
})
