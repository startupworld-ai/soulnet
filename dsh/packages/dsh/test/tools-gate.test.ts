/**
 * P4 send gate of `soulmirror_send_message` (src/tools/index.ts): the tool is
 * registered against a minimal fake cordis Context (fake network backend, a
 * stub alter-sessions face with a real DraftStore in a temp dir) and called
 * with an `exec.agent` whose session log decides the trigger:
 *   - owner-initiated turn → sent NOW, not flagged auto, no draft;
 *   - inbound-triggered turn, auto tier, under the cap → sent now, flagged
 *     auto, counted;
 *   - inbound-triggered turn, draft tier → a PENDING DRAFT is stored, nothing
 *     sent, the result says draft-queued (no dsh approval panel involved);
 *   - over the hourly cap / loop guard / other friend / unknown trigger → draft;
 *   - no sessions face (no drafts store) → the dsh approval seam as fallback.
 */
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, describe, expect, it } from 'vitest'
import type { Context } from '@deepseek-ai/cordis'
import type { ToolDefinition, ToolRunContext } from '@deepseek-ai/dsh-tools'
import { DraftStore } from '../src/drafts.ts'
import type { Fingerprint } from '../src/events.ts'
import { createFakeNetworkClient, FAKE_FRIENDS } from '../src/network/fake.ts'
import { HourlyWindow, type ReplyTier } from '../src/policy.ts'
import type { AlterSessions, SessionsEvent } from '../src/sessions/index.ts'
import { ownerMessageFor, userMessageFor } from '../src/sessions/index.ts'
import { triggerOf } from '../src/alter-state.ts'
import { apply, decideSend } from '../src/tools/index.ts'

const BOB = FAKE_FRIENDS[1]!.fp
const ALICE = FAKE_FRIENDS[0]!.fp
const SESSION_ALTER = 'session-alter'
const SESSION_AGENT = 'session-devbot'

interface Harness {
  send(args: { fingerprint: string; body: string }, agentEvents: unknown[] | undefined, sessionId?: string): Promise<Record<string, unknown>>
  sendGroup(args: { gid: string; body: string }, agentEvents: unknown[] | undefined, sessionId?: string): Promise<Record<string, unknown>>
  receipts: { gid: string; agent: string; expects: { fp: string; token: string }[] }[]
  approvals: string[]
  emitted: SessionsEvent[]
  window: HourlyWindow
  drafts: DraftStore
  setTier(fp: string, tier: ReplyTier): void
  net: ReturnType<typeof createFakeNetworkClient>
  dir: string
}

const dirs: string[] = []
afterEach(() => { for (const d of dirs.splice(0)) rmSync(d, { recursive: true, force: true }) })

function harness(options: { approval?: 'allowed-once' | 'rejected' | 'unavailable'; perHour?: number; sessions?: boolean } = {}): Harness {
  const net = createFakeNetworkClient({ firstInboundDelayMs: -1 })
  const tools: ToolDefinition[] = []
  const approvals: string[] = []
  const emitted: SessionsEvent[] = []
  const receipts: { gid: string; agent: string; expects: { fp: string; token: string }[] }[] = []
  const window = new HourlyWindow()
  const dir = mkdtempSync(join(tmpdir(), 'soulnet-dsh-gate-'))
  dirs.push(dir)
  const drafts = DraftStore.at(dir)
  const tiers = new Map<string, ReplyTier>()
  let events: unknown[] = []
  const face: AlterSessions = {
    sessionId: () => SESSION_ALTER as never,
    ensure: () => Promise.resolve(SESSION_ALTER as never),
    deliver: () => Promise.resolve(),
    markRead: () => Promise.resolve(),
    unread: () => 0,
    instruct: () => Promise.resolve({ sessionId: SESSION_ALTER as never, messageId: 'm' }),
    latest: () => undefined,
    history: () => ({ sessionId: SESSION_ALTER, status: 'idle', chat: { items: [], running: false, seq: 0 } }),
    // Same fold the real face uses, over whatever log the test hands in for that session.
    triggerOf: (id) => (id === SESSION_ALTER || id === SESSION_AGENT ? triggerOf(events as never) : { kind: 'unknown' }),
    tierOf: fp => tiers.get(fp) ?? 'draft',
    tierStored: fp => tiers.get(fp),
    setTier: (fp, next) => { if (next === undefined) tiers.delete(fp); else tiers.set(fp, next); return Promise.resolve(tiers.get(fp) ?? 'draft') },
    groupAlterOn: () => false,
    setGroupAlter: () => Promise.resolve(false),
    groupVoices: () => ({}),
    setGroupVoices: () => Promise.resolve({}),
    agents: () => [],
    setAgent: () => Promise.reject(new Error('not in this test')),
    removeAgent: () => Promise.resolve(false),
    voiceOf: (id) => (id === SESSION_ALTER ? { kind: 'alter' } : id === SESSION_AGENT ? { kind: 'agent', agent: { name: 'DevBot' } } : undefined),
    agentsInfo: () => [],
    ownerFp: () => '',
    ownGroupPost: () => Promise.resolve(),
    noteAwaitReply: (gid, agent, expects) => { receipts.push({ gid, agent, expects: expects.map(e => ({ ...e })) }) },
    instructAgent: () => Promise.reject(new Error('not in this test')),
    agentHistory: () => ({ sessionId: undefined, status: 'idle', chat: { items: [], running: false, seq: 0 } }),
    noteFriend: () => {},
    autoReplies: window,
    drafts,
    queueDraft: async (input) => {
      const d = await drafts.add({ fp: input.fp, ...(input.gid === undefined ? {} : { gid: input.gid }), name: input.name ?? (input.fp === BOB ? 'Bob' : input.fp), body: input.body, reason: input.reason, ...(input.trigger === undefined ? {} : { trigger: input.trigger }), ...(input.sessionId === undefined ? {} : { sessionId: input.sessionId }) })
      emitted.push({ kind: 'draft', action: 'added', draft: d })
      return d
    },
    decideDraft: () => Promise.reject(new Error('not in this test')),
    legacyFriendSessions: () => ({}),
    cancelMemory: async (ids) => ids.length,
    memoryList: () => [],
    memoryAdd: (input) => ({ id: 0, uid: 'mem-1', kind: input.kind, content: input.content, scope: input.scope, sourceCh: 'manual', sourceRef: '', weight: 1, origin: 'manual', createdAt: Date.now(), lastHitAt: null, hitCount: 0 }),
    memoryUpdate: () => true,
    memoryRemove: () => true,
    memorySummarizeGroup: () => {},
    memoryRemember: (input) => ({ id: 0, uid: 'mem-2', kind: input.kind, content: input.content, scope: input.scope, sourceCh: 'alter', sourceRef: '', weight: 1, origin: 'auto', createdAt: Date.now(), lastHitAt: null, hitCount: 0 }),
    friendMuted: () => false,
    setFriendMuted: async (_fp, muted) => muted,
    groupMuted: () => false,
    setGroupMuted: async (_gid, muted) => muted,
    emit: (event) => { emitted.push(event) },
    on: () => () => {},
  }
  const approvalSeam = options.approval === undefined ? undefined : {
    request: (req: { reason: string }) => { approvals.push(req.reason); return Promise.resolve(options.approval) },
  }
  const ctx = {
    soulmirror: net,
    logger: { info: () => {}, warn: () => {}, error: () => {} },
    tools: { register: (tool: ToolDefinition) => { tools.push(tool); return () => {} } },
    get: (name: string) => name === 'approval'
      ? approvalSeam
      : name === 'soulmirrorSessions'
        ? (options.sessions === false ? undefined : face)
        : name === 'soulmirrorConfig'
          ? { current: () => ({ autoReplyPerHour: options.perHour ?? 20 }) }
          : undefined,
  }
  apply(ctx as unknown as Context)
  const tool = tools.find(t => t.name === 'soulmirror_send_message')!
  const groupTool = tools.find(t => t.name === 'soulmirror_send_group_message')!
  const run = async (t: ToolDefinition, args: Record<string, unknown>, agentEvents: unknown[] | undefined, sessionId: string): Promise<Record<string, unknown>> => {
    events = agentEvents ?? []
    const exec = {
      callId: 'call-1', rootCallId: 'call-1', token: 't', name: t.name, arguments: args, signal: new AbortController().signal,
      deferContext: () => {}, concludeTurn: () => {},
      ...(agentEvents === undefined ? {} : { agent: { id: sessionId, session: { id: sessionId, events: agentEvents } } }),
    } as unknown as ToolRunContext
    return await t.execute(args, exec) as Record<string, unknown>
  }
  return {
    net,
    approvals,
    emitted,
    window,
    drafts,
    dir,
    setTier: (fp, next) => { tiers.set(fp, next) },
    receipts,
    send: (args, agentEvents, sessionId = SESSION_ALTER) => run(tool, args, agentEvents, sessionId),
    sendGroup: (args, agentEvents, sessionId = SESSION_AGENT) => run(groupTool, args, agentEvents, sessionId),
  }
}

let seq = 0
const ev = (type: string, data: unknown): unknown => ({ type, seq: seq++, time: Date.now(), data })
const ownerTurn = (): unknown[] => [ev('turn/start', { turn: 1 }), ev('user/message', ownerMessageFor('tell Bob yes'))]
const inboundTurn = (auto?: true, fp: string = BOB, name = 'Bob'): unknown[] => [ev('turn/start', { turn: 1 }), ev('user/message', userMessageFor({ id: 'in-1' as never, from: fp as Fingerprint, name, body: 'hi?', ts: Date.now(), ...(auto ? { auto } : {}) }, name))]

describe('decideSend', () => {
  it('maps the session and trigger to a gate decision; no face → unknown trigger → draft', () => {
    const seams = { sessions: () => undefined, settings: () => ({ autoReplyPerHour: 20 }) }
    expect(decideSend(seams, SESSION_ALTER as never, BOB).decision).toEqual({ kind: 'draft', reason: 'unknown-trigger' })
  })
})

describe('soulmirror_send_message gate', () => {
  it('owner-initiated turn: sends now, not auto, emits the outbound entry, no draft', async () => {
    const h = harness({ approval: 'rejected' }) // an answerer that would refuse — it must not even be asked
    const result = await h.send({ fingerprint: BOB, body: 'yes, count me in' }, ownerTurn())
    expect(result).toMatchObject({ ok: true, outcome: 'sent', gate: 'owner-initiated', auto: false, status: 'sent' })
    expect(h.approvals).toEqual([])
    expect(h.window.count(BOB)).toBe(0)
    expect(h.drafts.count()).toBe(0)
    expect(h.emitted).toHaveLength(1)
    expect(h.emitted[0]).toMatchObject({ kind: 'outbound', fp: BOB, entry: { dir: 'out', body: 'yes, count me in' } })
    const { entries } = await h.net.conversation(BOB as Fingerprint)
    expect(entries.at(-1)).toMatchObject({ dir: 'out', body: 'yes, count me in' })
    expect(entries.at(-1)?.auto).toBeUndefined()
  })

  it('owner-initiated turn may write to ANY friend (the alter serves all of them)', async () => {
    const h = harness()
    const result = await h.send({ fingerprint: ALICE, body: 'hi Alice' }, ownerTurn())
    expect(result).toMatchObject({ ok: true, outcome: 'sent', gate: 'owner-initiated' })
    expect(h.drafts.count()).toBe(0)
  })

  it('inbound turn in the auto tier: sends now, flagged auto, counted per friend; over the cap → draft', async () => {
    const h = harness({ approval: 'rejected', perHour: 2 })
    h.setTier(BOB, 'auto')
    const first = await h.send({ fingerprint: BOB, body: 'auto 1' }, inboundTurn())
    expect(first).toMatchObject({ ok: true, outcome: 'sent', gate: 'auto-tier', auto: true })
    const second = await h.send({ fingerprint: BOB, body: 'auto 2' }, inboundTurn())
    expect(second).toMatchObject({ ok: true, outcome: 'sent', gate: 'auto-tier', auto: true })
    expect(h.approvals).toEqual([])
    expect(h.window.count(BOB)).toBe(2)
    const { entries } = await h.net.conversation(BOB as Fingerprint)
    expect(entries.filter(e => e.dir === 'out' && e.auto === true)).toHaveLength(2)
    // third: over the cap → a pending draft, nothing sent
    const third = await h.send({ fingerprint: BOB, body: 'auto 3' }, inboundTurn())
    expect(third).toMatchObject({ ok: true, outcome: 'draft-queued', gate: 'rate-limited', auto: false })
    expect(h.approvals).toEqual([])
    expect(h.drafts.list(BOB)).toHaveLength(1)
    expect(h.drafts.list(BOB)[0]).toMatchObject({ fp: BOB, body: 'auto 3', reason: 'rate-limited', trigger: { kind: 'inbound', fp: BOB } })
    expect((await h.net.conversation(BOB as Fingerprint)).entries.filter(e => e.dir === 'out')).toHaveLength(2)
  })

  it('inbound turn in the draft tier: stores a pending draft, sends nothing, never asks the approval seam', async () => {
    const h = harness({ approval: 'allowed-once' }) // even a willing answerer is not used
    const result = await h.send({ fingerprint: BOB, body: 'draft reply' }, inboundTurn())
    expect(result).toMatchObject({ ok: true, outcome: 'draft-queued', gate: 'draft-tier', auto: false })
    expect(typeof result['draftId']).toBe('string')
    expect(String(result['message'])).toContain('Nothing was sent')
    expect(h.approvals).toEqual([])
    expect((await h.net.conversation(BOB as Fingerprint)).entries).toHaveLength(0)
    const stored = h.drafts.get(String(result['draftId']))
    expect(stored).toMatchObject({ fp: BOB, name: 'Bob', body: 'draft reply', reason: 'draft-tier', sessionId: SESSION_ALTER })
    expect(h.emitted.find(e => e.kind === 'draft')).toMatchObject({ kind: 'draft', action: 'added', draft: { body: 'draft reply' } })
    // The store is on disk: a fresh instance reads it back.
    const reloaded = DraftStore.at(h.dir)
    await reloaded.load()
    expect(reloaded.count(BOB)).toBe(1)
  })

  it('notify tier, loop guard, other friend and unknown trigger all become drafts with their reason', async () => {
    const h = harness({ approval: 'rejected' })
    h.setTier(BOB, 'notify')
    expect(await h.send({ fingerprint: BOB, body: 'n' }, inboundTurn())).toMatchObject({ outcome: 'draft-queued', gate: 'notify-tier' })
    h.setTier(BOB, 'auto')
    expect(await h.send({ fingerprint: BOB, body: 'echo' }, inboundTurn(true))).toMatchObject({ outcome: 'draft-queued', gate: 'loop-guard-auto' })
    h.setTier(ALICE, 'auto')
    expect(await h.send({ fingerprint: ALICE, body: 'to alice' }, inboundTurn())).toMatchObject({ outcome: 'draft-queued', gate: 'other-friend' })
    expect(await h.send({ fingerprint: BOB, body: 'x' }, ownerTurn(), 'session-unrelated')).toMatchObject({ outcome: 'draft-queued', gate: 'unknown-trigger' })
    expect(await h.send({ fingerprint: BOB, body: 'x' }, undefined)).toMatchObject({ outcome: 'draft-queued', gate: 'unknown-trigger' })
    expect(h.approvals).toEqual([])
    expect(h.drafts.count()).toBe(5)
    expect((await h.net.conversation(BOB as Fingerprint)).entries.filter(e => e.dir === 'out')).toHaveLength(0)
  })

  it('without a sessions face (no drafts store) the dsh approval seam is the fallback, failing closed', async () => {
    const allowed = harness({ approval: 'allowed-once', sessions: false })
    expect(await allowed.send({ fingerprint: BOB, body: 'x' }, ownerTurn())).toMatchObject({ ok: true, outcome: 'sent', approval: 'allowed-once', gate: 'unknown-trigger' })
    expect(allowed.approvals).toHaveLength(1)
    const refused = harness({ approval: 'rejected', sessions: false })
    expect(await refused.send({ fingerprint: BOB, body: 'x' }, ownerTurn())).toMatchObject({ ok: false, outcome: 'rejected', gate: 'unknown-trigger' })
    const nobody = harness({ sessions: false })
    expect(await nobody.send({ fingerprint: BOB, body: 'x' }, ownerTurn())).toMatchObject({ ok: false, outcome: 'unavailable' })
  })
})

describe('conversation receipts (group tool)', () => {
  it('records whom an agent-authored post addressed (owner-instructed direct send)', async () => {
    const h = harness()
    const info = await h.net.groups.create('receipts', [BOB], { speakHumans: true, speakAgents: true })
    const result = await h.sendGroup({ gid: info.gid, body: '@Bob 你今天忙不忙？' }, ownerTurn())
    expect(result['outcome']).toBe('sent')
    expect(h.receipts).toHaveLength(1)
    expect(h.receipts[0]!.gid).toBe(info.gid)
    expect(h.receipts[0]!.agent).toBe('DevBot')
    expect(h.receipts[0]!.expects).toEqual([{ fp: BOB, token: 'Bob' }])
  })

  it('records no expectation when the post addresses nobody', async () => {
    const h = harness()
    const info = await h.net.groups.create('receipts-2', [BOB], { speakHumans: true, speakAgents: true })
    const result = await h.sendGroup({ gid: info.gid, body: 'progress update, no mention' }, ownerTurn())
    expect(result['outcome']).toBe('sent')
    expect(h.receipts).toHaveLength(1)
    expect(h.receipts[0]!.expects).toEqual([])
  })

  it('does not record receipts for the alter voice', async () => {
    const h = harness()
    const info = await h.net.groups.create('receipts-3', [BOB], { speakHumans: true, speakAgents: true })
    const result = await h.sendGroup({ gid: info.gid, body: '@Bob hello from the alter' }, ownerTurn(), SESSION_ALTER)
    expect(result['outcome']).toBe('sent')
    expect(h.receipts).toHaveLength(0)
  })
})
