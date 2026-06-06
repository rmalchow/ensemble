import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import { writable } from 'svelte/store'

// Mock the node API so "Play test signal" can be asserted, keeping the real
// ApiError + CALIBRATE_DEFAULT_SEC.
const calibratePlayMock = vi.fn()
vi.mock('../../../lib/node', async (orig) => {
  const actual = (await orig()) as Record<string, unknown>
  return { ...actual, calibratePlay: (...a: unknown[]) => calibratePlayMock(...a) }
})
// Keep the group-status poll inert in the component.
vi.mock('../../../lib/groupStatus', () => ({
  pollGroupStatus: () => () => {},
  groupStatus: writable(new Map()),
}))

import CalibrationHelper from '../CalibrationHelper.svelte'
import { CALIBRATE_DEFAULT_SEC } from '../../../lib/node'
import type { NodeDetailView } from '../../../lib/node'

function node(p: Partial<NodeDetailView> = {}): NodeDetailView {
  return {
    id: 'n-1',
    name: 'Hall',
    addrs: [],
    hwDelayUs: 0,
    channel: 'stereo',
    gainDb: 0,
    online: true,
    groupId: 'g-1',
    isMaster: false,
    caps: { render: true, sinks: ['alsa'], encode: ['pcm'], decode: ['pcm'], fec: ['none'], maxRate: 48000 },
    ...p,
  }
}

beforeEach(() => calibratePlayMock.mockReset())
afterEach(cleanup)

describe('CalibrationHelper', () => {
  it('Play test signal calls calibratePlay with the node group + default duration', async () => {
    calibratePlayMock.mockResolvedValue({ playedOn: ['n-1'], durationSec: CALIBRATE_DEFAULT_SEC, warnings: [] })
    render(CalibrationHelper, { props: { node: node(), liveSyncErrorUs: 380, disabled: false } })
    await fireEvent.click(screen.getByText(/Play test signal/i))
    expect(calibratePlayMock).toHaveBeenCalledWith({ groupId: 'g-1', durationSec: CALIBRATE_DEFAULT_SEC })
  })

  it('a solo / ungrouped node falls back to nodeIds:[id]', async () => {
    calibratePlayMock.mockResolvedValue({ playedOn: ['n-1'], durationSec: 10, warnings: [] })
    render(CalibrationHelper, { props: { node: node({ groupId: undefined }), liveSyncErrorUs: null, disabled: false } })
    await fireEvent.click(screen.getByText(/Play test signal/i))
    expect(calibratePlayMock).toHaveBeenCalledWith({ nodeIds: ['n-1'], durationSec: CALIBRATE_DEFAULT_SEC })
  })

  it('surfaces warnings returned by the server', async () => {
    calibratePlayMock.mockResolvedValue({ playedOn: [], durationSec: 10, warnings: ['n-1 render=false'] })
    render(CalibrationHelper, { props: { node: node(), liveSyncErrorUs: null, disabled: false } })
    await fireEvent.click(screen.getByText(/Play test signal/i))
    expect(await screen.findByText('n-1 render=false')).toBeTruthy()
  })

  it('sync readout renders syncMs; ✔ when sub-ms', () => {
    render(CalibrationHelper, { props: { node: node(), liveSyncErrorUs: 380, disabled: false } })
    expect(screen.getByText(/✔ \+0\.38 ms/)).toBeTruthy()
  })

  it('master / no-group shows the em-dash readout', () => {
    render(CalibrationHelper, { props: { node: node(), liveSyncErrorUs: null, disabled: false } })
    expect(screen.getByText(/master \/ offline \/ no group/i)).toBeTruthy()
  })

  it('mentions automated measurement is a future enhancement (MVP-manual)', () => {
    render(CalibrationHelper, { props: { node: node(), liveSyncErrorUs: null, disabled: false } })
    expect(screen.getByText(/future enhancement/i)).toBeTruthy()
  })
})
