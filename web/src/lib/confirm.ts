// A single shared confirmation modal, replacing native confirm(). Components
// call confirmAction() and await a boolean. Each confirmation has a `type`; if
// the user unticks "Ask every time" when confirming, that type is suppressed
// for the rest of the session (auto-confirmed without a modal).

import { writable } from 'svelte/store'

export interface ConfirmOpts {
  // type identifies the confirmation kind for session-only suppression.
  type: string
  title?: string
  message: string
  confirmLabel?: string
  danger?: boolean
}

// suppressed is SESSION-ONLY and in-memory: it intentionally does not use
// localStorage/sessionStorage, so a page reload restores all confirmations.
const suppressed = new Set<string>()

// activeConfirm holds the request currently shown by ConfirmModal, or null.
export const activeConfirm = writable<
  (ConfirmOpts & { _resolve: (ok: boolean, askEveryTime: boolean) => void }) | null
>(null)

// confirmAction shows the modal and resolves true (confirmed) or false
// (cancelled). A suppressed type resolves true immediately without a modal.
export async function confirmAction(opts: ConfirmOpts): Promise<boolean> {
  if (suppressed.has(opts.type)) return true
  return new Promise<boolean>((resolve) => {
    activeConfirm.set({
      ...opts,
      _resolve(ok: boolean, askEveryTime: boolean) {
        // Only a confirmation (never a cancel) can suppress, and only when the
        // user unticks "Ask every time".
        if (ok && !askEveryTime) suppressed.add(opts.type)
        activeConfirm.set(null)
        resolve(ok)
      },
    })
  })
}
