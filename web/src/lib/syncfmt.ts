// Sync-error formatting + threshold classification for the Dashboard member
// rows (09 §3). Pure, dependency-free, table-tested.
//
// The warn edge is NOT invented here: A.12 fixes the drift loop's gross-error
// reseek bound at HardErrSamp = 2400 samples (50 ms) @ rate = 48000 Hz, and
// A.13 P4 fixes the steady-state acceptance target at "sub-ms over 10 min". So
// the ⚠ warn edge sits above the sub-ms target but well below the 50 ms reseek
// bound. See §9 open question — confirm the exact edge with the 06 author.

// SYNC_WARN_US is the |syncErrorUs| at/above which a member row flags ⚠
// (1 ms = 1000 µs). Derived from A.12 HardErrSamp (50 ms reseek) + A.13 P4
// (sub-ms target): warn well before the reseek bound, just past the target.
export const SYNC_WARN_US = 1000

export type SyncLevel = 'ok' | 'warn'

// syncMs formats a microsecond sync error as a signed millisecond string with
// two decimals, e.g. 380 → "+0.38 ms", -1800 → "-1.80 ms". A master (or a row
// with no measurable error) is passed `null` and renders the em-dash "—".
export function syncMs(us: number | null | undefined): string {
  if (us === null || us === undefined || !Number.isFinite(us)) return '—'
  const sign = us < 0 ? '-' : '+'
  const mag = Math.abs(us) / 1000
  return `${sign}${mag.toFixed(2)} ms`
}

// syncLevel classifies a sync error: |us| ≥ SYNC_WARN_US → 'warn', else 'ok'.
// A null/master value is 'ok' (it is the reference, never off).
export function syncLevel(us: number | null | undefined): SyncLevel {
  if (us === null || us === undefined || !Number.isFinite(us)) return 'ok'
  return Math.abs(us) >= SYNC_WARN_US ? 'warn' : 'ok'
}
