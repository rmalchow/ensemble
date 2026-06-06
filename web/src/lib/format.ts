// Shared human-readable formatters for the UI. Dependency-free. bytes()/
// duration() are copied from mpvsync; ms()/syncErrUs() are added for the
// Dashboard / Node-detail sync-error display (09 §3, §6).

const UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']

// bytes formats a byte count with a binary-ish step (1024) and one decimal for
// sub-TB sizes, e.g. 8_000_000_000 → "7.5 GB". Negative/NaN guards to "0 B".
export function bytes(n: number | undefined): string {
  if (!n || n < 0 || !Number.isFinite(n)) return '0 B'
  let v = n
  let i = 0
  while (v >= 1024 && i < UNITS.length - 1) {
    v /= 1024
    i++
  }
  const digits = i === 0 ? 0 : 1
  return `${v.toFixed(digits)} ${UNITS[i]}`
}

// duration formats seconds as "H h MM m SS s", dropping leading zero units:
// 15783 → "4h 23m 03s", 125 → "2m 05s", 7 → "7s".
export function duration(sec: number | undefined): string {
  if (!sec || sec < 0 || !Number.isFinite(sec)) return '0s'
  const total = Math.floor(sec)
  const h = Math.floor(total / 3600)
  const m = Math.floor((total / 60) % 60)
  const s = total % 60
  const mm = String(m).padStart(2, '0')
  const ss = String(s).padStart(2, '0')
  if (h > 0) return `${h}h ${mm}m ${ss}s`
  if (m > 0) return `${m}m ${ss}s`
  return `${s}s`
}

// ms formats a microsecond value as milliseconds with one decimal, e.g.
// 1500 → "1.5 ms", 0 → "0.0 ms". NaN/undefined guard to "—".
export function ms(us: number | undefined): string {
  if (us === undefined || !Number.isFinite(us)) return '—'
  return `${(us / 1000).toFixed(1)} ms`
}

// mmss formats seconds as "M:SS" (or "MM:SS"); minutes may exceed 59 (no hour
// rollover). Coarse by design — for the Media now-playing readout "0:48 / 3:12"
// (09 §7). Distinct from media's editor-grade timecode (H:MM:SS.mmm). Negative/
// NaN/undefined guard to "0:00", matching the bytes()/duration() guard idiom.
export function mmss(sec: number | undefined): string {
  if (!sec || sec < 0 || !Number.isFinite(sec)) return '0:00'
  const total = Math.floor(sec)
  const m = Math.floor(total / 60)
  const s = total % 60
  return `${m}:${String(s).padStart(2, '0')}`
}

// fmtFingerprint renders a cert/CA fingerprint as colon-grouped uppercase hex
// behind a `SHA256:` prefix, e.g. "sha256:1f2aab…9c" → "SHA256:1F:2A:AB:…:9C".
// Already-colon-grouped input is normalised (case/spacing). Used by the Setup
// Wizard adopt panel and Settings cluster-info CA fingerprint (09 §1 / §8).
export function fmtFingerprint(fp: string | undefined): string {
  if (!fp) return '—'
  // Strip an optional algorithm prefix ("sha256:") and any existing separators.
  const m = fp.match(/^([a-z0-9]+):(.*)$/i)
  const algo = (m ? m[1] : 'sha256').toUpperCase()
  const hex = (m ? m[2] : fp).replace(/[^0-9a-fA-F]/g, '').toUpperCase()
  if (!hex) return fp
  const groups = hex.match(/.{1,2}/g) ?? [hex]
  return `${algo}:${groups.join(':')}`
}

// fmtDate renders an ISO-8601 timestamp as a short local date, e.g.
// "2026-05-01T12:00:00Z" → "2026-05-01". Invalid/empty → "—". Created/last-used
// columns (09 §8) use this.
export function fmtDate(iso: string | null | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  const yyyy = d.getFullYear()
  const mm = String(d.getMonth() + 1).padStart(2, '0')
  const dd = String(d.getDate()).padStart(2, '0')
  return `${yyyy}-${mm}-${dd}`
}

// syncErrUs renders a signed sync-error magnitude in the most readable unit:
// sub-millisecond as "±NNN µs", larger as "±N.N ms". The sign is preserved so
// lead/lag is visible. NaN/undefined guard to "—". The ⚠ threshold itself is
// owned by 06 — this only formats the value.
export function syncErrUs(us: number | undefined): string {
  if (us === undefined || !Number.isFinite(us)) return '—'
  const sign = us < 0 ? '-' : us > 0 ? '+' : ''
  const mag = Math.abs(us)
  if (mag < 1000) return `${sign}${Math.round(mag)} µs`
  return `${sign}${(mag / 1000).toFixed(1)} ms`
}
