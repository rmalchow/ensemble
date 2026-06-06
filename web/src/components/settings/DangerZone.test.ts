import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'
import DangerZone from './DangerZone.svelte'
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

beforeEach(() => configVersion.set(46))
afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

describe('DangerZone', () => {
  it('leave is gated behind a typed confirm matching the cluster name', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL, _init?: RequestInit) =>
        jsonResp(200, { version: 47, leftNodeId: 'n-1', coordinated: true }, '"47"'),
    )
    vi.stubGlobal('fetch', fetchSpy)
    const onLeft = vi.fn()

    render(DangerZone, { clusterName: 'Home', onLeft })
    await fireEvent.click(screen.getByText('Leave / reset this cluster'))

    const leaveBtn = screen.getByText('Leave cluster').closest('button') as HTMLButtonElement
    expect(leaveBtn.disabled).toBe(true) // nothing typed yet

    await fireEvent.input(screen.getByLabelText(/Type/), { target: { value: 'Home' } })
    expect(leaveBtn.disabled).toBe(false)
    await fireEvent.click(leaveBtn)

    await waitFor(() => expect(fetchSpy).toHaveBeenCalledOnce())
    const init = fetchSpy.mock.calls[0][1] as RequestInit
    expect((init.headers as Record<string, string>)['If-Match']).toBe('46')
    await waitFor(() => expect(onLeft).toHaveBeenCalledWith(true))
  })

  it('coordinated:false still reports leave (App warns + re-probes)', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        jsonResp(200, { version: 47, leftNodeId: 'n-1', coordinated: false }, '"47"'),
      ),
    )
    const onLeft = vi.fn()
    render(DangerZone, { clusterName: 'Home', onLeft })
    await fireEvent.click(screen.getByText('Leave / reset this cluster'))
    await fireEvent.input(screen.getByLabelText(/Type/), { target: { value: 'Home' } })
    await fireEvent.click(screen.getByText('Leave cluster'))
    await waitFor(() => expect(onLeft).toHaveBeenCalledWith(false))
  })
})
