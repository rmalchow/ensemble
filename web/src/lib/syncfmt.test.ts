import { describe, it, expect } from 'vitest'
import { syncMs, syncLevel, SYNC_WARN_US } from './syncfmt'

describe('syncMs', () => {
  const cases: [number | null, string][] = [
    [380, '+0.38 ms'],
    [-1800, '-1.80 ms'],
    [0, '+0.00 ms'],
    [1000, '+1.00 ms'],
    [null, '—'],
  ]
  it.each(cases)('syncMs(%s) → %s', (us, want) => {
    expect(syncMs(us)).toBe(want)
  })

  it('NaN/undefined → "—" (master / no data)', () => {
    expect(syncMs(undefined)).toBe('—')
    expect(syncMs(NaN)).toBe('—')
  })
})

describe('syncLevel', () => {
  it('warn edge is SYNC_WARN_US (1 ms, A.12 HardErrSamp / A.13 P4)', () => {
    expect(SYNC_WARN_US).toBe(1000)
  })
  const cases: [number | null, 'ok' | 'warn'][] = [
    [900, 'ok'],
    [999, 'ok'],
    [1000, 'warn'], // at the edge → warn
    [-1000, 'warn'], // sign-independent
    [5000, 'warn'],
    [0, 'ok'],
    [null, 'ok'], // master is the reference, never off
  ]
  it.each(cases)('syncLevel(%s) → %s', (us, want) => {
    expect(syncLevel(us)).toBe(want)
  })
})
