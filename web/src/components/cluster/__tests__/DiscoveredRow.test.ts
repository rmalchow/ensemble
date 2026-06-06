import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import DiscoveredRow from '../DiscoveredRow.svelte'
import { ApiError, type DiscoveredNode } from '../../../lib/cluster'

// DiscoveredRow renders a <tr>, so mount it inside a <table><tbody> host to keep
// the DOM valid (jsdom would otherwise hoist a bare <tr> out of body).
import TableHost from './TableHost.svelte'

const baseNode: DiscoveredNode = {
  nodeId: 'node-b',
  name: 'Kitchen',
  addrs: ['192.168.1.42:7946'],
  fingerprint: 'sha256:1f2a3b4c',
  state: 'uninitialized',
}

function renderRow(props: {
  node?: DiscoveredNode
  busy?: boolean
  error?: ApiError
  onAdopt?: (pin: string) => void
}) {
  return render(TableHost, {
    props: {
      component: DiscoveredRow,
      childProps: {
        node: props.node ?? baseNode,
        busy: props.busy ?? false,
        error: props.error,
        onAdopt: props.onAdopt ?? (() => {}),
      },
    },
  })
}

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

describe('DiscoveredRow', () => {
  it('renders the PIN input pre-filled with the D9 default "0000"', () => {
    renderRow({})
    const input = screen.getByLabelText(/Adoption PIN/i) as HTMLInputElement
    expect(input.value).toBe('0000')
  })

  it('fires onAdopt with the default PIN value', async () => {
    const onAdopt = vi.fn()
    renderRow({ onAdopt })
    await fireEvent.click(screen.getByText('Adopt'))
    expect(onAdopt).toHaveBeenCalledTimes(1)
    expect(onAdopt).toHaveBeenCalledWith('0000')
  })

  it('fires onAdopt with an edited PIN value (sent verbatim)', async () => {
    const onAdopt = vi.fn()
    renderRow({ onAdopt })
    const input = screen.getByLabelText(/Adoption PIN/i) as HTMLInputElement
    await fireEvent.input(input, { target: { value: '8421' } })
    await fireEvent.click(screen.getByText('Adopt'))
    expect(onAdopt).toHaveBeenCalledWith('8421')
  })

  it('displays the CSR fingerprint (operator verifies out-of-band)', () => {
    renderRow({})
    // fmtFingerprint colon-groups + uppercases the hex behind a SHA256: prefix.
    expect(screen.getByText('SHA256:1F:2A:3B:4C')).toBeTruthy()
  })

  it('busy disables Adopt and does not fire onAdopt', async () => {
    const onAdopt = vi.fn()
    renderRow({ busy: true, onAdopt })
    const btn = screen.getByText('Adopt').closest('button') as HTMLButtonElement
    expect(btn.disabled).toBe(true)
    await fireEvent.click(btn)
    expect(onAdopt).not.toHaveBeenCalled()
  })

  it('renders the error envelope code + message inline', () => {
    renderRow({
      error: new ApiError(401, 'unauthenticated', 'Adoption rejected.'),
    })
    expect(screen.getByText('unauthenticated')).toBeTruthy()
    expect(screen.getByText('Adoption rejected.')).toBeTruthy()
  })

  it('a foreign node shows the Takeover-instead hint', () => {
    renderRow({ node: { ...baseNode, state: 'foreign' } })
    expect(screen.getByText(/use/i).textContent).toContain('Takeover')
  })
})
