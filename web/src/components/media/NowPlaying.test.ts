import { describe, it, expect, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'
import NowPlaying from './NowPlaying.svelte'

afterEach(cleanup)

describe('NowPlaying (09 §7)', () => {
  it('shows file + loop + group + cursor when positionSec is present', () => {
    render(NowPlaying, {
      props: { file: 'jazz-loop.mp3', loop: true, groupName: 'Downstairs', positionSec: 48, lengthSec: 192 },
    })
    expect(screen.getByText('jazz-loop.mp3')).toBeTruthy()
    expect(screen.getByText('on group Downstairs')).toBeTruthy()
    expect(screen.getByText(/0:48 \/ 3:12/)).toBeTruthy()
  })

  it('degrades to length-only when no position field exists (G.2 has no cursor)', () => {
    render(NowPlaying, { props: { file: 'ocean.mp3', loop: false, groupName: 'Kitchen', lengthSec: 192 } })
    expect(screen.getByText('ocean.mp3')).toBeTruthy()
    expect(screen.getByText('3:12')).toBeTruthy()
    expect(screen.queryByText(/\//)).toBeNull() // no "pos / len" cursor
  })

  it('idle when nothing is selected', () => {
    render(NowPlaying, { props: { file: null, loop: true, groupName: 'Kitchen' } })
    expect(screen.getByText('Nothing playing on Kitchen.')).toBeTruthy()
  })
})
