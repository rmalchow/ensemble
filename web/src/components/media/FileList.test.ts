import { describe, it, expect, afterEach, vi } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import FileList from './FileList.svelte'
import type { MediaFile } from '../../lib/media'

afterEach(cleanup)

const files: MediaFile[] = [
  { file: 'jazz-loop.mp3', durationMs: 192_000 },
  { file: 'ocean.mp3', durationMs: 588_000 },
  { file: 'chime.mp3', durationMs: 4_000 },
]

function base(p: Partial<Parameters<typeof render>[1] extends { props: infer P } ? P : never> = {}) {
  return {
    files,
    masterNodeName: 'data/ on N1',
    selectedFile: null,
    playing: false,
    loop: true,
    onPlay: vi.fn(),
    onStop: vi.fn(),
    onToggleLoop: vi.fn(),
    ...p,
  }
}

describe('FileList (09 §7)', () => {
  it('renders length via mmss and the master source line', () => {
    render(FileList, { props: base() as never })
    expect(screen.getByText('data/ on N1')).toBeTruthy()
    expect(screen.getByText('3:12')).toBeTruthy() // 192000ms
    expect(screen.getByText('9:48')).toBeTruthy() // 588000ms
    expect(screen.getByText('0:04')).toBeTruthy() // 4000ms
  })

  it('non-selected rows show Select & play; click → onPlay(file)', async () => {
    const onPlay = vi.fn()
    render(FileList, { props: base({ onPlay }) as never })
    const btns = screen.getAllByText(/Select & play/)
    expect(btns).toHaveLength(3)
    await fireEvent.click(btns[0])
    expect(onPlay).toHaveBeenCalledWith('jazz-loop.mp3')
  })

  it('selected+playing row shows ♪ playing + Stop + loop toggle', async () => {
    const onStop = vi.fn()
    const onToggleLoop = vi.fn()
    render(FileList, {
      props: base({ selectedFile: 'jazz-loop.mp3', playing: true, loop: true, onStop, onToggleLoop }) as never,
    })
    expect(screen.getByText('♪ playing')).toBeTruthy()
    await fireEvent.click(screen.getByText(/Stop/))
    expect(onStop).toHaveBeenCalled()
    // loop ON pill; clicking flips to off (passes !loop).
    const loopBtn = screen.getByText(/loop ON/)
    await fireEvent.click(loopBtn)
    expect(onToggleLoop).toHaveBeenCalledWith(false)
  })

  it('disabled (offline master) → no command fires', async () => {
    const onPlay = vi.fn()
    render(FileList, { props: base({ disabled: true, onPlay }) as never })
    const btn = screen.getAllByText(/Select & play/)[0]
    await fireEvent.click(btn)
    expect(onPlay).not.toHaveBeenCalled()
  })
})
