// Transient toast host store. Error toasts surface on proxy/action failure
// (09 §3). Toasts auto-dismiss after a timeout unless `sticky`.

import { writable } from 'svelte/store'

export type ToastTone = 'error' | 'success' | 'info'

export interface Toast {
  id: number
  tone: ToastTone
  message: string
  sticky?: boolean
}

export const toasts = writable<Toast[]>([])

let nextId = 1
const DEFAULT_MS = 5000

export function pushToast(
  message: string,
  tone: ToastTone = 'info',
  opts: { sticky?: boolean; timeoutMs?: number } = {},
): number {
  const id = nextId++
  toasts.update((t) => [...t, { id, tone, message, sticky: opts.sticky }])
  if (!opts.sticky) {
    setTimeout(() => dismissToast(id), opts.timeoutMs ?? DEFAULT_MS)
  }
  return id
}

export function dismissToast(id: number): void {
  toasts.update((t) => t.filter((x) => x.id !== id))
}

// Convenience for the common case: a failed action.
export function toastError(message: string): number {
  return pushToast(message, 'error')
}
