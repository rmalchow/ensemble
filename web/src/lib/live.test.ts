import { describe, it, expect, vi } from 'vitest'
import { connectWS } from './live'

// A fake WebSocket whose listeners we can drive manually.
class FakeSocket {
  listeners: Record<string, ((ev: unknown) => void)[]> = {}
  closed = false
  constructor(public url: string) {}
  addEventListener(type: string, fn: (ev: unknown) => void) {
    ;(this.listeners[type] ??= []).push(fn)
  }
  emit(type: string, ev?: unknown) {
    for (const fn of this.listeners[type] ?? []) fn(ev)
  }
  close() {
    this.closed = true
  }
}

describe('connectWS backoff', () => {
  it('reconnect delay sequence is 500→1000→2000→…→10000 (capped)', () => {
    const delays: number[] = []
    const sockets: FakeSocket[] = []
    let timerId = 0
    const pending: (() => void)[] = []

    const dispose = connectWS({
      url: 'wss://x/ws',
      onFrame: () => {},
      setTimeoutFn: (fn, ms) => {
        delays.push(ms)
        pending.push(fn)
        return ++timerId as unknown as ReturnType<typeof setTimeout>
      },
      clearTimeoutFn: () => {},
      socketFactory: (u) => {
        const s = new FakeSocket(u)
        sockets.push(s)
        return s as unknown as WebSocket
      },
    })

    // Drive close→reconnect 9 times; the backoff doubles, capped at 10000.
    for (let i = 0; i < 9; i++) {
      sockets[sockets.length - 1].emit('close')
      // fire the scheduled reconnect to create the next socket
      const fn = pending.shift()
      fn?.()
    }

    expect(delays.slice(0, 6)).toEqual([500, 1000, 2000, 4000, 8000, 10000])
    expect(delays.slice(6)).toEqual([10000, 10000, 10000])
    dispose()
  })

  it('open resets the backoff to 500', () => {
    const delays: number[] = []
    const sockets: FakeSocket[] = []
    const pending: (() => void)[] = []
    const dispose = connectWS({
      url: 'wss://x/ws',
      onFrame: () => {},
      setTimeoutFn: (fn, ms) => {
        delays.push(ms)
        pending.push(fn)
        return 0 as unknown as ReturnType<typeof setTimeout>
      },
      clearTimeoutFn: () => {},
      socketFactory: (u) => {
        const s = new FakeSocket(u)
        sockets.push(s)
        return s as unknown as WebSocket
      },
    })
    // close, reconnect, close → 500, 1000
    sockets[0].emit('close')
    pending.shift()?.()
    sockets[1].emit('close')
    expect(delays).toEqual([500, 1000])
    // a successful open resets backoff
    pending.shift()?.()
    sockets[2].emit('open')
    sockets[2].emit('close')
    expect(delays[delays.length - 1]).toBe(500)
    dispose()
  })

  it('disposer closes the socket and stops the timer', () => {
    const sockets: FakeSocket[] = []
    let cleared = false
    const dispose = connectWS({
      url: 'wss://x/ws',
      onFrame: () => {},
      setTimeoutFn: () => 7 as unknown as ReturnType<typeof setTimeout>,
      clearTimeoutFn: () => {
        cleared = true
      },
      socketFactory: (u) => {
        const s = new FakeSocket(u)
        sockets.push(s)
        return s as unknown as WebSocket
      },
    })
    // schedule a reconnect so there is a timer to clear
    sockets[0].emit('close')
    dispose()
    expect(cleared).toBe(true)
    // after dispose, a close must not schedule a new reconnect or reopen
    const before = sockets.length
    sockets[sockets.length - 1].emit('close')
    expect(sockets.length).toBe(before)
  })

  it('onFrame receives parsed JSON frames', () => {
    const frames: unknown[] = []
    const sockets: FakeSocket[] = []
    const dispose = connectWS({
      url: 'wss://x/ws',
      onFrame: (d) => frames.push(d),
      setTimeoutFn: () => 0 as unknown as ReturnType<typeof setTimeout>,
      clearTimeoutFn: () => {},
      socketFactory: (u) => {
        const s = new FakeSocket(u)
        sockets.push(s)
        return s as unknown as WebSocket
      },
    })
    sockets[0].emit('message', { data: JSON.stringify({ a: 1 }) })
    expect(frames).toEqual([{ a: 1 }])
    dispose()
  })
})
