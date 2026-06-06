import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'
import ChangePassword from './ChangePassword.svelte'
import { configVersion } from '../../lib/stores'

function jsonResp(status: number, body: unknown, etag?: string): Response {
  const headers = new Headers()
  if (etag) headers.set('ETag', etag)
  return {
    status,
    ok: status >= 200 && status < 300,
    statusText: '',
    headers,
    json: async () => body,
    text: async () => JSON.stringify(body),
  } as unknown as Response
}

beforeEach(() => configVersion.set(42))
afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

async function fill(cur: string, next: string, confirm: string) {
  await fireEvent.input(screen.getByLabelText(/Current password/), { target: { value: cur } })
  await fireEvent.input(screen.getByLabelText(/^New password/), { target: { value: next } })
  await fireEvent.input(screen.getByLabelText(/Confirm new password/), {
    target: { value: confirm },
  })
}

describe('ChangePassword', () => {
  it('sends If-Match: configVersion on the POST', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL, _init?: RequestInit) =>
        jsonResp(200, { version: 43 }, '"43"'),
    )
    vi.stubGlobal('fetch', fetchSpy)
    render(ChangePassword, {})
    await fill('old', 'new-strong-pass', 'new-strong-pass')
    await fireEvent.click(screen.getByText('Update password'))
    await waitFor(() => expect(fetchSpy).toHaveBeenCalledOnce())
    const init = fetchSpy.mock.calls[0][1] as RequestInit
    expect((init.headers as Record<string, string>)['If-Match']).toBe('42')
  })

  it('mismatched confirm blocks submit + shows field error', async () => {
    render(ChangePassword, {})
    await fill('old', 'aaaaaaaa', 'bbbbbbbb')
    expect(screen.getByText('Passwords do not match.')).toBeTruthy()
    const btn = screen.getByText('Update password').closest('button') as HTMLButtonElement
    expect(btn.disabled).toBe(true)
  })

  it('wrong current (401) → inline current-field error', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        jsonResp(401, { error: { code: 'unauthenticated', message: 'bad current' } }),
      ),
    )
    render(ChangePassword, {})
    await fill('wrong', 'new-strong-pass', 'new-strong-pass')
    await fireEvent.click(screen.getByText('Update password'))
    await waitFor(() => expect(screen.getByText('Current password is incorrect.')).toBeTruthy())
  })

  it('409 → reload-&-reapply banner', async () => {
    const onReloadReapply = vi.fn()
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        jsonResp(409, { error: { code: 'version_conflict', message: 'stale' } }),
      ),
    )
    render(ChangePassword, { onReloadReapply })
    await fill('old', 'new-strong-pass', 'new-strong-pass')
    await fireEvent.click(screen.getByText('Update password'))
    await waitFor(() => expect(screen.getByText('Reload & reapply')).toBeTruthy())
    await fireEvent.click(screen.getByText('Reload & reapply'))
    expect(onReloadReapply).toHaveBeenCalledOnce()
  })
})
