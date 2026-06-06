import { describe, it, expect } from 'vitest'
import { bytes, duration, ms, syncErrUs, fmtFingerprint, fmtDate, mmss } from './format'

describe('fmtFingerprint', () => {
  it('colon-groups bare hex behind SHA256:', () => {
    expect(fmtFingerprint('sha256:1f2aab9c')).toBe('SHA256:1F:2A:AB:9C')
  })
  it('normalises already-grouped input', () => {
    expect(fmtFingerprint('SHA256:1f:2a:ab:9c')).toBe('SHA256:1F:2A:AB:9C')
  })
  it('defaults algorithm to SHA256 when no prefix', () => {
    expect(fmtFingerprint('aabb')).toBe('SHA256:AA:BB')
  })
  it('undefined/empty → "—"', () => {
    expect(fmtFingerprint(undefined)).toBe('—')
    expect(fmtFingerprint('')).toBe('—')
  })
})

describe('fmtDate', () => {
  it('ISO → yyyy-mm-dd', () => {
    expect(fmtDate('2026-05-01T12:00:00Z')).toMatch(/^\d{4}-\d{2}-\d{2}$/)
  })
  it('null/empty/invalid → "—"', () => {
    expect(fmtDate(null)).toBe('—')
    expect(fmtDate('')).toBe('—')
    expect(fmtDate('not-a-date')).toBe('—')
  })
})

describe('bytes', () => {
  const cases: [number | undefined, string][] = [
    [0, '0 B'],
    [undefined, '0 B'],
    [-5, '0 B'],
    [NaN, '0 B'],
    [512, '512 B'],
    [1024, '1.0 KB'],
    [8_000_000_000, '7.5 GB'],
  ]
  it.each(cases)('bytes(%s) === %s', (n, want) => {
    expect(bytes(n)).toBe(want)
  })
})

describe('duration', () => {
  const cases: [number | undefined, string][] = [
    [0, '0s'],
    [undefined, '0s'],
    [-1, '0s'],
    [NaN, '0s'],
    [7, '7s'],
    [125, '2m 05s'],
    [15783, '4h 23m 03s'],
  ]
  it.each(cases)('duration(%s) === %s', (sec, want) => {
    expect(duration(sec)).toBe(want)
  })
})

describe('ms', () => {
  const cases: [number | undefined, string][] = [
    [0, '0.0 ms'],
    [1500, '1.5 ms'],
    [undefined, '—'],
    [NaN, '—'],
  ]
  it.each(cases)('ms(%s) === %s', (us, want) => {
    expect(ms(us)).toBe(want)
  })
})

describe('syncErrUs', () => {
  const cases: [number | undefined, string][] = [
    [0, '0 µs'],
    [250, '+250 µs'],
    [-250, '-250 µs'],
    [1500, '+1.5 ms'],
    [-2400, '-2.4 ms'],
    [undefined, '—'],
    [NaN, '—'],
  ]
  it.each(cases)('syncErrUs(%s) === %s', (us, want) => {
    expect(syncErrUs(us)).toBe(want)
  })
})

describe('mmss (09 §7 now-playing readout)', () => {
  const cases: [number | undefined, string][] = [
    [48, '0:48'],
    [192, '3:12'],
    [3675, '61:15'], // minutes may exceed 59 (no hour rollover)
    [0, '0:00'],
    [undefined, '0:00'],
    [-1, '0:00'],
    [NaN, '0:00'],
  ]
  it.each(cases)('mmss(%s) === %s', (sec, want) => {
    expect(mmss(sec)).toBe(want)
  })
})
