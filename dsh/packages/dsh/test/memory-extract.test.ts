/**
 * Unit tests for the memory extractor (src/memory/extract.ts): JSON parsing
 * tolerance and the streaming extract flow against a mock LLM.
 */
import { describe, expect, it } from 'vitest'
import type { StreamChunk } from '@deepseek-ai/dsh-llm'
import { extractMemories, parseMemories, type MemoryLlm } from '../src/memory/extract.ts'

describe('parseMemories', () => {
  it('parses clean JSON', () => {
    expect(parseMemories('{"memories":[{"kind":"fact","content":"喜欢篮球"}]}')).toEqual([
      { kind: 'fact', content: '喜欢篮球' },
    ])
  })

  it('parses fenced JSON', () => {
    expect(parseMemories('```json\n{"memories":[{"kind":"preference","content":"x"}]}\n```')).toEqual([
      { kind: 'preference', content: 'x' },
    ])
  })

  it('parses JSON wrapped in prose', () => {
    expect(parseMemories('here you go: {"memories":[{"kind":"decision","content":"y"}]} done')).toEqual([
      { kind: 'decision', content: 'y' },
    ])
  })

  it('returns [] for empty, garbage, invalid kinds, and empty content', () => {
    expect(parseMemories('')).toEqual([])
    expect(parseMemories('not json at all')).toEqual([])
    expect(parseMemories('{"memories":[{"kind":"bogus","content":"x"}]}')).toEqual([])
    expect(parseMemories('{"memories":[{"kind":"fact","content":""}]}')).toEqual([])
    expect(parseMemories('{"memories":"not-an-array"}')).toEqual([])
  })
})

function textDelta(text: string): StreamChunk {
  return { type: 'text-delta', index: 0, text } as StreamChunk
}

function finish(kind: 'stop' | 'error'): StreamChunk {
  return kind === 'stop'
    ? ({ type: 'finish', reason: { kind: 'stop' } } as StreamChunk)
    : ({ type: 'finish', reason: { kind: 'error', failure: { code: 'x' } } } as StreamChunk)
}

function mockLlm(chunks: StreamChunk[], onOptions?: (options: { system?: string }) => void): MemoryLlm {
  return {
    stream: async function* (options: { system?: string }) {
      onOptions?.(options)
      for (const c of chunks) yield c
    },
  } as unknown as MemoryLlm
}

describe('extractMemories', () => {
  it('streams deltas, parses, and stamps the agent scope', async () => {
    let system = ''
    const llm = mockLlm([
      textDelta('{"memories":['),
      textDelta('{"kind":"fact","content":"喜欢篮球"}]}'),
      finish('stop'),
    ], o => { system = o.system ?? '' })

    const out = await extractMemories({
      llm,
      provider: 'p',
      model: 'm',
      summary: '我喜欢打篮球',
      existing: [],
      scope: { kind: 'agent', name: 'basketball' },
    })
    expect(system).toContain('memory extractor')
    expect(out).toEqual([
      { kind: 'fact', content: '喜欢篮球', scope: { kind: 'agent', name: 'basketball' }, sourceCh: 'agent', weight: 1, origin: 'auto' },
    ])
  })

  it('stamps global scope with sourceCh alter', async () => {
    const llm = mockLlm([textDelta('{"memories":[{"kind":"summary","content":"s"}]}'), finish('stop')])
    const out = await extractMemories({ llm, provider: 'p', model: 'm', summary: 's', existing: [], scope: { kind: 'global' } })
    expect(out).toEqual([
      { kind: 'summary', content: 's', scope: { kind: 'global' }, sourceCh: 'alter', weight: 1, origin: 'auto' },
    ])
  })

  it('returns [] on a non-stop finish', async () => {
    const llm = mockLlm([textDelta('{"memories":[]}'), finish('error')])
    const out = await extractMemories({ llm, provider: 'p', model: 'm', summary: 's', existing: [], scope: { kind: 'global' } })
    expect(out).toEqual([])
  })
})
