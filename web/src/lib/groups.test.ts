import { describe, it, expect, beforeEach } from 'vitest'
import { get } from 'svelte/store'
import {
  __setSnapshotForTest,
  unassignedNodeIds,
  memberRows,
  leastCapableCodec,
  type MemberRow,
} from './groups'
import type {
  NodeRecord,
  GroupRecord,
  GroupStatus,
  Capabilities,
} from './types'

function caps(p: Partial<Capabilities> = {}): Capabilities {
  return {
    render: true,
    sinks: ['alsa'],
    encode: ['pcm', 'opus'],
    decode: ['pcm', 'opus'],
    fec: ['none', 'xorParity', 'duplicate'],
    maxRate: 48000,
    ...p,
  }
}

function node(id: string, p: Partial<NodeRecord> = {}): NodeRecord {
  return {
    id,
    name: id.toUpperCase(),
    addrs: [],
    hwDelayUs: 0,
    channel: 'stereo',
    gainDb: 0,
    caps: caps(),
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

describe('unassignedNodeIds', () => {
  beforeEach(() => __setSnapshotForTest([], []))

  it('nodes in explicit groups are not unassigned; orphans are', () => {
    __setSnapshotForTest(
      [group('g1', ['n1', 'n2'])],
      [node('n1'), node('n2'), node('n3')],
    )
    expect(get(unassignedNodeIds)).toEqual(['n3'])
  })

  it('a node in exactly one group never double-counts (glossary invariant)', () => {
    __setSnapshotForTest(
      [group('g1', ['n1']), group('g2', ['n2'])],
      [node('n1'), node('n2')],
    )
    expect(get(unassignedNodeIds)).toEqual([])
  })

  it('all nodes assigned → empty bucket', () => {
    __setSnapshotForTest([group('g1', ['n1', 'n2'])], [node('n1'), node('n2')])
    expect(get(unassignedNodeIds)).toEqual([])
  })
})

describe('memberRows join (09 §3)', () => {
  const nodes = new Map<string, NodeRecord>([
    ['m', node('m')], // master, renders
    ['l', node('l', { channel: 'left' })], // listener
    ['sl', node('sl', { caps: caps({ render: false }) })], // sink-less listener
    ['off', node('off', { channel: 'right' })], // offline listener
  ])

  function statusFor(masterId: string, online: Record<string, boolean>, sync: Record<string, number> = {}): GroupStatus {
    return {
      groupId: 'g',
      masterNodeId: masterId,
      profile: nodes.get('m')!.caps && { codec: 'pcm', fec: 'xorParity', rate: 48000, framesPerChunk: 480, fecK: 8, interleave: 4 },
      streamGen: 1,
      playing: true,
      members: Object.keys(online).map((id) => ({
        nodeId: id,
        syncErrorUs: sync[id] ?? 0,
        offsetUs: 0,
        driftRatio: 1,
        underruns: 0,
        clockQuality: 'good' as const,
        online: online[id],
      })),
    }
  }

  function byId(rows: MemberRow[]): Map<string, MemberRow> {
    return new Map(rows.map((r) => [r.node.id, r]))
  }

  it('master row → sync "—", kind master, no error', () => {
    const rows = byId(
      memberRows(group('g', ['m', 'l']), statusFor('m', { m: true, l: true }, { l: 380 }), nodes),
    )
    expect(rows.get('m')!.kind).toBe('master')
    expect(rows.get('m')!.syncErrorUs).toBeNull()
    expect(rows.get('l')!.kind).toBe('listener')
    expect(rows.get('l')!.syncErrorUs).toBe(380)
    expect(rows.get('l')!.showRole).toBe(true)
  })

  it('sink-less master → masterNoAudio, no role', () => {
    const slNodes = new Map(nodes)
    slNodes.set('m', node('m', { caps: caps({ render: false }) }))
    const rows = byId(
      memberRows(group('g', ['m', 'l']), statusFor('m', { m: true, l: true }), slNodes),
    )
    expect(rows.get('m')!.kind).toBe('masterNoAudio')
    expect(rows.get('m')!.showRole).toBe(false)
  })

  it('sink-less non-master → noSink, no role, no sync', () => {
    const rows = byId(
      memberRows(group('g', ['m', 'sl']), statusFor('m', { m: true, sl: true }), nodes),
    )
    expect(rows.get('sl')!.kind).toBe('noSink')
    expect(rows.get('sl')!.showRole).toBe(false)
    expect(rows.get('sl')!.syncErrorUs).toBeNull()
  })

  it('offline member → online false, sync null (last-known dimming)', () => {
    const rows = byId(
      memberRows(group('g', ['m', 'off']), statusFor('m', { m: true, off: false }, { off: 500 }), nodes),
    )
    expect(rows.get('off')!.online).toBe(false)
    expect(rows.get('off')!.syncErrorUs).toBeNull()
    expect(rows.get('off')!.kind).toBe('listener')
  })

  it('no status → rows fall back to liveness map, sync null', () => {
    const rows = byId(memberRows(group('g', ['m', 'l']), undefined, nodes, { m: true, l: true }))
    expect(rows.get('l')!.online).toBe(true)
    expect(rows.get('l')!.syncErrorUs).toBeNull()
  })
})

describe('least-capable listener naming (04 §4.3.2)', () => {
  // Worked example: {N2,N3 opus / N4 pcm} listeners, N1 master.
  const n1 = node('N1') // master (renders both)
  const n2 = node('N2', { caps: caps({ decode: ['pcm', 'opus'] }) })
  const n3 = node('N3', { caps: caps({ decode: ['pcm', 'opus'] }) })
  const n4 = node('N4', { caps: caps({ decode: ['pcm'] }) }) // pcm only

  function st(master: string): GroupStatus {
    return {
      groupId: 'g',
      masterNodeId: master,
      profile: { codec: 'pcm', fec: 'xorParity', rate: 48000, framesPerChunk: 480, fecK: 8, interleave: 4 },
      streamGen: 1,
      playing: false,
      members: [],
    }
  }

  it('N4 (pcm-only) floors the codec to pcm and is named the limiter', () => {
    const nodes = new Map([n1, n2, n3, n4].map((n) => [n.id, n]))
    const lc = leastCapableCodec(group('g', ['N1', 'N2', 'N3', 'N4']), st('N1'), nodes)
    expect(lc.value).toBe('pcm')
    expect(lc.nodeId).toBe('N4')
  })

  it('drop N4 → opus is universal, no limiter named', () => {
    const nodes = new Map([n1, n2, n3].map((n) => [n.id, n]))
    const lc = leastCapableCodec(group('g', ['N1', 'N2', 'N3']), st('N1'), nodes)
    expect(lc.value).toBe('opus')
    expect(lc.nodeId).toBeUndefined()
  })

  it('a sink-less master never constrains the codec', () => {
    // Master is sink-less + pcm-only, but it is the master (not a listener), so
    // it must not floor the listeners' opus.
    const slMaster = node('M', { caps: caps({ render: false, decode: ['pcm'] }) })
    const nodes = new Map([slMaster, n2, n3].map((n) => [n.id, n]))
    const lc = leastCapableCodec(group('g', ['M', 'N2', 'N3']), st('M'), nodes)
    expect(lc.value).toBe('opus')
    expect(lc.nodeId).toBeUndefined()
  })
})
