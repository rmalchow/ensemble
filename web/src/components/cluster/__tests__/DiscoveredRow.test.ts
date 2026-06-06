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
  onTakeover?: (password: string) => void
}) {
  return render(TableHost, {
    props: {
      component: DiscoveredRow,
      childProps: {
        node: props.node ?? baseNode,
        busy: props.busy ?? false,
        error: props.error,
        onAdopt: props.onAdopt ?? (() => {}),
        onTakeover: props.onTakeover ?? (() => {}),
      },
    },
  })
}

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

describe('DiscoveredRow', () => {
  it('hides the PIN input until Adopt is clicked, then pre-fills the D9 default "0000"', async () => {
    renderRow({})
    expect(screen.queryByLabelText(/Adoption PIN/i)).toBeNull()
    await fireEvent.click(screen.getByText('Adopt'))
    const input = screen.getByLabelText(/Adoption PIN/i) as HTMLInputElement
    expect(input.value).toBe('0000')
  })

  it('fires onAdopt with the default PIN value (arm then confirm)', async () => {
    const onAdopt = vi.fn()
    renderRow({ onAdopt })
    await fireEvent.click(screen.getByText('Adopt')) // arm: reveals the PIN
    expect(onAdopt).not.toHaveBeenCalled()
    await fireEvent.click(screen.getByText('Adopt')) // confirm
    expect(onAdopt).toHaveBeenCalledTimes(1)
    expect(onAdopt).toHaveBeenCalledWith('0000')
  })

  it('fires onAdopt with an edited PIN value (sent verbatim)', async () => {
    const onAdopt = vi.fn()
    renderRow({ onAdopt })
    await fireEvent.click(screen.getByText('Adopt'))
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

  it('a foreign node offers Take over and collects the cluster password', async () => {
    const onTakeover = vi.fn()
    renderRow({ node: { ...baseNode, state: 'foreign' }, onTakeover })
    expect(screen.queryByText('Adopt')).toBeNull()
    await fireEvent.click(screen.getByText('Take over')) // arm: reveals password
    const pw = screen.getByLabelText(/Current cluster password/i) as HTMLInputElement
    await fireEvent.input(pw, { target: { value: 'their-admin-password' } })
    await fireEvent.click(screen.getByText('Take over'))
    expect(onTakeover).toHaveBeenCalledWith('their-admin-password')
  })
})
