import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import Banner from './Banner.svelte'

afterEach(cleanup)

describe('Banner', () => {
  it('shows the envelope code + message', () => {
    render(Banner, { code: 'bad_request', message: 'nope' })
    expect(screen.getByText('bad_request')).toBeTruthy()
    expect(screen.getByText('nope')).toBeTruthy()
  })

  it('Retry fires the callback', async () => {
    const onRetry = vi.fn()
    render(Banner, { code: 'unavailable', message: 'down', onRetry })
    await fireEvent.click(screen.getByText('Retry'))
    expect(onRetry).toHaveBeenCalledOnce()
  })

  it('409 version_conflict shows Reload & reapply, others do not', () => {
    const onReloadReapply = vi.fn()
    const { unmount } = render(Banner, {
      code: 'version_conflict',
      message: 'stale',
      onReloadReapply,
    })
    expect(screen.getByText('Reload & reapply')).toBeTruthy()
    unmount()
    render(Banner, { code: 'bad_request', message: 'x', onReloadReapply })
    expect(screen.queryByText('Reload & reapply')).toBeNull()
  })
})
