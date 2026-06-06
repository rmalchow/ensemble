// Copy-to-clipboard helper (factored out — media copied this inline per
// component). Used by CopyField for node id / CSR fingerprint / CA fingerprint /
// once-shown API key. Returns true on success, false if the platform refused
// (insecure context, denied permission) so the caller can show a fallback.

export async function copy(text: string): Promise<boolean> {
  try {
    if (
      typeof navigator !== 'undefined' &&
      navigator.clipboard &&
      typeof navigator.clipboard.writeText === 'function'
    ) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    // fall through to the legacy path
  }
  // Legacy fallback for non-secure contexts (the dev proxy is http on the LAN).
  try {
    if (typeof document === 'undefined') return false
    const el = document.createElement('textarea')
    el.value = text
    el.setAttribute('readonly', '')
    el.style.position = 'absolute'
    el.style.left = '-9999px'
    document.body.appendChild(el)
    el.select()
    const ok = document.execCommand('copy')
    document.body.removeChild(el)
    return ok
  } catch {
    return false
  }
}
