/**
 * The `soulmirror_remember` tool, registered in the AGENT scope of the alter
 * and seat-agent sessions (never globally) so a regular dsh workspace session
 * never sees the function_call and never fires the memory-extraction popup.
 * Memory is a SoulMirror-conversation feature only.
 */
import type { ToolDefinition } from '@deepseek-ai/dsh-tools'
import type { AlterSessions } from '../sessions/index.ts'
import type { MemoryKind, MemoryScope } from '../memory/store.ts'
import { defineTool } from './define.ts'

/** Cordis logger slice the tool needs (kept structural to avoid a value import). */
export interface RememberToolLogger {
  info(message: string): void
}

export function createRememberTool(face: AlterSessions, logger: RememberToolLogger): ToolDefinition {
  return defineTool({
    name: 'soulmirror_remember',
    description: 'Save one long-term memory the user just told you — a fact about them, a preference, a decision, or a promise. Call this whenever the user says something worth keeping for later (in the language they used). Do NOT claim you remembered something without actually calling this tool.',
    parameters: {
      content: { type: 'string', description: 'One concrete, self-contained sentence (in the user\'s language) capturing what to remember.' },
      kind: { type: 'string', description: 'fact | preference | decision | promise | summary (default fact).', optional: true },
    },
    output: { type: 'object' },
    async execute(args, exec) {
      if (exec.agent === undefined) {
        return { ok: false, message: 'memory store is unavailable in this session.' }
      }
      const content = args.content.trim()
      if (content === '') return { ok: false, message: 'content must not be empty.' }
      const voice = face.voiceOf(exec.agent.id)
      const trigger = face.triggerOf(exec.agent.id)
      // 归属：alter → global；seat agent 群会话 → shared-group:<gid>；seat agent 直聊 → agent:<name>。
      let scope: MemoryScope
      if (voice?.kind === 'agent') {
        scope = trigger.kind === 'group' && trigger.gid !== undefined
          ? { kind: 'shared-group', gid: trigger.gid }
          : { kind: 'agent', name: voice.agent.name }
      } else {
        scope = { kind: 'global' }
      }
      const kinds: MemoryKind[] = ['fact', 'preference', 'decision', 'promise', 'summary']
      const kind: MemoryKind = kinds.includes(args.kind as MemoryKind) ? args.kind as MemoryKind : 'fact'
      const record = face.memoryRemember({ kind, content, scope })
      face.emit({ kind: 'memory', phase: 'extracted', count: 1, memories: [{ id: record.uid, content: record.content }] })
      logger.info(`soulmirror-tools: remembered -> ${scope.kind} : ${content}`)
      return { ok: true, id: record.uid, scope: scope.kind, content: record.content }
    },
  })
}
