/**
 * 记忆提炼：一次轻量 LLM 调用（不是完整 agent），输入最近对话摘要 + 已有记忆
 * 标题，输出「新的、有价值的」记忆列表（结构化 JSON），或空数组。
 *
 * P1 用 ctx.llm 直接流式调用；提炼的「子智能体」语义留到 P2 再换成 ctx.subagents
 * （届时把本模块的 llm 参数换成 subagent runtime 即可，落库/弹窗不变）。
 */
import type { StreamChunk } from '@deepseek-ai/dsh-llm'
import { createUserMessage } from '@deepseek-ai/dsh-llm'
import type { MemoryKind, NewMemory, MemoryScope } from './store.ts'

/** ctx.llm 的流式调用形状（最小面，便于测试和后续替换成 subagent）。 */
export interface MemoryLlm {
  stream(options: {
    provider: string
    model: string
    messages: ReturnType<typeof createUserMessage>[]
    system?: string
    signal?: AbortSignal
    temperature?: number
    maxTokens?: number
  }): AsyncIterable<StreamChunk>
}

export interface ExtractRequest {
  llm: MemoryLlm
  provider: string
  model: string
  /** 最近一轮对话摘要（提炼的输入）。 */
  summary: string
  /** 已有记忆的正文（供去重参考；传给模型让它避免重复）。 */
  existing: string[]
  /** 提炼出的记忆默认挂到哪个 scope。 */
  scope: MemoryScope
  signal?: AbortSignal
}

const SYSTEM_PROMPT = [
  'You are a memory extractor. From the given conversation summary, extract ONLY new, reusable, long-term memories about the owner — facts, preferences, decisions, promises, and summaries.',
  'Rules:',
  '- Ignore transient chatter, greetings, and anything already covered by the EXISTING memories.',
  '- Write each memory as one concrete, self-contained sentence in the language of the summary (Chinese or English).',
  '- Do not invent; only state what the conversation actually said.',
  '- If nothing new is worth keeping, return an empty list.',
  '- Answer with ONLY a JSON object of the shape {"memories":[{"kind":"fact|preference|decision|promise|summary","content":"..."}]}, no prose, no markdown fences.',
].join('\n')

interface RawMemory {
  kind?: unknown
  content?: unknown
}

export function parseMemories(text: string): Array<{ kind: MemoryKind; content: string }> {
  const cleaned = text.trim().replace(/^```(?:json)?/i, '').replace(/```$/i, '').trim()
  let parsed: unknown
  try {
    parsed = JSON.parse(cleaned)
  } catch {
    // 模型偶发在 JSON 前后带说明文字：截取第一个 { 到最后一个 }
    const start = cleaned.indexOf('{')
    const end = cleaned.lastIndexOf('}')
    if (start >= 0 && end > start) {
      try { parsed = JSON.parse(cleaned.slice(start, end + 1)) } catch { return [] }
    } else {
      return []
    }
  }
  const list = (parsed as { memories?: unknown } | null)?.memories
  if (!Array.isArray(list)) return []
  const kinds: MemoryKind[] = ['fact', 'preference', 'summary', 'decision', 'promise']
  const out: Array<{ kind: MemoryKind; content: string }> = []
  for (const item of list) {
    const raw = item as RawMemory
    const kind = raw.kind
    const content = raw.content
    if (typeof content !== 'string' || content.trim() === '') continue
    if (typeof kind !== 'string' || !kinds.includes(kind as MemoryKind)) continue
    out.push({ kind: kind as MemoryKind, content: content.trim() })
  }
  return out
}

export async function extractMemories(req: ExtractRequest): Promise<NewMemory[]> {
  const existingBlock = req.existing.length === 0
    ? '(none)'
    : req.existing.map((c, i) => `${i + 1}. ${c}`).join('\n')

  const prompt = [
    'CONVERSATION SUMMARY:',
    req.summary.trim() === '' ? '(empty)' : req.summary.trim(),
    '',
    'EXISTING MEMORIES (do not repeat these):',
    existingBlock,
  ].join('\n')

  const chunks = req.llm.stream({
    provider: req.provider,
    model: req.model,
    messages: [createUserMessage({ content: [{ type: 'text', text: prompt }], source: { kind: 'user' } })],
    system: SYSTEM_PROMPT,
    ...(req.signal === undefined ? {} : { signal: req.signal }),
    temperature: 0.2,
    maxTokens: 1200,
  })

  let text = ''
  for await (const chunk of chunks) {
    if (chunk.type === 'text-delta') text += chunk.text
    if (chunk.type === 'finish' && chunk.reason.kind !== 'stop' && chunk.reason.kind !== 'max-tokens') {
      // aborted / error：直接放弃本轮提炼
      return []
    }
  }

  const memories = parseMemories(text)
  return memories.map(m => ({
    kind: m.kind,
    content: m.content,
    scope: req.scope,
    sourceCh: req.scope.kind === 'global' ? 'alter' : 'agent',
    weight: 1.0,
    origin: 'auto' as const,
  }))
}
