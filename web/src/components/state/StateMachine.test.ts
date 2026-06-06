import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import StateMachine from './StateMachine.svelte'

afterEach(cleanup)

describe('StateMachine branch selection', () => {
  it('loading renders the skeleton fallback only', () => {
    const { container } = render(StateMachine, { props: { state: 'loading' } })
    expect(container.querySelector('.skeleton')).not.toBeNull()
    expect(container.querySelector('.banner')).toBeNull()
  })

  it('empty renders the empty fallback only', () => {
    render(StateMachine, { props: { state: 'empty' } })
    expect(screen.getByText('Nothing here yet.')).toBeTruthy()
  })

  it('offline renders the offline chip', () => {
    const { container } = render(StateMachine, { props: { state: 'offline' } })
    expect(container.querySelector('.offline-chip')).not.toBeNull()
  })

  it('error renders the error banner with the code/message', () => {
    render(StateMachine, {
      props: { state: 'error', error: { code: 'not_found', message: 'gone' } },
    })
    expect(screen.getByText('not_found')).toBeTruthy()
    expect(screen.getByText('gone')).toBeTruthy()
  })
})

describe('StateMachine error affordances', () => {
  it('version_conflict shows Reload & reapply; other codes do not', () => {
    const onReloadReapply = vi.fn()
    const { unmount } = render(StateMachine, {
      props: {
        state: 'error',
        error: { code: 'version_conflict', message: 'stale' },
        onReloadReapply,
      },
    })
    expect(screen.getByText('Reload & reapply')).toBeTruthy()
    unmount()

    render(StateMachine, {
      props: {
        state: 'error',
        error: { code: 'not_found', message: 'gone' },
        onReloadReapply,
      },
    })
    expect(screen.queryByText('Reload & reapply')).toBeNull()
  })

  it('Retry invokes onRetry', async () => {
    const onRetry = vi.fn()
    render(StateMachine, {
      props: { state: 'error', error: { code: 'x', message: 'y' }, onRetry },
    })
    await fireEvent.click(screen.getByText('Retry'))
    expect(onRetry).toHaveBeenCalledOnce()
  })
})
