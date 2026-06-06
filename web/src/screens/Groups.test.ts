import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'

import { ApiError } from '../lib/groupActions'

// Mock the action layer (createGroup/patchGroup/playGroup/...) so the screen's
// write paths are observable; ApiError stays a real instanceof-able class.
const createGroup = vi.fn()
const patchGroup = vi.fn()
const moveNode = vi.fn()
const playGroup = vi.fn()
const stopGroup = vi.fn()
vi.mock('../lib/groupActions', async (orig) => {
  const real = (await orig()) as Record<string, unknown>
  return {
    ...real,
    createGroup: (...a: unknown[]) => createGroup(...a),
    patchGroup: (...a: unknown[]) => patchGroup(...a),
    moveNode: (...a: unknown[]) => moveNode(...a),
    playGroup: (...a: unknown[]) => playGroup(...a),
    stopGroup: (...a: unknown[]) => stopGroup(...a),
  }
})

// Stub the live status poller so no real fetch fires; feed an empty map.
const { statusStore } = await vi.hoisted(async () => {
  const { writable: w } = await import('svelte/store')
  return { statusStore: w<Map<string, unknown>>(new Map()) }
})
vi.mock('../lib/groupStatus', () => ({
  pollGroupStatus: () => () => {},
  groupStatus: statusStore,
}))

// refreshGroups is the loader; we control it + seed the derived stores directly.
const refreshGroups = vi.fn()
vi.mock('../lib/groups', async (orig) => {
  const real = (await orig()) as Record<string, unknown>
  return { ...real, refreshGroups: (...a: unknown[]) => refreshGroups(...a) }
})

import Groups from './Groups.svelte'
import { __setSnapshotForTest } from '../lib/groups'
import { configVersion } from '../lib/stores'
import type { NodeRecord, GroupRecord } from '../lib/types'

function node(id: string, p: Partial<NodeRecord> = {}): NodeRecord {
  return {
    id,
    name: id,
    addrs: [],
    hwDelayUs: 0,
    channel: 'stereo',
    gainDb: 0,
    caps: { render: true, sinks: ['alsa'], encode: ['pcm'], decode: ['pcm', 'opus'], fec: ['none', 'xorParity'], maxRate: 48000 },
    ...p,
  }
}
function group(id: string, members: string[], p: Partial<GroupRecord> = {}): GroupRecord {
  return {
    id,
    name: id,
    memberNodeIds: members,
    profile: { codec: 'pcm', fec: 'xorParity', rate: 48000, framesPerChunk: 480, fecK: 8, interleave: 4 },
    media: { file: '', loop: false },
    playing: false,
    ...p,
  }
}

beforeEach(() => {
  configVersion.set(7)
  __setSnapshotForTest(
    [group('g-kitchen', ['n1', 'n2']), group('g-bath', ['n3'])],
    [node('n1'), node('n2', { channel: 'left' }), node('n3')],
  )
  refreshGroups.mockResolvedValue(undefined)
})

afterEach(() => {
  cleanup()
  vi.clearAllMocks()
})

describe('Groups screen (09 §5)', () => {
  it('lists groups with member counts and a New-group CTA', async () => {
    render(Groups)
    // Scope to the Groups nav rail so member-transfer pool entries (which also
    // name groups) do not create ambiguous matches.
    await waitFor(() => expect(screen.getByText('+ New group')).toBeTruthy())
    const rail = screen.getByRole('navigation', { name: 'Groups' })
    expect(rail).toBeTruthy()
    expect(rail.textContent).toContain('g-kitchen')
    expect(rail.textContent).toContain('g-bath')
  })

  it('create: modal → POST createGroup with If-Match; empty name blocked client-side', async () => {
    createGroup.mockResolvedValue({ version: 8 })
    render(Groups)
    await waitFor(() => expect(screen.getByText('+ New group')).toBeTruthy())
    await fireEvent.click(screen.getByText('+ New group'))

    // Scope to the modal dialog (the editor also has a "Group name" field).
    const dialog = await screen.findByRole('dialog')
    const input = dialog.querySelector('#new-group-name') as HTMLInputElement
    const createBtn = screen.getByText('Create').closest('button') as HTMLButtonElement
    // Empty name: the Create button is disabled (blocked client-side).
    expect(createBtn.disabled).toBe(true)

    await fireEvent.input(input, { target: { value: 'Patio' } })
    expect((screen.getByText('Create').closest('button') as HTMLButtonElement).disabled).toBe(false)
    await fireEvent.click(screen.getByText('Create'))
    // createGroup is called with the trimmed name + the If-Match version (7).
    await waitFor(() => expect(createGroup).toHaveBeenCalledWith('Patio', 7))
  })

  it('move member: emits a transactional two-group moveNode PATCH', async () => {
    moveNode.mockResolvedValue(9)
    render(Groups)
    // Select g-kitchen (default first). Its editor shows the member transfer.
    await waitFor(() => expect(screen.getByText('In this group')).toBeTruthy())
    // n3 is in g-bath → it appears in the "Available" pool of g-kitchen.
    const n3 = await screen.findByText('n3')
    const checkbox = n3.closest('label')?.querySelector('input') as HTMLInputElement
    await fireEvent.click(checkbox)
    await fireEvent.click(screen.getByText('← Move in'))
    // moveNode is the transactional two-group write (n3: g-bath → g-kitchen).
    await waitFor(() =>
      expect(moveNode).toHaveBeenCalledWith('n3', 'g-bath', 'g-kitchen', ['n3'], ['n1', 'n2'], 7),
    )
  })

  it('profile override: flip codec override → PATCH profile {codec}', async () => {
    patchGroup.mockResolvedValue({ version: 9 })
    render(Groups)
    await waitFor(() => expect(screen.getByText('Profile')).toBeTruthy())
    // Toggle the codec override checkbox (first "Override" in the profile block).
    const overrides = screen.getAllByText('Override')
    const codecToggle = overrides[0].closest('label')?.querySelector('input') as HTMLInputElement
    await fireEvent.click(codecToggle)
    // A select appears; choose opus.
    const select = screen.getAllByRole('combobox')[0] as HTMLSelectElement
    await fireEvent.change(select, { target: { value: 'opus' } })
    await waitFor(() =>
      expect(patchGroup).toHaveBeenCalledWith('g-kitchen', { profile: { codec: 'opus' } }, 7),
    )
  })

  it('409 version_conflict on a write → reload-&-reapply prompt (no silent overwrite)', async () => {
    patchGroup.mockRejectedValueOnce(new ApiError(409, 'version_conflict', 'Stale config version.'))
    render(Groups)
    await waitFor(() => expect(screen.getByText('Profile')).toBeTruthy())
    const overrides = screen.getAllByText('Override')
    const fecToggle = overrides[1].closest('label')?.querySelector('input') as HTMLInputElement
    await fireEvent.click(fecToggle)
    const select = screen.getAllByRole('combobox')[0] as HTMLSelectElement
    await fireEvent.change(select, { target: { value: 'duplicate' } })
    await waitFor(() => expect(screen.getAllByText(/reapply/i).length).toBeGreaterThan(0))
  })
})
