// Client-side password-strength estimate for the Setup Wizard create-path and
// the Settings change-password panel (09 §1 / §8). This is ADVISORY ONLY — the
// authoritative policy and the argon2id hash (m=64MiB, t=3, p=4 per doc 03/07)
// live server-side. The meter never gates submit on strength; it only nudges.

export interface Strength {
  // score is 0..4 (the canonical zxcvbn-style bucketing, kept dependency-free).
  score: 0 | 1 | 2 | 3 | 4
  // label is the human word shown next to the bar.
  label: 'empty' | 'weak' | 'fair' | 'good' | 'strong'
}

const LABELS = ['empty', 'weak', 'fair', 'good', 'strong'] as const

// estimate scores a candidate password by length and character-class variety.
// It is intentionally simple and monotonic-ish: longer + more classes never
// scores lower than a strict subset. Empty → 0/"empty".
export function estimate(pw: string): Strength {
  if (!pw) return { score: 0, label: 'empty' }

  let classes = 0
  if (/[a-z]/.test(pw)) classes++
  if (/[A-Z]/.test(pw)) classes++
  if (/[0-9]/.test(pw)) classes++
  if (/[^A-Za-z0-9]/.test(pw)) classes++

  const len = pw.length

  // Length is the dominant factor (a long passphrase beats a short complex
  // password — D11 favours passphrases). Buckets accumulate, then clamp to 4.
  let points = 0
  if (len >= 8) points++
  if (len >= 12) points++
  if (len >= 16) points++
  if (len >= 24) points++
  // Character-class variety contributes up to 2 extra points but cannot, alone,
  // push a tiny password to the top — short strings are capped below.
  points += Math.max(0, classes - 1)

  let score = Math.min(points, 4)
  // Hard caps so a 4-char "aB1!" cannot read "strong".
  if (len < 8) score = Math.min(score, 1)
  else if (len < 12) score = Math.min(score, 2)
  else if (len < 16) score = Math.min(score, 3)

  const s = score as 0 | 1 | 2 | 3 | 4
  return { score: s, label: LABELS[s] }
}
