import { describe, it, expect, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import { get } from 'svelte/store'
import HwDelayControl from '../HwDelayControl.svelte'
import { setLoaded, draft, __resetForTest } from '../../../lib/nodeStore'
import type { NodeDetailView } from '../../../lib/node'

function node(): NodeDetailView {
  return {
    id: 'n-1',
    name: 'Hall',
    addrs: [],
    hwDelayUs: 0,
    channel: 'stereo',
    gainDb: 0,
    online: true,
    caps: { render: true, sinks: ['alsa'], encode: ['pcm'], decode: ['pcm'], fec: ['none'], maxRate: 48000 },
  }
}

beforeEach(() => {
  __resetForTest()
  setLoaded(node())
})
afterEach(cleanup)

describe('HwDelayControl two-way bind (§5.7)', () => {
  it('moving the slider stages an integer µs hwDelayUs edit', async () => {
    render(HwDelayControl, { props: { value: 0, disabled: false } })
    const slider = screen.getByLabelText('Hardware delay slider in microseconds') as HTMLInputElement
    await fireEvent.input(slider, { target: { value: '8200' } })
    expect(get(draft).hwDelayUs).toBe(8200)
  })

  it('typing into the numeric box stages the same field (integer µs)', async () => {
    render(HwDelayControl, { props: { value: 0, disabled: false } })
    const box = screen.getByLabelText('Hardware delay in microseconds') as HTMLInputElement
    await fireEvent.input(box, { target: { value: '1500.7' } })
    expect(get(draft).hwDelayUs).toBe(1501) // rounded to integer µs
  })

  it('an out-of-slider value typed in the numeric box is preserved', async () => {
    render(HwDelayControl, { props: { value: 0, disabled: false } })
    const box = screen.getByLabelText('Hardware delay in microseconds') as HTMLInputElement
    await fireEvent.input(box, { target: { value: '50000' } }) // beyond 20000 slider max
    expect(get(draft).hwDelayUs).toBe(50000)
  })

  it('negative input clamps to 0', async () => {
    render(HwDelayControl, { props: { value: 0, disabled: false } })
    const box = screen.getByLabelText('Hardware delay in microseconds') as HTMLInputElement
    await fireEvent.input(box, { target: { value: '-200' } })
    expect(get(draft).hwDelayUs).toBe(0)
  })

  it('disabled blocks edits', () => {
    render(HwDelayControl, { props: { value: 100, disabled: true } })
    const slider = screen.getByLabelText('Hardware delay slider in microseconds') as HTMLInputElement
    expect(slider.disabled).toBe(true)
  })
})
