/**
 * The memory-extraction SSE frame must reach the onMemory listeners: the
 * NetworkStore registers the `memory` SSE event and routes it without folding
 * into inbox state. Runs under node with document/EventSource/fetch stubbed.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

class FakeEventSource {
  static instances: FakeEventSource[] = []
  readonly url: string
  closed = false
  onerror: (() => void) | null = null
  private readonly handlers = new Map<string, Array<(event: { data: string }) => void>>()
  constructor(url: string) {
    this.url = url
    FakeEventSource.instances.push(this)
  }
  addEventListener(type: string, handler: (event: { data: string }) => void): void {
    const arr = this.handlers.get(type) ?? []
    arr.push(handler)
    this.handlers.set(type, arr)
  }
  close(): void {
    this.closed = true
  }
  emit(type: string, data: string): void {
    for (const h of this.handlers.get(type) ?? []) h({ data })
  }
}

const okJson = (body: unknown): Promise<{ ok: boolean; status: number; text(): Promise<string> }> =>
  Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve(JSON.stringify(body)) })

const EMPTY_STATE = { friends: [], pending: [], groups: [], drafts: [] }

interface StoreLike {
  subscribe(listener: () => void): () => void
  onMemory(listener: (f: { phase: 'extracting' | 'extracted'; count: number }) => void): () => void
}

describe('NetworkStore memory SSE routing', () => {
  let store: StoreLike
  let unsubscribe: (() => void) | undefined

  beforeEach(async () => {
    FakeEventSource.instances = []
    vi.stubGlobal('document', { addEventListener: () => {}, hidden: false })
    vi.stubGlobal('EventSource', FakeEventSource)
    vi.stubGlobal('fetch', vi.fn(() => okJson(EMPTY_STATE)))
    vi.resetModules()
    const mod = await import('../src/client/api.ts')
    store = mod.networkStore as unknown as StoreLike
    unsubscribe = store.subscribe(() => {})
  })

  afterEach(() => {
    unsubscribe?.()
    vi.unstubAllGlobals()
  })

  it('routes memory frames to onMemory listeners in order', () => {
    const got: Array<{ phase: string; count: number }> = []
    const off = store.onMemory(f => { got.push(f) })
    const src = FakeEventSource.instances[0]!
    src.emit('memory', JSON.stringify({ kind: 'memory', phase: 'extracting', count: 0 }))
    src.emit('memory', JSON.stringify({ kind: 'memory', phase: 'extracted', count: 3 }))
    expect(got).toEqual([
      { phase: 'extracting', count: 0 },
      { phase: 'extracted', count: 3 },
    ])
    off()
    src.emit('memory', JSON.stringify({ kind: 'memory', phase: 'extracted', count: 9 }))
    expect(got).toHaveLength(2) // unsubscribed
  })
})
