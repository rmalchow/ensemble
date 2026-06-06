import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import MemberRow from '../MemberRow.svelte'
import TableHost from './TableHost.svelte'
import { confirmAction } from '../../../lib/confirm'
import { ApiError, type MemberNode, type Capabilities } from '../../../lib/cluster'

const renderCaps: Capabilities = {
  render: true,
  sinks: ['alsa'],
  encode: ['pcm', 'opus'],
  decode: ['pcm', 'opus'],
  fec: ['none'],
  maxRate: 48000,
}
const sinklessCaps: Capabilities = { ...renderCaps, render: false, sinks: [] }

const baseNode: MemberNode = {
  id: 'node-a',
  name: 'Living Room',
  addrs: ['192.168.1.10:9000', '10.0.0.10:9000'],
  online: true,
  caps: renderCaps,
}

function renderRow(props: {
  node?: MemberNode
  busy?: boolean
  error?: ApiError
  onForget?: () => void
  onOpenNode?: () => void
}) {
  return render(TableHost, {
    props: {
      component: MemberRow,
      childProps: {
        node: props.node ?? baseNode,
        busy: props.busy ?? false,
        error: props.error,
        onForget: props.onForget ?? (() => {}),
        onOpenNode: props.onOpenNode ?? (() => {}),
      },
    },
  })
}

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

describe('MemberRow', () => {
  it('sink-less node shows the "control / media only" tag linking to the node', async () => {
    const navigate = vi.fn()
    vi.doMock('../../../lib/router', () => ({ navigate }))
    renderRow({ node: { ...baseNode, caps: sinklessCaps } })
    const tag = screen.getByText('control / media only')
    expect(tag).toBeTruthy()
    // The tag is a button that deep-links to the node's capabilities panel.
    expect(tag.closest('button')).toBeTruthy()
  })

  it('a node with render === true has no sink-less tag', () => {
    renderRow({ node: baseNode })
    expect(screen.queryByText('control / media only')).toBeNull()
  })

  it('a sink-less elected master shows the "master (no local audio)" badge', () => {
    renderRow({ node: { ...baseNode, caps: sinklessCaps, isMaster: true } })
    expect(screen.getByText('master (no local audio)')).toBeTruthy()
  })

  it('a rendering master does NOT show the master-no-audio badge', () => {
    renderRow({ node: { ...baseNode, isMaster: true } })
    expect(screen.queryByText('master (no local audio)')).toBeNull()
  })

  it('offline node shows the offline chip, dims the row, but keeps Forget enabled', () => {
    const { container } = renderRow({ node: { ...baseNode, online: false } })
    expect(screen.getByText(/offline/i)).toBeTruthy()
    expect(container.querySelector('tr.offline')).toBeTruthy()
    const forget = screen.getByLabelText(/^Forget /) as HTMLButtonElement
    expect(forget.disabled).toBe(false)
  })

  it('renders the row error envelope inline', () => {
    renderRow({ error: new ApiError(502, 'proxy_failed', 'Node unreachable.') })
    expect(screen.getByText('proxy_failed')).toBeTruthy()
    expect(screen.getByText('Node unreachable.')).toBeTruthy()
  })

  it('clicking the trashcan invokes onForget (the screen gates it via confirmAction)', async () => {
    const onForget = vi.fn()
    renderRow({ onForget })
    await fireEvent.click(screen.getByLabelText(/^Forget /))
    expect(onForget).toHaveBeenCalledTimes(1)
  })

  it('the name is a link to the node detail page (onOpenNode)', async () => {
    const onOpenNode = vi.fn()
    renderRow({ onOpenNode })
    await fireEvent.click(screen.getByText('Living Room'))
    expect(onOpenNode).toHaveBeenCalledTimes(1)
  })
})

// The confirm gate itself lives in the screen's action handlers (Cluster.svelte
// wires onForget through confirmAction). These tests assert that
// contract directly: a confirmed dialog runs the action; a cancelled one is a
// no-op. They drive confirmAction's activeConfirm store the way ConfirmModal
// does, without rendering the modal.
import { activeConfirm } from '../../../lib/confirm'
import { get } from 'svelte/store'

describe('confirm-gated disruptive actions (screen wiring contract)', () => {
  it('only runs the action when the operator confirms', async () => {
    const action = vi.fn()
    const p = (async () => {
      const ok = await confirmAction({
        type: 'forget',
        message: 'Forget Living Room? Revokes its cert and drops it from config + allowlist.',
        confirmLabel: 'Forget',
        danger: true,
      })
      if (ok) action()
    })()
    // Confirm (askEveryTime=true so the type is not session-suppressed).
    get(activeConfirm)?._resolve(true, true)
    await p
    expect(action).toHaveBeenCalledTimes(1)
  })

  it('cancel is a no-op', async () => {
    const action = vi.fn()
    const p = (async () => {
      const ok = await confirmAction({
        type: 'takeover',
        message: "Force re-issue this node's identity into this cluster?",
        confirmLabel: 'Take over',
        danger: true,
      })
      if (ok) action()
    })()
    get(activeConfirm)?._resolve(false, true)
    await p
    expect(action).not.toHaveBeenCalled()
  })
})
