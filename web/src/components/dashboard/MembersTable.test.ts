import { describe, it, expect, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'
import MembersTable from './MembersTable.svelte'
import type { NodeRecord, GroupRecord, GroupStatus, Capabilities } from '../../lib/types'

function caps(p: Partial<Capabilities> = {}): Capabilities {
  return { render: true, sinks: ['alsa'], encode: ['pcm'], decode: ['pcm', 'opus'], fec: ['none', 'xorParity'], maxRate: 48000, ...p }
}
function node(id: string, p: Partial<NodeRecord> = {}): NodeRecord {
  return { id, name: id, addrs: [], hwDelayUs: 0, channel: 'stereo', gainDb: 0, caps: caps(), ...p }
}
function group(members: string[]): GroupRecord {
  return { id: 'g', name: 'G', memberNodeIds: members, profile: { codec: 'pcm', fec: 'xorParity', rate: 48000, framesPerChunk: 480, fecK: 8, interleave: 4 }, media: { file: '', loop: false }, playing: true }
}

afterEach(cleanup)

function status(master: string, members: GroupStatus['members']): GroupStatus {
  return { groupId: 'g', masterNodeId: master, profile: group([]).profile, streamGen: 1, playing: true, members }
}

describe('MembersTable badges (09 §3)', () => {
  it('offline (dimmed + chip) is visually distinct from sink-less (normal-weight ⊘)', () => {
    const nodes = new Map([
      ['m', node('m')],
      ['off', node('off', { channel: 'left' })],
      ['sl', node('sl', { caps: caps({ render: false }) })],
    ])
    const { container } = render(MembersTable, {
      props: {
        group: group(['m', 'off', 'sl']),
        status: status('m', [
          { nodeId: 'm', syncErrorUs: 0, offsetUs: 0, driftRatio: 1, underruns: 0, clockQuality: 'good', online: true },
          { nodeId: 'off', syncErrorUs: 0, offsetUs: 0, driftRatio: 1, underruns: 0, clockQuality: 'good', online: false },
          { nodeId: 'sl', syncErrorUs: 0, offsetUs: 0, driftRatio: 1, underruns: 0, clockQuality: 'good', online: true },
        ]),
        nodes,
      },
    })
    // Offline row: dimmed via .offline class + an "offline" chip.
    const offRow = screen.getByText('off').closest('tr') as HTMLTableRowElement
    expect(offRow.className).toContain('offline')
    expect(screen.getByText('offline')).toBeTruthy()
    // Sink-less row: "⊘ no sink", normal weight (NOT in a dimmed row).
    const slRow = screen.getByText('sl').closest('tr') as HTMLTableRowElement
    expect(slRow.className).not.toContain('offline')
    expect(screen.getByText('⊘ no sink')).toBeTruthy()
    // Master shows its badge.
    expect(screen.getByText(/master/i)).toBeTruthy()
    expect(container).toBeTruthy()
  })

  it('⚠ shows when syncErrorUs ≥ SYNC_WARN_US (1 ms); ✔ below', () => {
    const nodes = new Map([
      ['m', node('m')],
      ['hot', node('hot', { channel: 'left' })],
      ['cool', node('cool', { channel: 'right' })],
    ])
    render(MembersTable, {
      props: {
        group: group(['m', 'hot', 'cool']),
        status: status('m', [
          { nodeId: 'm', syncErrorUs: 0, offsetUs: 0, driftRatio: 1, underruns: 0, clockQuality: 'good', online: true },
          { nodeId: 'hot', syncErrorUs: 1500, offsetUs: 0, driftRatio: 1, underruns: 0, clockQuality: 'good', online: true },
          { nodeId: 'cool', syncErrorUs: 200, offsetUs: 0, driftRatio: 1, underruns: 0, clockQuality: 'good', online: true },
        ]),
        nodes,
      },
    })
    expect(screen.getByText(/⚠ \+1\.50 ms/)).toBeTruthy()
    expect(screen.getByText(/✔ \+0\.20 ms/)).toBeTruthy()
  })

  it('sink-less master renders "master (no local audio)"', () => {
    const nodes = new Map([['m', node('m', { caps: caps({ render: false }) })], ['l', node('l', { channel: 'left' })]])
    render(MembersTable, {
      props: {
        group: group(['m', 'l']),
        status: status('m', [
          { nodeId: 'm', syncErrorUs: 0, offsetUs: 0, driftRatio: 1, underruns: 0, clockQuality: 'good', online: true },
          { nodeId: 'l', syncErrorUs: 0, offsetUs: 0, driftRatio: 1, underruns: 0, clockQuality: 'good', online: true },
        ]),
        nodes,
      },
    })
    expect(screen.getByText(/master \(no local audio\)/i)).toBeTruthy()
  })
})
