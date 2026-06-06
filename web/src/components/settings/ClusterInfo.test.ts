import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, waitFor } from '@testing-library/svelte'
import ClusterInfo from './ClusterInfo.svelte'

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

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

describe('ClusterInfo', () => {
  it('renders name/created/counts + a copyable, formatted CA fingerprint', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        jsonResp(
          200,
          {
            version: 42,
            cluster: { name: 'Living Room', caFingerprint: 'sha256:1f2aab9c', created: '2026-03-18T0:0:0Z' },
            counts: { nodes: 4, groups: 2 },
          },
          '"42"',
        ),
      ),
    )
    render(ClusterInfo)
    await waitFor(() => expect(screen.getByText('Living Room')).toBeTruthy())
    expect(screen.getByText('4')).toBeTruthy()
    expect(screen.getByText('2')).toBeTruthy()
    expect(screen.getByText('SHA256:1F:2A:AB:9C')).toBeTruthy()
    expect(screen.getByText('Copy')).toBeTruthy()
  })
})
