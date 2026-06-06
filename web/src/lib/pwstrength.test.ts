import { describe, it, expect } from 'vitest'
import { estimate } from './pwstrength'

describe('pwstrength.estimate', () => {
  it('empty → score 0 / "empty"', () => {
    expect(estimate('')).toEqual({ score: 0, label: 'empty' })
  })

  it('short complex password is capped low (not "strong")', () => {
    // 4 chars, all classes — must not read strong (hard length cap).
    const s = estimate('aB1!')
    expect(s.score).toBeLessThanOrEqual(1)
  })

  it('long passphrase scores high even with one class', () => {
    const s = estimate('correcthorsebatterystaple')
    expect(s.score).toBeGreaterThanOrEqual(3)
  })

  it('is monotonic-ish across length buckets (same charset)', () => {
    const buckets = ['aaaa', 'aaaaaaaa', 'aaaaaaaaaaaa', 'aaaaaaaaaaaaaaaa', 'a'.repeat(24)]
    const scores = buckets.map((p) => estimate(p).score)
    for (let i = 1; i < scores.length; i++) {
      expect(scores[i]).toBeGreaterThanOrEqual(scores[i - 1])
    }
  })

  it('adding character classes never lowers the score at fixed length', () => {
    const base = estimate('abcdefghijkl').score // 12 lower
    const more = estimate('abcdEF12!@kl').score // 12, all classes
    expect(more).toBeGreaterThanOrEqual(base)
  })

  it('label tracks score buckets', () => {
    expect(estimate('a'.repeat(24)).label).toBe('strong')
  })
})
