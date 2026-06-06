import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'
import SetupWizard from './SetupWizard.svelte'

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

const probe = { initialized: false, nodeId: 'n-7a3f', fingerprint: 'sha256:1f2aab9c' }

beforeEach(() => {
  history.replaceState({}, '', '/setup')
})
afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

describe('SetupWizard', () => {
  it('renders both path selectors', () => {
    render(SetupWizard, { probe })
    expect(screen.getByText('Create a new cluster')).toBeTruthy()
    expect(screen.getByText('Wait to be adopted')).toBeTruthy()
  })

  it('create path: submit is disabled until name + matching passwords', async () => {
    render(SetupWizard, { probe })
    const submit = screen.getByText('Create cluster →').closest('button') as HTMLButtonElement
    expect(submit.disabled).toBe(true)

    await fireEvent.input(screen.getByLabelText(/Cluster name/), { target: { value: 'Home' } })
    await fireEvent.input(screen.getByLabelText(/^Admin password/), {
      target: { value: 'a-good-passphrase' },
    })
    expect(submit.disabled).toBe(true) // confirm still empty
    await fireEvent.input(screen.getByLabelText(/Confirm password/), {
      target: { value: 'a-good-passphrase' },
    })
    expect(submit.disabled).toBe(false)
  })

  it('create path: successful setup() calls navigate("/") without onComplete', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL, _init?: RequestInit) =>
        jsonResp(
          200,
          {
            cluster: { name: 'Home', caFingerprint: 'sha256:aa', created: 't' },
            node: { id: 'n-7a3f', name: 'x' },
            version: 1,
          },
          '"1"',
        ),
    )
    vi.stubGlobal('fetch', fetchSpy)
    const pushSpy = vi.spyOn(history, 'pushState')

    render(SetupWizard, { probe })
    await fireEvent.input(screen.getByLabelText(/Cluster name/), { target: { value: 'Home' } })
    await fireEvent.input(screen.getByLabelText(/^Admin password/), {
      target: { value: 'a-good-passphrase' },
    })
    await fireEvent.input(screen.getByLabelText(/Confirm password/), {
      target: { value: 'a-good-passphrase' },
    })
    await fireEvent.click(screen.getByText('Create cluster →'))

    await waitFor(() => expect(fetchSpy).toHaveBeenCalledOnce())
    const [url, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
    expect(url).toBe('/api/v1/setup')
    expect(JSON.parse(init.body as string)).toMatchObject({
      clusterName: 'Home',
      adminPassword: 'a-good-passphrase',
    })
    await waitFor(() =>
      expect(pushSpy).toHaveBeenCalledWith(expect.anything(), '', '/'),
    )
  })

  it('create path: successful setup() fires onComplete (App re-probe → Dashboard)', async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL, _init?: RequestInit) =>
        jsonResp(
          200,
          {
            cluster: { name: 'Home', caFingerprint: 'sha256:aa', created: 't' },
            node: { id: 'n-7a3f', name: 'x' },
            version: 1,
          },
          '"1"',
        ),
    )
    vi.stubGlobal('fetch', fetchSpy)
    const pushSpy = vi.spyOn(history, 'pushState')
    const onComplete = vi.fn()

    render(SetupWizard, { probe, onComplete })
    await fireEvent.input(screen.getByLabelText(/Cluster name/), { target: { value: 'Home' } })
    await fireEvent.input(screen.getByLabelText(/^Admin password/), {
      target: { value: 'a-good-passphrase' },
    })
    await fireEvent.input(screen.getByLabelText(/Confirm password/), {
      target: { value: 'a-good-passphrase' },
    })
    await fireEvent.click(screen.getByText('Create cluster →'))

    // The wizard delegates the redirect to App's re-probe (whose guard lands on
    // '/'); it must NOT navigate itself — that would run App's stale pre-setup
    // guard and bounce back to /setup.
    await waitFor(() => expect(onComplete).toHaveBeenCalledOnce())
    expect(pushSpy).not.toHaveBeenCalled()
  })

  it('adopt path: shows node id, fingerprint, PIN 0 0 0 0 with secret note', async () => {
    render(SetupWizard, { probe })
    await fireEvent.click(screen.getByText('Wait to be adopted'))
    expect(screen.getByText('n-7a3f')).toBeTruthy()
    // Fingerprint formatted with colon grouping + SHA256 prefix.
    expect(screen.getByText('SHA256:1F:2A:AB:9C')).toBeTruthy()
    expect(screen.getByLabelText('adoption pin').textContent).toContain('0 0 0 0')
    expect(screen.getByText(/treated as a real secret/)).toBeTruthy()
    // Copy buttons present on the two CopyFields.
    expect(screen.getAllByText('Copy').length).toBeGreaterThanOrEqual(2)
  })
})
