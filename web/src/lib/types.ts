// TS mirrors of the canonical JSON shapes (README §6.5 / 08 §0.7). Field names
// are the canonical ones verbatim — screens import these for typed reads/writes.

export type Channel = 'stereo' | 'left' | 'right'
export type CodecName = 'pcm' | 'opus'
export type FECName = 'none' | 'xorParity' | 'duplicate'
export type Transport = 'udp' | 'tcp'

export interface Capabilities {
  render: boolean
  sinks: string[] // "alsa" | "exec:aplay" | ...
  encode: CodecName[]
  decode: CodecName[]
  fec: FECName[]
  maxRate: number
}

export interface NodeRecord {
  id: string
  name: string
  addrs: string[]
  hwDelayUs: number
  channel: Channel
  gainDb: number
  caps: Capabilities
}

export interface Profile {
  codec: CodecName
  fec: FECName
  rate: number
  framesPerChunk: number
  fecK: number
  interleave: number
}

export interface GroupRecord {
  id: string
  name: string
  memberNodeIds: string[]
  profile: Profile
  media: { file: string; loop: boolean }
  playing: boolean
}

export interface ErrorEnvelope {
  error: { code: string; message: string }
}

// Live status (08 G.2), non-replicated.
export interface MemberStatus {
  nodeId: string
  syncErrorUs: number
  offsetUs: number
  driftRatio: number
  underruns: number
  clockQuality: 'good' | 'fair' | 'poor'
  online: boolean
}

export interface GroupStatus {
  groupId: string
  masterNodeId: string
  profile: Profile
  streamGen: number
  playing: boolean
  members: MemberStatus[]
}
