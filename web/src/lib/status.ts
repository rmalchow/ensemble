// First-run probe (P1.4 §5.1, 09 §1). Thin re-export of the normalised probe
// from api.ts so screens import a stable `probeStatus()` name. The actual
// /bootstrap/info-vs-/api/v1/auth/session resolution (08 risk #1) lives in
// api.getStatus(); this module documents the contract the boot machine relies on
// and surfaces the StatusProbe type.

import { getStatus } from './api'
import type { StatusProbe } from './api'

export type { StatusProbe }

// probeStatus returns { initialized, nodeId, fingerprint, clusterName? }. It
// never guesses on error — it rejects, and the boot machine renders the
// resilient error state with Retry (09 §1 "Detection must be resilient").
export async function probeStatus(): Promise<StatusProbe> {
  const { data } = await getStatus()
  return data
}
