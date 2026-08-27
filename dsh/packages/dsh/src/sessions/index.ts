/**
 * soulmirror-sessions — ONE alter session ("My alter", P4). The alter
 * session's agent IS the owner's alter for every friend: the owner talks to
 * it (and only to it); every friend's mail is relayed into it; it writes to
 * any friend through `soulmirror_send_message`.
 *
 *   - no dsh workspace of its own (P5): the SoulMirror page is the alter's
 *     home, so the session sits ungrouped in dsh's session list rather than
 *     under a "SoulMirror" folder; the P2–P4 workspace(s) this plugin created
 *     (`<home>/a2a`, title "SoulMirror") are removed on start (sessions
 *     detached first, nothing deleted)
 *   - the alter session: `ctx.agents.create({ sessionId, meta: { cwd: <home>/a2a,
 *     agentPreset }, setup })` → `ctx.sessionTitle.rename(session, 'My alter')`. `setup(agentCtx)`
 *     mounts the preset AND registers the alter persona in the agent scope
 *     (see ../persona.ts): an agent-scoped `deployment:persona` section
 *     shadowing the preset's, with prompt VARIABLES (owner / friend roster /
 *     THIS TURN's friend + tier + override / protocol / pending drafts) whose
 *     providers are evaluated on every assembly — so the persona survives
 *     restart (setup runs on resume too) and compaction (the system prompt is
 *     assembled per step), and protocol / tier edits reach the next turn
 *     without a restart. The per-turn friend context is read back from the
 *     session log (../alter-state.ts `triggerOf`).
 *   - the session id is persisted at `<home>/a2a/dsh-sessions.json`
 *     (`{ alterSessionId }`); on restart we `ctx.agents.resume`. Per-friend
 *     reply tier in `<home>/a2a/dsh-friends.json`; pending drafts in
 *     `<home>/a2a/dsh-pending.json` (../drafts.ts).
 *   - MIGRATION from P3 (one session per friend): an old map `{ sessions: {
 *     fp: id } }` is kept in the file as `legacyFriendSessions` and logged;
 *     those sessions are never created again, never resumed by us and get no
 *     new mail (dsh still lists them in the sidebar until the owner deletes
 *     them). A tool call from such a session is gated like any other session
 *     (by what woke that turn).
 *   - inbound mail → ONE `user/message` with `source: { kind:'plugin',
 *     plugin:'soulmirror', form:'relay', senderSessionId:<friend name>, a2a:{
 *     id, fp, ts, auto?, type? } }` appended to the ALTER session
 *     (model-visible, known event type, survives restart; the client's keyed
 *     chat-node renderer paints it as a bubble). Reply tiers (../policy.ts
 *     `routeInbound`): `notify` → appended only (no turn); `draft` / `auto`
 *     → delivered through `agent.followup` so the alter wakes and answers
 *     (draft: its send becomes a pending draft for the owner; auto: it sends
 *     by itself, rate-limited). Mail flagged `auto` never wakes a turn (loop
 *     guard); neither does mail from a non-friend.
 *   - the owner's instruction (page composer → `POST
 *     /soulmirror/api/alter.instruct`) → `instruct(text)`: an ordinary
 *     `user/message` with `source: { kind: 'user' }` queued with
 *     `agent.followup` — the model sees the owner speaking; the tool reads
 *     the trigger back from the log and sends owner-initiated messages
 *     directly (no draft).
 *   - drafts: `queueDraft` (the tool) stores a pending draft; `decideDraft`
 *     (the page) approves (sends as the alter, `auto:false`, archives),
 *     rejects, or asks the alter to revise (an owner instruction carrying
 *     the feedback). Every decision is also written into the alter session
 *     as a plugin NOTE (`a2a.note`) so the alter knows what happened to its
 *     draft next time it runs; notes never wake a turn.
 *   - `latest()` / `history()` fold the session log into what the page needs;
 *     `on()` publishes changes (`alter`, `outbound`, `draft`) so the page
 *     updates live (the browser API forwards them as SSE frames).
 *   - `typing` is NOT logged; the network plugin's browser API streams it live.
 *   - GROUPS (wire spec §14.7): a `group_message` event wakes the alter only
 *     when the group profile allows agents (speakAgents), the per-group
 *     toggle (`<home>/dsh-groups.json`, default off) is on, the sender is
 *     someone else and the profile's wake policy passes (mention / always /
 *     never). A waking message is injected like friend mail with the group
 *     context in the relay source (`a2a.gid`), and that group's rules join
 *     the persona for the turn. Non-waking group messages are left entirely
 *     to the network layer (unread accounting); nothing is injected.
 *     Approved group drafts post via `groups.send(gid, body, {by:'alter'})`.
 *
 * No @deepseek-ai value imports (see ../index.ts header).
 */
import type { Context } from '@deepseek-ai/cordis'
import type { Agent } from '@deepseek-ai/dsh-agent'
import type { Session, SessionId } from '@deepseek-ai/dsh-session'
import type { UserMessage, MessageId } from '@deepseek-ai/dsh-llm'
// Type-only: the Context merges for ctx.agents / ctx.agentLoop / ctx.sessions /
// ctx.workspaceRegistry / ctx.sessionTitle / ctx.agentPresets.
import type {} from '@deepseek-ai/dsh-agent'
import type {} from '@deepseek-ai/dsh-agent-loop'
import type {} from '@deepseek-ai/dsh-session'
import type {} from '@deepseek-ai/dsh-workspace'
import type {} from '@deepseek-ai/dsh-session-title'
import type {} from '@deepseek-ai/dsh-agent-presets'
import { access, appendFile, copyFile, mkdir, readFile, writeFile } from 'node:fs/promises'
import { homedir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { ALTER_EVENT_TYPES, chatFromEvents, classifyUserMessage, EMPTY_CHAT, EMPTY_LATEST, latestFromEvents, triggerOf, type AlterChat, type AlterLatest } from '../alter-state.ts'
import { AgentRegistryStore, type SeatAgent } from '../agent-registry.ts'
import { DraftStore, type PendingDraft } from '../drafts.ts'
import type { A2AMessageId, A2ANoteKind, A2ASourceMeta, Fingerprint } from '../events.ts'
import { RELAY_FORM, SOULMIRROR_PLUGIN } from '../events.ts'
import { FriendSettingsStore, ProtocolFile } from '../friend-settings.ts'
import { groupProfileOf, sendGroupMessage, type GroupProfileView } from '../group-contract.ts'
import { GroupSettingsStore, type GroupSettings, type GroupSettingsPatch } from '../group-settings.ts'
import { sendAndArchive } from '../network/send.ts'
import type { ConversationEntry, Friend, InboundMessage, NetworkClient } from '../network/types.ts'
import { agentPersonaTemplate, PERSONA_NONE, PERSONA_ORDER, PERSONA_SECTION, PERSONA_TEMPLATE, PERSONA_VARIABLES } from '../persona.ts'
import { DEFAULT_AUTO_REPLY_PER_HOUR, DEFAULT_REPLY_TIER, HourlyWindow, mentionsAgent, mentionsMe, routeInbound, UNKNOWN_TRIGGER, wakeAgentForGroup, wakeForGroup, type DraftReason, type ReplyTier, type TurnTrigger } from '../policy.ts'
import type { SoulmirrorSettings } from '../settings.ts'
import { MemoryStore, type AllowScopes, type MemoryKind, type MemoryRecord, type MemoryScope } from '../memory/store.ts'
import { extractMemories, type MemoryLlm } from '../memory/extract.ts'
import type {} from '../index.ts'

export interface Config {
  /** Agent preset id to compose the alter session with. Default 'soulmirror-chat'. */
  preset?: string
  /** Put the unread total into the alter session title (` (n)`). Default true. */
  unreadInTitle?: boolean
}

export const name = 'soulmirror-sessions'
export const inject = ['soulmirror', 'soulmirrorHome', 'agents', 'agentLoop', 'sessions', 'workspaceRegistry', 'sessionPersistence']

const WORKSPACE_TITLE = '灵镜'
const PRESET_ID = 'soulmirror-chat'
const MAP_FILE = 'dsh-sessions.json'
const LOG_FILE = 'dsh-sessions.log'
/** Title of the alter session in dsh's own sidebar. */
export const ALTER_SESSION_TITLE = 'My alter · SoulMirror'
/** Debounce of the `alter` change publication (ms). */
const ALTER_DEBOUNCE_MS = 80

interface SessionMap {
  /** The single alter session (P4). */
  alterSessionId?: string
  /** Named seat agent DIRECT sessions (the owner's chat): agent name → session id. */
  agentSessions?: Record<string, string>
  /** Named seat agent GROUP work sessions: agent name → gid → session id (group wakes stay out of the direct chat). */
  agentGroupSessions?: Record<string, Record<string, string>>
  /** P3 layout: fp → session id. Read once for the migration, then kept as `legacyFriendSessions`. */
  sessions?: Record<string, string>
  legacyFriendSessions?: Record<string, string>
}

/** What the owner sees about their alter (page "My alter"). */
export interface AlterState {
  readonly sessionId: string
  readonly status: 'idle' | 'running'
  readonly latest: AlterLatest
}

/** The alter transcript the page renders. */
export interface AlterHistory {
  readonly sessionId: string | undefined
  readonly status: 'idle' | 'running'
  readonly chat: AlterChat
}

export type DraftDecision =
  | { readonly action: 'approve'; readonly body?: string }
  | { readonly action: 'reject' }
  | { readonly action: 'revise'; readonly feedback: string }

/** Live events of the sessions plugin (the browser API forwards them as SSE frames). */
export type SessionsEvent =
  | { readonly kind: 'alter'; readonly state: AlterState }
  /** A named seat agent's sessions moved (status flip / new events; sessionId = its DIRECT session when it exists; gids = groups it is currently working in); its pane refetches. */
  | { readonly kind: 'agent'; readonly name: string; readonly sessionId?: string; readonly status: 'idle' | 'running'; readonly seq: number; readonly gids?: readonly string[] }
  /** An outbound entry archived by sending (the alter's tool, or an approved draft); the page folds it like its own sends. `gid` is set for group posts (fp then carries the gid too). */
  | { readonly kind: 'outbound'; readonly fp: Fingerprint; readonly gid?: string; readonly entry: ConversationEntry }
  /** A pending draft was stored or decided. */
  | { readonly kind: 'draft'; readonly action: 'added' | 'removed'; readonly draft: PendingDraft; readonly decision?: 'approved' | 'rejected' | 'revise' }
  /** Memory extraction progress for the owner's popup: extracting → extracted(count, memories). */
  | { readonly kind: 'memory'; readonly phase: 'extracting' | 'extracted'; readonly count: number; readonly memories?: readonly { id: string; content: string }[]; readonly clue?: string }

/** Public face for tools / API / tests. */
export interface AlterSessions {
  /** The alter session id (undefined until bootstrapped). */
  sessionId(): SessionId | undefined
  /** Create (or resume) the alter session; returns its id. */
  ensure(): Promise<SessionId>
  /** Deliver one inbound message into the alter session (same path as the network subscription). */
  deliver(message: InboundMessage): Promise<void>
  /** The owner viewed a friend's conversation: reset that friend's unread count. */
  markRead(fp: Fingerprint): Promise<void>
  /** Current unread count per fingerprint (since this process started / last markRead). */
  unread(fp: Fingerprint): number
  /** The owner instructs the alter (an owner `user/message` + a woken turn). */
  instruct(text: string): Promise<{ sessionId: SessionId; messageId: string }>
  /** The alter's state; undefined before the session exists. */
  latest(): AlterState | undefined
  /** The alter transcript (last `limit` items; all when omitted). */
  history(limit?: number): AlterHistory
  /** What woke the current turn of a session (owner / inbound[fp] / inbound-auto / unknown). */
  triggerOf(sessionId: SessionId): TurnTrigger
  /** Effective reply tier of a friend (stored, else the global default). */
  tierOf(fp: Fingerprint): ReplyTier
  /** The tier stored for this friend, undefined when the global default applies. */
  tierStored(fp: Fingerprint): ReplyTier | undefined
  /** Store (or clear with undefined) the reply tier of a friend; answers the effective tier. */
  setTier(fp: Fingerprint, tier: ReplyTier | undefined): Promise<ReplyTier>
  /** Do-not-disturb flag of a friend (false by default). */
  friendMuted(fp: Fingerprint): boolean
  /** Set or clear a friend's do-not-disturb. */
  setFriendMuted(fp: Fingerprint, muted: boolean): Promise<boolean>
  /** The per-group alter toggle (dsh-groups.json `alter`); default off (quiet by default). */
  groupAlterOn(gid: string): boolean
  /** Flip the per-group alter toggle; answers the stored value. */
  setGroupAlter(gid: string, on: boolean): Promise<boolean>
  /**
   * Read / patch one group's voice settings THROUGH THIS plugin's store — the
   * single writer the routing reads. (The API routes must not keep their own
   * GroupSettingsStore instance for writes: the sessions plugin's in-memory
   * copy would go stale and a freshly enabled voice would never wake.)
   */
  groupVoices(gid: string): GroupSettings
  setGroupVoices(gid: string, patch: GroupSettingsPatch): Promise<GroupSettings>
  /** Do-not-disturb flag of a group (false by default). */
  groupMuted(gid: string): boolean
  /** Set or clear a group's do-not-disturb. */
  setGroupMuted(gid: string, muted: boolean): Promise<boolean>
  /** Named seat agents of this seat (read side of ../agent-registry.ts). */
  agents(): readonly SeatAgent[]
  /** Create or update one seat agent; its session is (re)shaped in the background. */
  setAgent(input: SeatAgent): Promise<SeatAgent>
  /** Remove one seat agent (its dsh session stays until the owner deletes it). */
  removeAgent(name: string): Promise<boolean>
  /** Which voice a session belongs to (tool provenance): the alter, a named agent, or neither. */
  voiceOf(sessionId: SessionId): { kind: 'alter' } | { kind: 'agent'; agent: SeatAgent } | undefined
  /** Per-agent session info for the API/UI. */
  agentsInfo(): readonly { name: string; sessionId?: string; status: 'idle' | 'running' }[]
  /** This seat's fingerprint (the owner's identity); '' until known. */
  ownerFp(): string
  /** The owner's own group post: wake every ENABLED seat agent it names (the owner is implicit commander). */
  ownGroupPost(gid: string, body: string): Promise<void>
  /**
   * Conversation receipt: MY agent's group post addressed these counterparts
   * (`@token` of a member's alter or agent) — their NEXT agent-authored post
   * wakes my agent even without a mention (consumed once, so a chain still
   * ends when nobody addresses anybody).
   */
  noteAwaitReply(gid: string, agentName: string, expects: readonly { readonly fp: string; readonly token: string }[]): void
  /** The owner instructs one named seat agent directly (its own chat pane). */
  instructAgent(name: string, text: string): Promise<{ sessionId: SessionId; messageId: string }>
  /** One agent's transcript — its DIRECT session, or its work session in `gid` (process items included). */
  agentHistory(name: string, limit?: number, gid?: string): AlterHistory
  /** Refresh the cached friend record (name / protocol) after the peer changed it. */
  noteFriend(friend: Friend): void
  /** Automatic-reply accounting per friend (the tools plugin records sends and reads the count). */
  readonly autoReplies: HourlyWindow
  /** Pending drafts (read side). */
  readonly drafts: Pick<DraftStore, 'list' | 'get' | 'count' | 'counts'>
  /** The tool stores a draft instead of sending. Group drafts pass the gid as `fp` AND as `gid`, with the group name as `name`. */
  queueDraft(input: { fp: Fingerprint; gid?: string; name?: string; body: string; reason: DraftReason; trigger?: TurnTrigger; sessionId?: string; agent?: string }): Promise<PendingDraft>
  /** The owner decided a draft on the page. */
  decideDraft(id: string, decision: DraftDecision): Promise<{ draft: PendingDraft; entry?: ConversationEntry }>
  /** fp → legacy (P3) friend session id, for information only. */
  legacyFriendSessions(): Readonly<Record<string, string>>
  /** Cancel (delete) extracted pre-memories by uid; answers how many were removed. */
  cancelMemory(ids: readonly string[]): Promise<number>
  /** All memories visible to one scope (the memory page). */
  memoryList(allow: AllowScopes): MemoryRecord[]
  /** Add a memory by hand (origin manual). */
  memoryAdd(input: { kind: MemoryKind; content: string; scope: MemoryScope }): MemoryRecord
  /** Edit one memory's content; answers whether it existed. */
  memoryUpdate(uid: string, content: string, scope?: MemoryScope): boolean
  /** Delete one memory by uid. */
  memoryRemove(uid: string): boolean
  /** 埋点：进群（未读多）时总结该群最近一段消息并提炼记忆（限量，避免全量 token）。 */
  memorySummarizeGroup(gid: string): void
  /** 分身/agent 通过 soulmirror_remember 工具主动记住一条记忆（origin auto）。 */
  memoryRemember(input: { kind: MemoryKind; content: string; scope: MemoryScope }): MemoryRecord
  /** Publish a live event (the tools plugin reports the entries it archived). */
  emit(event: SessionsEvent): void
  /** Subscribe to live events. */
  on(listener: (event: SessionsEvent) => void): () => void
}

declare module '@deepseek-ai/cordis' {
  interface Context {
    soulmirrorSessions: AlterSessions
  }
}

/** The slice of dsh-system-prompt's service the persona needs (typed structurally: no dsh value import, no hard type edge). */
interface SystemPromptLike {
  section(section: { name: string; order: number; text: string; complete?: boolean }): () => void
  variable(name: string, provider: (context: unknown) => string | undefined): () => void
}

function dshHome(env: NodeJS.ProcessEnv = process.env): string {
  const home = env['DSH_HOME']
  return home !== undefined && home !== '' ? home : join(homedir(), '.dsh')
}

async function exists(path: string): Promise<boolean> {
  try {
    await access(path)
    return true
  } catch {
    return false
  }
}

/** Copy presets/soulmirror-chat into $DSH_HOME/.agent-presets/ once (user root; authorable). */
async function installPreset(log: (level: 'info' | 'warn', message: string) => void, presetId: string): Promise<void> {
  const target = join(dshHome(), '.agent-presets', presetId)
  if (await exists(join(target, 'agent.cordis.yml'))) return
  // lib/sessions.js → ../presets/<id>; src/sessions/index.ts → ../../presets/<id>
  const here = dirname(fileURLToPath(import.meta.url))
  const candidates = [join(here, '..', 'presets', PRESET_ID), join(here, '..', '..', 'presets', PRESET_ID)]
  for (const source of candidates) {
    if (!(await exists(join(source, 'agent.cordis.yml')))) continue
    await mkdir(target, { recursive: true })
    await copyFile(join(source, 'agent.cordis.yml'), join(target, 'agent.cordis.yml'))
    await copyFile(join(source, 'preset.yml'), join(target, 'preset.yml'))
    log('info', `installed agent preset "${presetId}" → ${target}`)
    return
  }
  log('warn', `preset sources not found beside ${here}; the alter session will use the deployment default preset`)
}

/** Build the model-visible `user/message` for one inbound mail (exported for tests). */
export function userMessageFor(message: InboundMessage, displayName: string): UserMessage {
  // `senderSessionId` is what ui-conversation's RelayBody reads for its
  // "from …" caption; extra source fields are allowed on the wire (the source
  // map is merge-extensible), but the literal type is closed, hence the cast.
  const a2a: A2ASourceMeta = {
    id: message.id,
    fp: message.from,
    ts: message.ts,
    ...(message.auto ? { auto: true } : {}),
    ...(message.type === undefined ? {} : { type: message.type }),
  }
  const source = {
    kind: 'plugin',
    plugin: SOULMIRROR_PLUGIN,
    form: RELAY_FORM,
    senderSessionId: displayName,
    a2a,
  } as unknown as UserMessage['source']
  const attachment = message.artifactPath === undefined ? '' : `\n[attachment: ${message.artifactName ?? 'file'} at ${message.artifactPath}]`
  return {
    id: crypto.randomUUID() as MessageId,
    role: 'user',
    content: [{
      type: 'text',
      text: `[SoulMirror A2A inbound] from friend "${displayName}" (fingerprint ${message.from})${message.auto ? ' (peer auto-reply)' : ''}:\n${message.body}${attachment}`,
    }],
    source,
  }
}

/** Build the model-visible `user/message` for one group message (exported for tests): the relay source gains `a2a.gid` (+ `by` when the sender's alter wrote it). */
export function groupMessageFor(message: InboundMessage, gid: string, groupName: string): UserMessage {
  const by = (message as unknown as Record<string, unknown>)['by']
  const a2a: A2ASourceMeta = {
    id: message.id,
    fp: message.from,
    ts: message.ts,
    gid,
    ...(message.auto ? { auto: true } : {}),
    ...(typeof by === 'string' && by !== '' ? { by } : {}),
    ...(message.agent === undefined || message.agent === '' ? {} : { agent: message.agent }),
    ...(message.type === undefined ? {} : { type: message.type }),
  }
  const source = {
    kind: 'plugin',
    plugin: SOULMIRROR_PLUGIN,
    form: RELAY_FORM,
    senderSessionId: `${message.name} in ${groupName}`,
    a2a,
  } as unknown as UserMessage['source']
  return {
    id: crypto.randomUUID() as MessageId,
    role: 'user',
    content: [{
      type: 'text',
      text: `[SoulMirror A2A inbound] group "${groupName}" (gid ${gid}) from "${message.name}"${by === 'alter' ? (message.agent !== undefined && message.agent !== '' ? `'s agent "${message.agent}"` : "'s alter") : ''} (fingerprint ${message.from})${message.auto ? ' (automatic post)' : ''}:\n${message.body}`,
    }],
    source,
  }
}

/** Build the owner's instruction to the alter: an ordinary user message (exported for tests). */
export function ownerMessageFor(text: string): UserMessage {
  return {
    id: crypto.randomUUID() as MessageId,
    role: 'user',
    content: [{ type: 'text', text }],
    source: { kind: 'user' },
  }
}

/** Build the plugin's note about a draft decision (exported for tests). Never woken as a turn. */
export function noteMessageFor(note: A2ANoteKind, draft: PendingDraft, text: string): UserMessage {
  const a2a: A2ASourceMeta = { id: `note-${crypto.randomUUID()}`, fp: draft.fp, ts: Date.now(), note, draftId: draft.id }
  const source = {
    kind: 'plugin',
    plugin: SOULMIRROR_PLUGIN,
    form: RELAY_FORM,
    senderSessionId: 'SoulMirror',
    a2a,
  } as unknown as UserMessage['source']
  return {
    id: crypto.randomUUID() as MessageId,
    role: 'user',
    content: [{ type: 'text', text: `[SoulMirror note] ${text}` }],
    source,
  }
}

export function apply(ctx: Context, config: Config = {}): void {
  const client: NetworkClient = ctx.soulmirror
  const home: string = ctx.soulmirrorHome
  const a2aDir = join(home, 'a2a')
  const mirrorDir = join(dshHome(), '灵镜') // alter session cwd + its 灵镜 dsh workspace, under \
  const mapPath = join(a2aDir, MAP_FILE)
  const presetId = config.preset ?? PRESET_ID
  const unreadInTitle = config.unreadInTitle ?? true
  const friendSettings = FriendSettingsStore.at(a2aDir)
  const protocolFile = ProtocolFile.at(a2aDir)
  const draftStore = DraftStore.at(a2aDir)
  const groupSettings = GroupSettingsStore.at(home)
  const agentRegistry = AgentRegistryStore.at(a2aDir)
  const memoryStore = new MemoryStore(join(a2aDir, 'dsh-memory.db'))

  let alterId: SessionId | undefined
  let legacy: Record<string, string> = {}
  const friendsByFp = new Map<string, Friend>()
  const groupsByGid = new Map<string, { name: string; profile: GroupProfileView }>()
  const unreadByFp = new Map<string, number>()
  let ensuring: Promise<Agent> | undefined
  const agentSessionIds = new Map<string, string>() // canonical agent name → DIRECT session id
  const agentGroupSessionIds = new Map<string, Map<string, string>>() // agent name → gid → session id
  const ensuringAgents = new Map<string, Promise<Agent>>() // key: name, or `${name} ${gid}`
  const listeners = new Set<(event: SessionsEvent) => void>()
  let alterTimer: ReturnType<typeof setTimeout> | undefined
  const autoReplies = new HourlyWindow()
  let ownerName = ''
  let myFp = ''
  let disposed = false

  // `dsh web` attaches no console exporter to ctx.logger by default, so we
  // also append our own lines to <home>/a2a/dsh-sessions.log (inbound mail,
  // resume refusals, races, creation order).
  const logFile = join(a2aDir, LOG_FILE)
  const log = (level: 'info' | 'warn' | 'error', message: string): void => {
    ctx.logger[level](`soulmirror-sessions: ${message}`)
    void appendFile(logFile, `${new Date().toISOString()} ${level.toUpperCase()} ${message}\n`, 'utf8').catch(() => {})
  }

  /** Live settings of the `soulmirror` namespace (the network plugin provides them); defaults when absent. */
  const settings = (): Pick<SoulmirrorSettings, 'defaultTier' | 'autoReplyPerHour' | 'alterMode'> => {
    const live = (ctx as unknown as { get(name: string): unknown }).get('soulmirrorConfig') as { current(): SoulmirrorSettings } | undefined
    const current = live?.current()
    return { defaultTier: current?.defaultTier ?? DEFAULT_REPLY_TIER, autoReplyPerHour: current?.autoReplyPerHour ?? DEFAULT_AUTO_REPLY_PER_HOUR, alterMode: current?.alterMode === 'full' ? 'full' : 'comms' }
  }

  /**
   * Provider / model for the alter session: the deployment default the web
   * UI's own sessions start from (`ctx.agentDefaultModel`, the "Default model"
   * settings section). An agent without it cannot open a turn ("has no
   * provider/model"). Read per creation so a changed default applies to the
   * next creation; a session keeps the selection its request log names.
   */
  const defaultAgentOptions = (): { provider: string; model: string } | undefined => {
    const svc = (ctx as unknown as { get(name: string): unknown }).get('agentDefaultModel') as { currentSelection(): { provider?: string; model?: string } | undefined } | undefined
    try {
      const selection = svc?.currentSelection()
      if (selection?.provider !== undefined && selection.provider !== '' && selection.model !== undefined && selection.model !== '') {
        return { provider: selection.provider, model: selection.model }
      }
    } catch (error: unknown) {
      log('warn', `default model unavailable: ${String(error)}`)
    }
    return undefined
  }

  const persistMap = async (): Promise<void> => {
    const groupSessions: Record<string, Record<string, string>> = {}
    for (const [agentName, byGid] of agentGroupSessionIds) {
      if (byGid.size > 0) groupSessions[agentName] = Object.fromEntries(byGid)
    }
    const data: SessionMap = {
      ...(alterId === undefined ? {} : { alterSessionId: alterId }),
      ...(agentSessionIds.size === 0 ? {} : { agentSessions: Object.fromEntries(agentSessionIds) }),
      ...(Object.keys(groupSessions).length === 0 ? {} : { agentGroupSessions: groupSessions }),
      ...(Object.keys(legacy).length === 0 ? {} : { legacyFriendSessions: legacy }),
    }
    await writeFile(mapPath, JSON.stringify(data, null, 2), 'utf8')
  }

  const displayNameOf = (friend: Friend | undefined, fallback: string): string => friend?.remark ?? friend?.name ?? fallback
  const nameOf = (fp: string): string => displayNameOf(friendsByFp.get(fp), fp)

  const unreadTotal = (): number => {
    let total = 0
    for (const n of unreadByFp.values()) total += n
    return total
  }

  const renameSession = (session: Session): void => {
    const titles = ctx.get('sessionTitle')
    if (titles === undefined) return
    try {
      const unread = unreadTotal()
      titles.rename(session, unreadInTitle && unread > 0 ? `${ALTER_SESSION_TITLE} (${unread})` : ALTER_SESSION_TITLE)
    } catch (error: unknown) {
      log('warn', `rename failed: ${String(error)}`)
    }
  }

  const alterSession = (): Session | undefined => {
    if (alterId === undefined) return undefined
    return ctx.agents.get(alterId)?.session ?? ctx.sessions.get(alterId)
  }

  const tierOf = (fp: Fingerprint): ReplyTier => friendSettings.tier(fp, settings().defaultTier)

  const emit = (event: SessionsEvent): void => {
    for (const listener of listeners) {
      try {
        listener(event)
      } catch (error: unknown) {
        log('warn', `sessions listener failed: ${String(error)}`)
      }
    }
  }

  const latest = (): AlterState | undefined => {
    if (alterId === undefined) return undefined
    const agent = ctx.agents.get(alterId)
    const session = agent?.session ?? ctx.sessions.get(alterId)
    return {
      sessionId: alterId,
      status: agent?.status ?? 'idle',
      latest: session === undefined ? EMPTY_LATEST : latestFromEvents(session.events),
    }
  }

  const history = (limit?: number): AlterHistory => {
    const session = alterSession()
    const agent = alterId === undefined ? undefined : ctx.agents.get(alterId)
    const chat = session === undefined ? EMPTY_CHAT : chatFromEvents(session.events, { process: true })
    const items = limit !== undefined && limit > 0 && chat.items.length > limit ? chat.items.slice(chat.items.length - limit) : chat.items
    return { sessionId: alterId, status: agent?.status ?? 'idle', chat: { ...chat, items } }
  }

  /** Publish the alter state, debounced (bursts of session events → one frame). */
  const publishAlter = (): void => {
    if (alterTimer !== undefined) clearTimeout(alterTimer)
    alterTimer = setTimeout(() => {
      alterTimer = undefined
      if (disposed) return
      const state = latest()
      if (state !== undefined) emit({ kind: 'alter', state })
    }, ALTER_DEBOUNCE_MS)
    alterTimer.unref?.()
  }

  const roster = (): string => {
    const friends = [...friendsByFp.values()].sort((a, b) => displayNameOf(a, a.fp).localeCompare(displayNameOf(b, b.fp)))
    if (friends.length === 0) return PERSONA_NONE
    return friends.map((f) => {
      const override = (f.protocol ?? '').trim()
      return `- ${displayNameOf(f, f.fp)} · ${f.fp} · ${tierOf(f.fp)} · ${override === '' ? '(no override)' : override.replace(/\s*\n\s*/g, ' / ')}`
    }).join('\n')
  }

  const groupsRoster = (): string => {
    if (groupsByGid.size === 0) return PERSONA_NONE
    return [...groupsByGid.entries()]
      .sort(([, a], [, b]) => a.name.localeCompare(b.name))
      .map(([gid, g]) => {
        const p = g.profile
        const agents = p.speakAgents ? `agents may post (tier ${p.agentTier}, wake ${p.agentWake})` : 'agents muted'
        const mine = groupSettings.alterOn(gid)
          ? (groupSettings.modeOf(gid) === 'always' ? 'active (every message may wake me)' : 'on mention only')
          : 'off'
        return `- ${g.name} · gid ${gid} · ${agents} · my participation: ${mine}`
      }).join('\n')
  }

  const draftsLine = (): string => {
    const list = draftStore.list()
    if (list.length === 0) return 'none'
    return `${list.length}\n${list.map(d => `- ${d.id} → ${d.name} (${d.fp}): "${d.body.length > 120 ? `${d.body.slice(0, 120)}…` : d.body}" [${d.reason}]`).join('\n')}`
  }

  /**
   * Register the alter persona in the agent scope: the `deployment:persona`
   * section (shadows the preset's) + the live variables it references. The
   * per-turn friend context is read from the session log at assembly time.
   */
  const installPersona = (agentCtx: Context, sessionId: SessionId): void => {
    const sp = (agentCtx as unknown as { get(name: string): unknown }).get('systemPrompt') as SystemPromptLike | undefined
    if (sp === undefined) {
      log('warn', 'systemPrompt service unavailable; the alter runs on the preset persona only')
      return
    }
    const nonEmpty = (text: string): string => (text.trim() === '' ? PERSONA_NONE : text)
    const trigger = (): TurnTrigger => {
      const session = ctx.agents.get(sessionId)?.session ?? ctx.sessions.get(sessionId)
      return session === undefined ? UNKNOWN_TRIGGER : triggerOf(session.events)
    }
    try {
      sp.variable(PERSONA_VARIABLES.owner, () => nonEmpty(ownerName !== '' ? ownerName : 'the owner'))
      sp.variable(PERSONA_VARIABLES.friends, () => roster())
      sp.variable(PERSONA_VARIABLES.groups, () => groupsRoster())
      // Group triggers resolve through the group variables below; the friend
      // slots then read "none" (the sender is a group member, not the friend
      // whose tier and protocol these lines describe).
      sp.variable(PERSONA_VARIABLES.triggerFriend, () => {
        const t = trigger()
        return t.fp === undefined || t.kind === 'group' ? 'none' : nameOf(t.fp)
      })
      sp.variable(PERSONA_VARIABLES.triggerFriendFp, () => {
        const t = trigger()
        return t.fp === undefined || t.kind === 'group' ? 'none' : t.fp
      })
      sp.variable(PERSONA_VARIABLES.triggerTier, () => {
        const t = trigger()
        return t.fp === undefined || t.kind === 'group' ? 'none' : tierOf(t.fp as Fingerprint)
      })
      sp.variable(PERSONA_VARIABLES.triggerProtocol, () => {
        const t = trigger()
        return t.fp === undefined || t.kind === 'group' ? 'none' : nonEmpty(friendsByFp.get(t.fp)?.protocol ?? '')
      })
      sp.variable(PERSONA_VARIABLES.triggerGroup, () => {
        const t = trigger()
        return t.kind === 'group' && t.gid !== undefined ? (groupsByGid.get(t.gid)?.name ?? t.gid) : 'none'
      })
      sp.variable(PERSONA_VARIABLES.triggerGid, () => {
        const t = trigger()
        return t.kind === 'group' && t.gid !== undefined ? t.gid : 'none'
      })
      sp.variable(PERSONA_VARIABLES.triggerGroupRules, () => {
        const t = trigger()
        if (t.kind !== 'group' || t.gid === undefined) return PERSONA_NONE
        return nonEmpty(groupsByGid.get(t.gid)?.profile.rules ?? '')
      })
      sp.variable(PERSONA_VARIABLES.protocol, () => nonEmpty(protocolFile.read()))
      sp.variable(PERSONA_VARIABLES.drafts, () => draftsLine())
      sp.variable(PERSONA_VARIABLES.memory, () => {
        try {
          const t = trigger()
          const allow: AllowScopes = t.kind === 'group' && t.gid !== undefined
            ? { global: true, group: t.gid }
            : t.fp !== undefined ? { global: true, friend: t.fp } : { global: true }
          const query = t.kind === 'group' && t.gid !== undefined ? (groupsByGid.get(t.gid)?.name ?? '')
            : t.fp !== undefined ? (t.name ?? '') : ''
          const recs = memoryStore.retrieve(allow, query, 8)
          return recs.length === 0 ? PERSONA_NONE : recs.map(r => `- [${r.kind}] ${r.content}`).join('\n')
        } catch {
          return PERSONA_NONE
        }
      })
      sp.section({ name: PERSONA_SECTION, order: PERSONA_ORDER, text: PERSONA_TEMPLATE, complete: true })
    } catch (error: unknown) {
      log('warn', `persona registration failed: ${String(error)}`)
    }
  }

  /** Persona of one NAMED seat agent: its own template + the shared live variables (owner / groups roster / this-turn group). */
  const installAgentPersona = (agentCtx: Context, sessionId: SessionId, seat: SeatAgent): void => {
    const sp = (agentCtx as unknown as { get(name: string): unknown }).get('systemPrompt') as SystemPromptLike | undefined
    if (sp === undefined) {
      log('warn', `systemPrompt service unavailable; agent "${seat.name}" runs on the preset persona only`)
      return
    }
    const nonEmpty = (text: string): string => (text.trim() === '' ? PERSONA_NONE : text)
    const trigger = (): TurnTrigger => {
      const session = ctx.agents.get(sessionId)?.session ?? ctx.sessions.get(sessionId)
      return session === undefined ? UNKNOWN_TRIGGER : triggerOf(session.events)
    }
    try {
      sp.variable(PERSONA_VARIABLES.owner, () => nonEmpty(ownerName !== '' ? ownerName : 'the owner'))
      // Live: reads the registry per assembly, so a saved brief reaches the very next turn.
      sp.variable(PERSONA_VARIABLES.agentBrief, () => nonEmpty(agentRegistry.get(seat.name)?.prompt ?? ''))
      sp.variable(PERSONA_VARIABLES.groups, () => groupsRoster())
      sp.variable(PERSONA_VARIABLES.triggerGroup, () => {
        const t = trigger()
        return t.kind === 'group' && t.gid !== undefined ? (groupsByGid.get(t.gid)?.name ?? t.gid) : 'none'
      })
      sp.variable(PERSONA_VARIABLES.triggerGid, () => {
        const t = trigger()
        return t.kind === 'group' && t.gid !== undefined ? t.gid : 'none'
      })
      sp.variable(PERSONA_VARIABLES.triggerGroupRules, () => {
        const t = trigger()
        if (t.kind !== 'group' || t.gid === undefined) return PERSONA_NONE
        return nonEmpty(groupsByGid.get(t.gid)?.profile.rules ?? '')
      })
      sp.variable(PERSONA_VARIABLES.memory, () => {
        try {
          const t = trigger()
          const query = t.kind === 'group' && t.gid !== undefined ? (groupsByGid.get(t.gid)?.name ?? '') : ''
          const recs = memoryStore.retrieve({ global: true, agent: seat.name }, query, 8)
          return recs.length === 0 ? PERSONA_NONE : recs.map(r => `- [${r.kind}] ${r.content}`).join('\n')
        } catch {
          return PERSONA_NONE
        }
      })
      sp.section({ name: PERSONA_SECTION, order: PERSONA_ORDER, text: agentPersonaTemplate(seat.name, seat.cwd ?? a2aDir), complete: true })
    } catch (error: unknown) {
      log('warn', `agent persona registration failed (${seat.name}): ${String(error)}`)
    }
  }

  const composeSetup = async (sessionId: SessionId): Promise<{ agentPreset?: string; setup: (agentCtx: Context) => Promise<void> }> => {
    const presets = ctx.get('agentPresets')
    let mount: ((agentCtx: Context) => Promise<void>) | undefined
    let agentPreset: string | undefined
    if (presets !== undefined) {
      try {
        const presetForMode = config.preset ?? (settings().alterMode === 'full' ? 'standard' : presetId)
        const resolved = await presets.resolve(presetForMode)
        if (resolved.broken !== undefined) throw new Error(resolved.broken)
        agentPreset = resolved.id
        mount = async (agentCtx: Context) => {
          await presets.mount(agentCtx, resolved.id)
        }
      } catch (error: unknown) {
        log('warn', `preset "${presetId}" unavailable (${String(error)}); composing with the deployment default`)
      }
    }
    return {
      ...(agentPreset === undefined ? {} : { agentPreset }),
      setup: async (agentCtx: Context) => {
        if (mount !== undefined) await mount(agentCtx)
        installPersona(agentCtx, sessionId)
      },
    }
  }

  /**
   * Composition of one seat agent session: its preset + its persona. The
   * DEFAULT preset is 'standard' (the full coding agent) — explicitly, never
   * the deployment default: this home's deployment default is the alter's
   * tool-less soulmirror-chat, and a working agent without tools can only
   * CLAIM it did the work.
   */
  const composeAgentSetup = async (sessionId: SessionId, seat: SeatAgent): Promise<{ agentPreset?: string; setup: (agentCtx: Context) => Promise<void> }> => {
    const presets = ctx.get('agentPresets')
    let mount: ((agentCtx: Context) => Promise<void>) | undefined
    let agentPreset: string | undefined
    const wanted = seat.preset ?? 'standard'
    if (presets !== undefined) {
      try {
        const resolved = await presets.resolve(wanted)
        if (resolved.broken !== undefined) throw new Error(resolved.broken)
        agentPreset = resolved.id
        mount = async (agentCtx: Context) => {
          await presets.mount(agentCtx, resolved.id)
        }
      } catch (error: unknown) {
        log('warn', `preset "${wanted}" unavailable for agent "${seat.name}" (${String(error)}); composing with the deployment default`)
      }
    }
    return {
      ...(agentPreset === undefined ? {} : { agentPreset }),
      setup: async (agentCtx: Context) => {
        if (mount !== undefined) await mount(agentCtx)
        installAgentPersona(agentCtx, sessionId, seat)
      },
    }
  }

  const ensureAlterInner = async (): Promise<Agent> => {
    if (alterId !== undefined) {
      const live = ctx.agents.get(alterId)
      if (live !== undefined) return live
      // Cold session from a previous run: resume it (log replay).
      try {
        const composition = await composeSetup(alterId)
        const agentOptions = defaultAgentOptions()
        const handle = await ctx.agents.resume({ resumeSessionId: alterId, setup: composition.setup, ...(agentOptions === undefined ? {} : { agentOptions }) })
        log('info', `resumed the alter session ${alterId}`)
        renameSession(handle.agent.session)
        return handle.agent
      } catch (error: unknown) {
        // Another host entry path (the browser re-opening the session through
        // apiproxy while we boot) may have published the same identity first;
        // that is a reuse, not a failure (same pattern as apiproxy's create()).
        const live = ctx.agents.get(alterId)
        if (live !== undefined) {
          log('warn', `resume of ${alterId} lost a publication race; reusing the live agent. Cause: ${String(error)}`)
          return live
        }
        log('error', `resume of ${alterId} failed; creating a fresh alter session. Cause: ${String(error)}`)
        alterId = undefined
      }
    }
    await mkdir(mirrorDir, { recursive: true })
    const sessionId = `session-${crypto.randomUUID()}` as SessionId
    const composition = await composeSetup(sessionId)
    const agentOptions = defaultAgentOptions()
    if (agentOptions === undefined) log('warn', `no default model (agentDefaultModel); session ${sessionId} cannot run turns until one is selected`)
    const handle = await ctx.agents.create({
      sessionId,
      meta: { cwd: mirrorDir, ...(composition.agentPreset === undefined ? {} : { agentPreset: composition.agentPreset }) },
      setup: composition.setup,
      ...(agentOptions === undefined ? {} : { agentOptions }),
    })
    alterId = sessionId
    await persistMap()
    renameSession(handle.agent.session)
    log('info', `created the alter session ${sessionId} (preset=${composition.agentPreset ?? 'default'}, model=${agentOptions === undefined ? 'none' : `${agentOptions.provider}/${agentOptions.model}`})`)
    return handle.agent
  }

  /** Hide the alter session from every sidebar grouping surface (the SoulMirror page is its only home). */
  const hideAlterFromSidebar = async (sessionId: SessionId): Promise<void> => {
    try {
      await ctx.workspaceRegistry.archiveSession(sessionId)
      log('info', `hidden the alter session ${sessionId} from the sidebar (archived)`)
    } catch (error: unknown) {
      log('warn', `hide alter session ${sessionId} from the sidebar failed: ${String(error)}`)
    }
  }

  /** Archive every persisted alter session (cwd = DSH_HOME/灵镜) so none lingers in the sidebar's ungrouped list. */
  const archiveAlterSessions = async (): Promise<void> => {
    const persistence = ctx.get('sessionPersistence') as { list(): Promise<Array<{ id: SessionId; cwd?: string }>> } | undefined
    if (persistence === undefined) return
    const mirrorPath = mirrorDir.replace(/\\/g, '/').replace(/\/+$/, '')
    try {
      const headers = await persistence.list()
      for (const header of headers) {
        const cwd = header.cwd?.replace(/\\/g, '/').replace(/\/+$/, '')
        if (cwd !== undefined && cwd === mirrorPath) await hideAlterFromSidebar(header.id)
      }
    } catch (error: unknown) {
      log('warn', `archive alter sessions by cwd failed: ${String(error)}`)
    }
  }

  /** Serialised: two concurrent callers (startup + a notification) must not create two sessions. */
  const ensureAlter = (): Promise<Agent> => {
    if (ensuring !== undefined) return ensuring
    const p = ensureAlterInner()
      .then(async (agent) => {
        if (alterId !== undefined) await hideAlterFromSidebar(alterId)
        return agent
      })
      .finally(() => { ensuring = undefined })
    ensuring = p
    return p
  }

  const renameAgentSession = (title: string, session: Session): void => {
    const titles = ctx.get('sessionTitle')
    if (titles === undefined) return
    try {
      titles.rename(session, title)
    } catch (error: unknown) {
      log('warn', `rename failed (${title}): ${String(error)}`)
    }
  }

  /**
   * One agent runs SEVERAL sessions: a DIRECT one (the owner's chat pane) and
   * one per group it works in — group wakes never land in the direct chat.
   * `group` undefined = the direct session.
   */
  const ensureAgentSessionInner = async (seat: SeatAgent, group?: { gid: string; name: string }): Promise<Agent> => {
    const byGid = group === undefined ? undefined : agentGroupSessionIds.get(seat.name)
    const known = group === undefined ? agentSessionIds.get(seat.name) : byGid?.get(group.gid)
    const title = group === undefined ? `${seat.name} · SoulMirror` : `${seat.name} · ${group.name}`
    const label = group === undefined ? seat.name : `${seat.name} @ ${group.name}`
    const remember = (sessionId: string): void => {
      if (group === undefined) {
        agentSessionIds.set(seat.name, sessionId)
        return
      }
      let map = agentGroupSessionIds.get(seat.name)
      if (map === undefined) {
        map = new Map()
        agentGroupSessionIds.set(seat.name, map)
      }
      map.set(group.gid, sessionId)
    }
    const forget = (): void => {
      if (group === undefined) agentSessionIds.delete(seat.name)
      else agentGroupSessionIds.get(seat.name)?.delete(group.gid)
    }
    if (known !== undefined) {
      const live = ctx.agents.get(known as SessionId)
      if (live !== undefined) return live
      try {
        const composition = await composeAgentSetup(known as SessionId, seat)
        const agentOptions = defaultAgentOptions()
        const handle = await ctx.agents.resume({ resumeSessionId: known as SessionId, setup: composition.setup, ...(agentOptions === undefined ? {} : { agentOptions }) })
        log('info', `resumed agent session ${known} ("${label}")`)
        renameAgentSession(title, handle.agent.session)
        return handle.agent
      } catch (error: unknown) {
        const live = ctx.agents.get(known as SessionId)
        if (live !== undefined) {
          log('warn', `resume of ${known} ("${label}") lost a publication race; reusing the live agent. Cause: ${String(error)}`)
          return live
        }
        log('error', `resume of ${known} ("${label}") failed; creating a fresh session. Cause: ${String(error)}`)
        forget()
      }
    }
    const sessionId = `session-${crypto.randomUUID()}` as SessionId
    const composition = await composeAgentSetup(sessionId, seat)
    const agentOptions = defaultAgentOptions()
    if (agentOptions === undefined) log('warn', `no default model (agentDefaultModel); agent session ${sessionId} ("${label}") cannot run turns until one is selected`)
    const handle = await ctx.agents.create({
      sessionId,
      meta: { cwd: seat.cwd ?? a2aDir, ...(composition.agentPreset === undefined ? {} : { agentPreset: composition.agentPreset }) },
      setup: composition.setup,
      ...(agentOptions === undefined ? {} : { agentOptions }),
    })
    remember(sessionId)
    await persistMap()
    renameAgentSession(title, handle.agent.session)
    log('info', `created agent session ${sessionId} for "${label}" (preset=${composition.agentPreset ?? 'default'}, cwd=${seat.cwd ?? a2aDir})`)
    return handle.agent
  }

  /** Serialised per (agent, context), like ensureAlter. */
  const ensureAgentSession = (seat: SeatAgent, group?: { gid: string; name: string }): Promise<Agent> => {
    const key = group === undefined ? seat.name : `${seat.name} ${group.gid}`
    const running = ensuringAgents.get(key)
    if (running !== undefined) return running
    const p = ensureAgentSessionInner(seat, group).finally(() => { ensuringAgents.delete(key) })
    ensuringAgents.set(key, p)
    return p
  }

  /** The DIRECT session of one agent (the owner's chat pane). */
  const ensureAgent = (seat: SeatAgent): Promise<Agent> => ensureAgentSession(seat)

  /**
   * "Working here" signal for a group wake: the group sees a typing marker
   * for `owner · agent` until that (agent, group) session goes idle. Best
   * effort; my own view runs on the local SSE agent frames instead.
   */
  const groupWorkTyping = new Set<string>() // `${name}|${gid}` with an 'on' signal out
  /**
   * Mechanical reply check for group wakes: a completed turn in a group work
   * session that never CALLED soulmirror_send_group_message reached nobody —
   * the model sometimes claims otherwise. One corrective nudge per wake.
   */
  const groupReplyWatch = new Map<string, { gid: string; groupName: string; nudged: boolean }>() // group-session id → watch
  /**
   * Conversation receipts per group: my agent asked someone — their next
   * agent-authored post wakes it even without an @ (models forget the
   * mention; the ANSWER must still reach the asker). Consumed on match,
   * expired after AWAIT_REPLY_MS, in-memory only.
   */
  const AWAIT_REPLY_MS = 30 * 60_000
  const awaitReply = new Map<string, { myAgent: string; fp: string; token: string; ts: number }[]>() // gid → receipts
  const noteAwaitReply = (gid: string, agentName: string, expects: readonly { readonly fp: string; readonly token: string }[]): void => {
    if (expects.length === 0) return
    const list = (awaitReply.get(gid) ?? []).filter(w => Date.now() - w.ts < AWAIT_REPLY_MS)
    for (const e of expects) {
      if (!list.some(w => w.myAgent === agentName && w.fp === e.fp && w.token.toLowerCase() === e.token.toLowerCase())) {
        list.push({ myAgent: agentName, fp: e.fp, token: e.token, ts: Date.now() })
      }
    }
    awaitReply.set(gid, list)
    log('info', `agent "${agentName}" awaits a reply in ${gid} from ${expects.map(e => `${e.token}(${e.fp.slice(0, 8)})`).join(', ')}`)
  }
  /** Consume the receipts this inbound message satisfies: my agents owed a reply by this sender's voice. */
  const takeAwaitedRepliers = (gid: string, message: InboundMessage): string[] => {
    if (message.by !== 'alter') return [] // only agent/alter posts count as the counterpart's reply
    const list = (awaitReply.get(gid) ?? []).filter(w => Date.now() - w.ts < AWAIT_REPLY_MS)
    const senderTokens = [message.agent ?? '', message.name].filter(t => t !== '').map(t => t.toLowerCase())
    const hit: string[] = []
    const rest: typeof list = []
    for (const w of list) {
      if (w.fp === message.from && senderTokens.includes(w.token.toLowerCase())) hit.push(w.myAgent)
      else rest.push(w)
    }
    awaitReply.set(gid, rest)
    return [...new Set(hit)]
  }
  const signalGroupWork = (seat: SeatAgent, gid: string, on: boolean): void => {
    void client.groups.typing(gid, on, seat.name).catch(() => {})
  }
  const noteGroupWorkStart = (seat: SeatAgent, gid: string): void => {
    const key = `${seat.name}|${gid}`
    if (groupWorkTyping.has(key)) return
    groupWorkTyping.add(key)
    signalGroupWork(seat, gid, true)
  }

  const deliver = async (message: InboundMessage): Promise<void> => {
    let friend = friendsByFp.get(message.from)
    if (friend === undefined) {
      friend = (await client.friends.list()).find(f => f.fp === message.from)
      if (friend !== undefined) friendsByFp.set(friend.fp, friend)
    }
    const isFriend = friend !== undefined
    const agent = await ensureAlter()
    unreadByFp.set(message.from, (unreadByFp.get(message.from) ?? 0) + 1)
    const tier = tierOf(message.from)
    const route = routeInbound({ tier, auto: message.auto === true, isFriend })
    const userMessage = userMessageFor(message, displayNameOf(friend, message.name))
    let how: string
    if (route.action === 'wake') {
      // Through the inbox: the alter wakes, the message is logged as the turn's user/message when claimed.
      agent.followup(userMessage)
      how = `woke the alter (tier=${tier})`
    } else {
      agent.session.append('user/message', userMessage, { surfaceOp: 'append' })
      how = `appended only (${route.reason ?? 'no-wake'}; tier=${tier})`
    }
    renameSession(agent.session)
    log('info', `inbound ${message.id} from ${displayNameOf(friend, message.name)} → alter session ${agent.id}: ${how}; unread=${unreadByFp.get(message.from) ?? 0}`)
    publishAlter()
  }

  /** Refresh the cached group list (names + profiles). */
  const refreshGroups = async (): Promise<void> => {
    const list = await client.groups.list()
    groupsByGid.clear()
    for (const g of list) groupsByGid.set(g.gid, { name: g.name, profile: groupProfileOf(g) })
  }

  /** One group's cached name + profile; fetched on demand. */
  const groupOf = async (gid: string): Promise<{ name: string; profile: GroupProfileView } | undefined> => {
    const cached = groupsByGid.get(gid)
    if (cached !== undefined) return cached
    try {
      const info = await client.groups.info(gid)
      const view = { name: info.name, profile: groupProfileOf(info) }
      groupsByGid.set(gid, view)
      return view
    } catch (error: unknown) {
      log('warn', `group ${gid} unavailable: ${String(error)}`)
      return undefined
    }
  }

  /**
   * One inbound group message (wire spec §14.7): the alter wakes only when
   * the group profile allows agents, the per-group toggle is on, the sender
   * is someone else and the wake policy passes. Everything else is left to
   * the network layer's unread accounting — nothing is injected.
   */
  const deliverGroup = async (gid: string, message: InboundMessage): Promise<void> => {
    if (myFp === '') await refreshOwner()
    if (message.from === myFp) return // own fan-out echo (any of my voices; siblings are woken by the own-post hook, never by echo)
    const group = await groupOf(gid)
    if (group === undefined) return
    const senderLabel = message.agent !== undefined && message.agent !== '' ? `${message.name} · ${message.agent}` : message.name
    // The default alter: the duty slot ('alter') lifts it to answer unmentioned traffic.
    const route = wakeForGroup({
      speakAgents: group.profile.speakAgents,
      enabled: groupSettings.alterOn(gid),
      fromSelf: false,
      wake: group.profile.agentWake,
      myMode: groupSettings.modeOf(gid),
      mentioned: mentionsMe(message.body, ownerName),
    })
    if (route.wake) {
      const agent = await ensureAlter()
      agent.followup(groupMessageFor(message, gid, group.name))
      log('info', `group message ${message.id} from ${senderLabel} in ${group.name} (${gid}) woke the alter (session ${agent.id})`)
      publishAlter()
    } else {
      log('info', `group message ${message.id} in ${group.name} (${gid}) does not wake the alter (${route.reason ?? 'no-wake'})`)
    }
    // Conversation receipts: whose questions does this message answer?
    const awaitedAgents = takeAwaitedRepliers(gid, message)
    // Named seat agents: name mention (no @all) + commander whitelist + per-group voice switch; the duty slot answers unmentioned traffic.
    for (const seat of agentRegistry.list()) {
      // An awaited reply wakes the asking agent even without a mention and
      // regardless of the commander whitelist (answering is not commanding).
      if (awaitedAgents.includes(seat.name) && groupSettings.voiceOn(gid, seat.name) && group.profile.speakAgents) {
        const worker = await ensureAgentSession(seat, { gid, name: group.name })
        worker.followup(groupMessageFor(message, gid, group.name))
        log('info', `awaited reply from ${senderLabel} in ${group.name} (${gid}) woke agent "${seat.name}" (session ${worker.id})`)
        // No reply enforcement here: an awaited wake DELIVERS the answer; posting
        // a follow-up is the agent's choice (silence on a courtesy closing is right).
        noteGroupWorkStart(seat, gid)
        publishAgent(seat.name)
        continue
      }
      const agentRoute = wakeAgentForGroup({
        speakAgents: group.profile.speakAgents,
        enabled: groupSettings.voiceOn(gid, seat.name),
        fromSelf: false,
        wake: group.profile.agentWake,
        duty: groupSettings.dutyOf(gid) === seat.name,
        mentioned: mentionsAgent(message.body, seat.name),
        // Per-group whitelist (dsh-groups.json voices[name].commanders): '*' = any member; empty = owner only.
        commander: groupSettings.commandersOf(gid, seat.name).some(c => c === '*' || c === message.from),
      })
      if (!agentRoute.wake) {
        // Unmentioned traffic is the normal quiet case; a MENTIONED agent that still
        // does not wake is always worth a line (the silent skip cost a debugging round).
        if (agentRoute.reason === 'not-commander' || agentRoute.reason === 'wake-never' || (mentionsAgent(message.body, seat.name) && agentRoute.reason !== 'not-mentioned')) {
          log('info', `group message ${message.id} in ${group.name} (${gid}) does not wake agent "${seat.name}" (${agentRoute.reason ?? 'no-wake'})`)
        }
        continue
      }
      const worker = await ensureAgentSession(seat, { gid, name: group.name })
      worker.followup(groupMessageFor(message, gid, group.name))
      log('info', `group message ${message.id} from ${senderLabel} in ${group.name} (${gid}) woke agent "${seat.name}" (session ${worker.id})`)
      groupReplyWatch.set(worker.id, { gid, groupName: group.name, nudged: false })
      noteGroupWorkStart(seat, gid)
      publishAgent(seat.name)
    }
  }

  /**
   * The owner's own group post: wake every seat agent it names. The owner's
   * explicit mention IS the instruction — it does not require the per-group
   * voice switch (that switch governs passive listening to OTHERS); only the
   * group profile's speakAgents can silence the reply.
   */
  const ownGroupPost = async (gid: string, body: string): Promise<void> => {
    if (myFp === '') await refreshOwner()
    const group = await groupOf(gid)
    if (group === undefined) return
    if (!group.profile.speakAgents) {
      for (const seat of agentRegistry.list()) {
        if (mentionsAgent(body, seat.name)) log('info', `own post in ${group.name} (${gid}) mentions "${seat.name}" but agents are muted there (profile speakAgents=false)`)
      }
      return
    }
    for (const seat of agentRegistry.list()) {
      if (!mentionsAgent(body, seat.name)) continue
      const message: InboundMessage = {
        id: `own-${crypto.randomUUID()}` as A2AMessageId,
        from: myFp as Fingerprint,
        name: ownerName !== '' ? ownerName : 'the owner',
        body,
        ts: Date.now(),
      }
      const worker = await ensureAgentSession(seat, { gid, name: group.name })
      worker.followup(groupMessageFor(message, gid, group.name))
      log('info', `owner's own post in ${group.name} (${gid}) woke agent "${seat.name}" (session ${worker.id})`)
      groupReplyWatch.set(worker.id, { gid, groupName: group.name, nudged: false })
      noteGroupWorkStart(seat, gid)
      publishAgent(seat.name)
    }
  }

  /**
   * Which voice a session belongs to (tool provenance). The id a tool hands in
   * (`exec.agent.id`) may not be the literal session id, so it is normalized
   * through the agent/session registries to the session's own id first.
   */
  const voiceOf = (sessionId: SessionId): { kind: 'alter' } | { kind: 'agent'; agent: SeatAgent } | undefined => {
    const sid = (ctx.agents.get(sessionId)?.session ?? ctx.sessions.get(sessionId))?.id ?? sessionId
    if (alterId !== undefined && (sid === alterId || sessionId === alterId)) return { kind: 'alter' }
    const matchAgent = (name: string): { kind: 'agent'; agent: SeatAgent } | undefined => {
      const seat = agentRegistry.get(name)
      return seat === undefined ? undefined : { kind: 'agent', agent: seat }
    }
    for (const [name, id] of agentSessionIds) {
      if (id === sid || id === sessionId) {
        const hit = matchAgent(name)
        if (hit !== undefined) return hit
      }
    }
    for (const [name, byGid] of agentGroupSessionIds) {
      for (const id of byGid.values()) {
        if (id === sid || id === sessionId) {
          const hit = matchAgent(name)
          if (hit !== undefined) return hit
        }
      }
    }
    if (agentSessionIds.size > 0 || agentGroupSessionIds.size > 0) {
      log('warn', `voiceOf(${sessionId}): no voice matched (sid=${String(sid)}, alter=${alterId ?? '-'})`)
    }
    return undefined
  }

  /** Announce this seat's enabled agent names in one group (the other members' @-autocomplete; best effort). */
  const announceVoices = (gid: string): void => {
    const names = agentRegistry.list().filter(a => groupSettings.voiceOn(gid, a.name)).map(a => a.name)
    void client.groups.announceVoices(gid, names).catch((error: unknown) => {
      log('warn', `voices announce for ${gid} failed: ${String(error)}`)
    })
  }

  const announceAllGroups = (): void => {
    for (const gid of groupsByGid.keys()) announceVoices(gid)
  }

  const agentsInfo = (): readonly { name: string; sessionId?: string; status: 'idle' | 'running' }[] =>
    agentRegistry.list().map((seat) => {
      const sessionId = agentSessionIds.get(seat.name)
      const sessions = [sessionId, ...(agentGroupSessionIds.get(seat.name)?.values() ?? [])]
      const running = sessions.some(id => id !== undefined && ctx.agents.get(id as SessionId)?.status === 'running')
      return { name: seat.name, ...(sessionId === undefined ? {} : { sessionId }), status: running ? 'running' as const : 'idle' as const }
    })

  const setAgent = async (input: SeatAgent): Promise<SeatAgent> => {
    if (!agentRegistry.isLoaded) await agentRegistry.load()
    if (ownerName !== '' && input.name.trim().toLowerCase() === ownerName.trim().toLowerCase()) {
      throw new Error(`"${input.name}" is the owner's name`)
    }
    const stored = await agentRegistry.set(input)
    void ensureAgent(stored).catch((error: unknown) => { log('error', `agent session for "${stored.name}" failed: ${String(error)}`) })
    announceAllGroups()
    publishAlter()
    return stored
  }

  const removeAgent = async (name: string): Promise<boolean> => {
    if (!agentRegistry.isLoaded) await agentRegistry.load()
    const existed = await agentRegistry.remove(name)
    if (!existed) return false
    const key = name.trim().toLowerCase()
    for (const n of [...agentSessionIds.keys()]) {
      if (n.toLowerCase() === key) agentSessionIds.delete(n)
    }
    for (const n of [...agentGroupSessionIds.keys()]) {
      if (n.toLowerCase() === key) agentGroupSessionIds.delete(n)
    }
    await persistMap()
    announceAllGroups()
    publishAlter()
    return true
  }

  /** Publish one agent's state (status + event count), debounced per agent like publishAlter. */
  const agentTimers = new Map<string, ReturnType<typeof setTimeout>>()
  const publishAgent = (name: string): void => {
    const existing = agentTimers.get(name)
    if (existing !== undefined) clearTimeout(existing)
    const timer = setTimeout(() => {
      agentTimers.delete(name)
      if (disposed) return
      const sessionId = agentSessionIds.get(name)
      const gids: string[] = []
      for (const [gid, id] of agentGroupSessionIds.get(name) ?? []) {
        if (ctx.agents.get(id as SessionId)?.status === 'running') gids.push(gid)
      }
      const directRunning = sessionId !== undefined && ctx.agents.get(sessionId as SessionId)?.status === 'running'
      const session = sessionId === undefined ? undefined : (ctx.agents.get(sessionId as SessionId)?.session ?? ctx.sessions.get(sessionId as SessionId))
      emit({ kind: 'agent', name, ...(sessionId === undefined ? {} : { sessionId }), status: directRunning || gids.length > 0 ? 'running' : 'idle', seq: session?.events.length ?? 0, ...(gids.length === 0 ? {} : { gids }) })
    }, ALTER_DEBOUNCE_MS)
    timer.unref?.()
    agentTimers.set(name, timer)
  }

  const instructAgent = async (name: string, text: string): Promise<{ sessionId: SessionId; messageId: string }> => {
    const body = text.replace(/\s+$/, '')
    if (body === '') throw new Error('instruction must not be empty')
    const seat = agentRegistry.get(name)
    if (seat === undefined) throw new Error(`no seat agent named "${name}"`)
    if (ownerName === '') await refreshOwner()
    const worker = await ensureAgent(seat)
    const message = ownerMessageFor(body)
    worker.followup(message)
    log('info', `owner instruction → agent "${seat.name}" (session ${worker.id}, message ${message.id})`)
    publishAgent(seat.name)
    return { sessionId: worker.id, messageId: message.id }
  }

  const agentHistory = (name: string, limit?: number, gid?: string): AlterHistory => {
    const seat = agentRegistry.get(name)
    const sessionId = seat === undefined
      ? undefined
      : gid === undefined
        ? agentSessionIds.get(seat.name)
        : agentGroupSessionIds.get(seat.name)?.get(gid)
    if (sessionId === undefined) return { sessionId: undefined, status: 'idle', chat: EMPTY_CHAT }
    const agent = ctx.agents.get(sessionId as SessionId)
    const session = agent?.session ?? ctx.sessions.get(sessionId as SessionId)
    // Process on: the owner watches their agent work (thinking + tool calls).
    const chat = session === undefined ? EMPTY_CHAT : chatFromEvents(session.events, { process: true })
    const items = limit !== undefined && limit > 0 && chat.items.length > limit ? chat.items.slice(chat.items.length - limit) : chat.items
    return { sessionId: sessionId as SessionId, status: agent?.status ?? 'idle', chat: { ...chat, items } }
  }

  const instruct = async (text: string): Promise<{ sessionId: SessionId; messageId: string }> => {
    const body = text.replace(/\s+$/, '')
    if (body === '') throw new Error('instruction must not be empty')
    if (ownerName === '') await refreshOwner()
    const agent = await ensureAlter()
    const message = ownerMessageFor(body)
    agent.followup(message)
    log('info', `owner instruction → alter (session ${agent.id}, message ${message.id})`)
    publishAlter()
    return { sessionId: agent.id, messageId: message.id }
  }

  const appendNote = async (note: A2ANoteKind, draft: PendingDraft, text: string): Promise<void> => {
    try {
      const agent = await ensureAlter()
      agent.session.append('user/message', noteMessageFor(note, draft, text), { surfaceOp: 'append' })
      publishAlter()
    } catch (error: unknown) {
      log('warn', `note "${note}" for draft ${draft.id} not written: ${String(error)}`)
    }
  }

  const queueDraft: AlterSessions['queueDraft'] = async (input) => {
    const draft = await draftStore.add({
      fp: input.fp,
      ...(input.gid === undefined ? {} : { gid: input.gid }),
      name: input.name ?? (input.gid === undefined ? nameOf(input.fp) : groupsByGid.get(input.gid)?.name ?? input.gid),
      body: input.body,
      reason: input.reason,
      ...(input.trigger === undefined ? {} : { trigger: { kind: input.trigger.kind, ...(input.trigger.fp === undefined ? {} : { fp: input.trigger.fp }), ...(input.trigger.name === undefined ? {} : { name: input.trigger.name }), ...(input.trigger.messageId === undefined ? {} : { messageId: input.trigger.messageId }), ...(input.trigger.gid === undefined ? {} : { gid: input.trigger.gid }) } }),
      ...(input.sessionId === undefined ? {} : { sessionId: input.sessionId }),
      ...(input.agent === undefined ? {} : { agent: input.agent }),
    })
    log('info', `draft ${draft.id} queued → ${draft.name} (${draft.fp}); reason=${draft.reason}; pending=${draftStore.count()}`)
    emit({ kind: 'draft', action: 'added', draft })
    publishAlter()
    return draft
  }

  const decideDraft: AlterSessions['decideDraft'] = async (id, decision) => {
    const draft = draftStore.get(id)
    if (draft === undefined) throw new Error(`draft ${id} is not pending`)
    const who = draft.gid === undefined ? `${draft.name} (${draft.fp})` : `group ${draft.name} (${draft.gid})`
    switch (decision.action) {
      case 'approve': {
        const body = (decision.body ?? draft.body).replace(/\s+$/, '')
        if (body === '') throw new Error('body must not be empty')
        const edited = body !== draft.body
        if (draft.gid !== undefined) {
          // Group draft: post as the alter (wire spec §14.7); archive read-back is best effort.
          const receipt = await sendGroupMessage(client, draft.gid, body, { by: 'alter', ...(draft.agent === undefined ? {} : { agent: draft.agent }) })
          let entry: ConversationEntry | undefined
          if (receipt.seq !== undefined && receipt.seq > 0) {
            try {
              const { entries } = await client.groups.conversation(draft.gid, { since: receipt.seq - 1, limit: 1 })
              entry = entries.find(e => e.seq === receipt.seq)
            } catch {
              // best effort
            }
          }
          entry ??= { seq: receipt.seq ?? 0, dir: 'out', id: receipt.id, body, ts: Date.now(), status: receipt.status }
          await draftStore.remove(id)
          log('info', `draft ${id} approved${edited ? ' (edited)' : ''} → posted ${receipt.id} into ${who} (${receipt.status})`)
          if (draft.agent !== undefined) {
            // Conversation receipts on the APPROVAL path too (the direct-send
            // path records them in the tool): whom did this post address?
            // Their next agent-authored post wakes this agent without an @.
            try {
              const info = await client.groups.info(draft.gid)
              const expects: { fp: string; token: string }[] = []
              for (const m of info.memberList ?? []) {
                if (myFp !== '' && m.fp === myFp) continue
                for (const token of [m.name, ...(m.agents ?? [])]) {
                  if (token !== '' && mentionsAgent(body, token)) expects.push({ fp: m.fp, token })
                }
              }
              noteAwaitReply(draft.gid, draft.agent, expects)
            } catch (error: unknown) {
              log('warn', `draft ${id}: could not record conversation receipts (${String(error)})`)
            }
          }
          emit({ kind: 'outbound', fp: draft.fp as Fingerprint, gid: draft.gid, entry })
          emit({ kind: 'draft', action: 'removed', draft, decision: 'approved' })
          await appendNote('draft-approved', draft, edited
            ? `The owner edited your draft ${id} to ${who} and posted: "${body}" (your draft was: "${draft.body}").`
            : `The owner approved your draft ${id} to ${who}; it was posted as written.`)
          return { draft, entry }
        }
        const { entry, receipt } = await sendAndArchive(client, draft.fp as Fingerprint, body)
        await draftStore.remove(id)
        log('info', `draft ${id} approved${edited ? ' (edited)' : ''} → sent ${receipt.id} to ${who} (${receipt.status})`)
        emit({ kind: 'outbound', fp: draft.fp as Fingerprint, entry })
        emit({ kind: 'draft', action: 'removed', draft, decision: 'approved' })
        await appendNote('draft-approved', draft, edited
          ? `The owner edited your draft ${id} to ${who} and sent: "${body}" (your draft was: "${draft.body}").`
          : `The owner approved your draft ${id} to ${who}; it was sent as written.`)
        return { draft, entry }
      }
      case 'reject': {
        await draftStore.remove(id)
        log('info', `draft ${id} rejected (→ ${who})`)
        emit({ kind: 'draft', action: 'removed', draft, decision: 'rejected' })
        await appendNote('draft-rejected', draft, `The owner rejected your draft ${id} to ${who}; nothing was sent: "${draft.body}".`)
        return { draft }
      }
      case 'revise': {
        const feedback = decision.feedback.replace(/\s+$/, '')
        if (feedback === '') throw new Error('feedback must not be empty')
        await draftStore.remove(id)
        log('info', `draft ${id} sent back for revision (→ ${who})`)
        emit({ kind: 'draft', action: 'removed', draft, decision: 'revise' })
        await appendNote('draft-revise', draft, `The owner sent your draft ${id} to ${who} back for revision; it was discarded: "${draft.body}".`)
        await instruct(draft.gid === undefined
          ? `Revise your draft to ${draft.name} (fingerprint ${draft.fp}). Your draft was: "${draft.body}". My feedback: ${feedback}\nSend the revised message to ${draft.name} with soulmirror_send_message, then tell me what you sent.`
          : `Revise your draft to group ${draft.name} (gid ${draft.gid}). Your draft was: "${draft.body}". My feedback: ${feedback}\nPost the revised message into the group with soulmirror_send_group_message, then tell me what you sent.`)
        return { draft }
      }
      default:
        throw new Error('unknown decision')
    }
  }

  const refreshOwner = async (): Promise<void> => {
    try {
      const identity = await client.identity()
      if (identity !== undefined) {
        if (identity.name !== '') ownerName = identity.name
        myFp = identity.fp
      }
    } catch {
      // the peer may still be starting; the variable falls back to "the owner"
    }
  }

  // 进群总结的入口：声明在外层作用域供 face 引用，实际实现在下方 try 块内赋值。
  let summarizeGroupMemories: (gid: string) => void = () => {}

  const face: AlterSessions = {
    sessionId: () => alterId,
    ensure: async () => (await ensureAlter()).session.id,
    deliver,
    markRead: async (fp) => {
      if (!unreadByFp.has(fp)) return
      unreadByFp.delete(fp)
      const session = alterSession()
      if (session !== undefined) renameSession(session)
    },
    unread: fp => unreadByFp.get(fp) ?? 0,
    instruct,
    latest,
    history,
    triggerOf: (sessionId) => {
      const session = ctx.agents.get(sessionId)?.session ?? ctx.sessions.get(sessionId)
      return session === undefined ? UNKNOWN_TRIGGER : triggerOf(session.events)
    },
    tierOf,
    tierStored: fp => friendSettings.get(fp).tier,
    setTier: async (fp, tier) => {
      await friendSettings.set(fp, { tier })
      publishAlter()
      return tierOf(fp)
    },
    friendMuted: (fp) => friendSettings.get(fp).muted === true,
    setFriendMuted: async (fp, muted) => {
      await friendSettings.set(fp, { muted })
      publishAlter()
      return muted
    },
    groupAlterOn: gid => groupSettings.alterOn(gid),
    setGroupAlter: async (gid, on) => {
      await groupSettings.set(gid, { alter: on })
      publishAlter()
      return groupSettings.alterOn(gid)
    },
    groupVoices: gid => groupSettings.get(gid),
    setGroupVoices: async (gid, patch) => {
      if (!groupSettings.isLoaded) await groupSettings.load()
      const stored = await groupSettings.set(gid, patch)
      announceVoices(gid)
      publishAlter()
      return stored
    },
    groupMuted: (gid) => groupSettings.get(gid).muted === true,
    setGroupMuted: async (gid, muted) => {
      if (!groupSettings.isLoaded) await groupSettings.load()
      await groupSettings.set(gid, { muted })
      publishAlter()
      return muted
    },
    agents: () => agentRegistry.list(),
    setAgent,
    removeAgent,
    voiceOf,
    agentsInfo,
    ownerFp: () => myFp,
    ownGroupPost,
    noteAwaitReply,
    instructAgent,
    agentHistory,
    noteFriend: (friend) => {
      friendsByFp.set(friend.fp, friend)
      publishAlter()
    },
    autoReplies,
    drafts: draftStore,
    queueDraft,
    decideDraft,
    legacyFriendSessions: () => legacy,
    cancelMemory: async (ids) => {
      let removed = 0
      for (const id of ids) if (memoryStore.remove(id)) removed += 1
      return removed
    },
    memoryList: (allow) => memoryStore.list(allow),
    memoryAdd: (input) => memoryStore.add({ ...input, sourceCh: 'manual', origin: 'manual' }),
    memoryUpdate: (uid, content, scope) => {
      try { return memoryStore.update(uid, content, scope) !== undefined } catch { return false }
    },
    memoryRemove: (uid) => memoryStore.remove(uid),
    memorySummarizeGroup: (gid) => { summarizeGroupMemories(gid) },
    memoryRemember: (input) => memoryStore.add({ ...input, sourceCh: input.scope.kind === 'global' ? 'alter' : input.scope.kind === 'shared-group' ? 'group' : 'agent', origin: 'auto' }),
    emit,
    on: (listener) => {
      listeners.add(listener)
      return () => { listeners.delete(listener) }
    },
  }
  ctx.provide('soulmirrorSessions', face)

  let unsubscribe: (() => void) | undefined
  ctx.effect(() => () => {
    disposed = true
    unsubscribe?.()
    if (alterTimer !== undefined) clearTimeout(alterTimer)
    alterTimer = undefined
    for (const timer of agentTimers.values()) clearTimeout(timer)
    agentTimers.clear()
    listeners.clear()
  }, 'soulmirror-sessions: network subscription')

  // Live alter state: session events of the alter session and agent status flips.
  try {
    const agentNameOfSession = (sessionId: string): string | undefined => {
      for (const [name, id] of agentSessionIds) if (id === sessionId) return name
      for (const [name, byGid] of agentGroupSessionIds) {
        for (const id of byGid.values()) if (id === sessionId) return name
      }
      return undefined
    }
    /** Did the CURRENT (last) turn of these events call the group send tool? */
    const turnHadGroupSend = (events: readonly { type: string; data: unknown }[]): boolean => {
      let start = -1
      for (let i = events.length - 1; i >= 0; i -= 1) {
        if (events[i]!.type === 'turn/start') {
          start = i
          break
        }
      }
      if (start < 0) return true
      for (let i = start; i < events.length; i += 1) {
        const ev = events[i]!
        if (ev.type === 'tool/call' && (ev.data as Record<string, unknown>)['name'] === 'soulmirror_send_group_message') return true
      }
      return false
    }
    const enforceGroupReply = (sessionId: string, endEvent: { data: unknown }): void => {
      const watch = groupReplyWatch.get(sessionId)
      if (watch === undefined) return
      // The turn's outcome comes from the END EVENT itself (not from scanning
      // session.events for the last turn/end): delivery order vs the event
      // append is a host detail this check must not depend on.
      const reasonKind = String(((endEvent.data as Record<string, unknown>)['reason'] as Record<string, unknown> | undefined)?.['kind'] ?? '')
      if (reasonKind !== 'completed') {
        groupReplyWatch.delete(sessionId) // a failed turn is already visible as such
        return
      }
      const session = ctx.agents.get(sessionId as SessionId)?.session ?? ctx.sessions.get(sessionId as SessionId)
      if (session === undefined) {
        log('warn', `group-reply check: session ${sessionId} not found — cannot verify the turn (gid ${watch.gid})`)
        groupReplyWatch.delete(sessionId)
        return
      }
      if (turnHadGroupSend(session.events)) {
        groupReplyWatch.delete(sessionId)
        return
      }
      if (watch.nudged) {
        log('warn', `agent session ${sessionId}: turn completed without a group send AGAIN — giving up the nudge (gid ${watch.gid})`)
        groupReplyWatch.delete(sessionId)
        return
      }
      watch.nudged = true
      log('info', `agent session ${sessionId}: turn completed without a group send — nudged once (gid ${watch.gid})`)
      const nudge = ownerMessageFor(`[SoulMirror mechanical check] Your turn ENDED without calling soulmirror_send_group_message — NOTHING reached the group; whatever you wrote was only a private note. Post your report into gid ${watch.gid} now by actually CALLING soulmirror_send_group_message.`)
      // followup() APPENDS to the session log, and this runs inside the
      // turn/end publish window, where a reentrant append THROWS ("cannot
      // reenter while another append is being published") — the very bug that
      // silently disarmed this check since it shipped. Deliver strictly after
      // the window closes.
      setTimeout(() => {
        if (disposed) return
        try {
          const live = ctx.agents.get(sessionId as SessionId)
          if (live !== undefined) {
            live.followup(nudge)
            return
          }
          // The live handle can be gone by then (idle agents unregister) —
          // resume the seat's group session to deliver it.
          const name = agentNameOfSession(sessionId)
          const seat = name === undefined ? undefined : agentRegistry.get(name)
          if (seat === undefined) {
            log('warn', `agent session ${sessionId}: silent group turn but no live handle and no seat to resume — nudge dropped (gid ${watch.gid})`)
            return
          }
          void ensureAgentSession(seat, { gid: watch.gid, name: watch.groupName })
            .then((worker) => { worker.followup(nudge) })
            .catch((error: unknown) => { log('warn', `agent session ${sessionId}: nudge resume failed (${String(error)})`) })
        } catch (error: unknown) {
          log('warn', `agent session ${sessionId}: nudge delivery failed (${String(error)})`)
        }
      }, 0)
      const name = agentNameOfSession(sessionId)
      if (name !== undefined) publishAgent(name)
    }
    /** 提炼共用的执行体：喂给模型、去重入库、发进度帧。 */
    const runMemoryExtract = (scope: MemoryScope, allow: AllowScopes, summary: string, label: string): void => {
      const opts = defaultAgentOptions()
      if (opts === undefined) { log('warn', 'memory: no default model configured — skipping extraction'); return }
      const llm = (ctx as unknown as { get(name: string): unknown }).get('llm') as unknown as MemoryLlm | undefined
      if (llm === undefined) { log('warn', 'memory: llm service unavailable — skipping extraction'); return }
      if (summary.trim() === '') { log('info', 'memory: nothing to summarize for ' + label); return }
      const existing = memoryStore.list(allow).map(m => m.content)
      log('info', 'memory: extracting for ' + label + ' (' + summary.length + ' chars)')
      const clue = summary.replace(/\n/g, ' ').trim().slice(0, 24)
      emit({ kind: 'memory', phase: 'extracting', count: 0, ...(clue === '' ? {} : { clue }) })
      void extractMemories({ llm, provider: opts.provider, model: opts.model, summary, existing, scope })
        .then((memories) => {
          const saved = memories.map(m => memoryStore.add(m))
          if (saved.length > 0) log('info', 'memory: extracted ' + saved.length + ' item(s) -> ' + label)
          emit({ kind: 'memory', phase: 'extracted', count: saved.length, memories: saved.map(r => ({ id: r.uid, content: r.content })) })
        })
        .catch((error: unknown) => {
          log('warn', 'memory extract failed: ' + String(error))
          emit({ kind: 'memory', phase: 'extracted', count: 0 })
        })
    }

    /** 群消息总结的窗口上限（条数），避免几万条消息一次性喂给模型。 */
    const MEMORY_GROUP_WINDOW = 50
    /** 埋点：很久没来群（未读多）进群时，总结最近一段群消息并提炼记忆。 */
    summarizeGroupMemories = (gid: string): void => {
      setTimeout(() => {
        if (disposed) return
        void client.groups.conversation(gid, { limit: MEMORY_GROUP_WINDOW })
          .then(({ entries }) => {
            const lines: string[] = []
            for (const e of entries) {
              lines.push((e.from === undefined ? 'member' : e.from) + ': ' + e.body)
            }
            runMemoryExtract({ kind: 'shared-group', gid }, { global: true, group: gid }, lines.join('\n'), 'shared-group:' + gid)
          })
          .catch((error: unknown) => { log('warn', 'memory: group summary conversation failed for ' + gid + ': ' + String(error)) })
      }, 0)
    }

    // Agent panes also stream the THINKING (reasoning deltas); the alter pane does not.
    const AGENT_EVENT_TYPES = new Set([...ALTER_EVENT_TYPES, 'assistant/chunk', 'reasoning-chunks'])
    /** 本回合是否调用了 soulmirror_remember（决定收尾弹「有记忆」还是「没记忆」）。 */
    const turnHadRemember = (events: readonly { type: string; data: unknown }[]): boolean => {
      let start = -1
      for (let i = events.length - 1; i >= 0; i -= 1) {
        if (events[i]!.type === 'turn/start') { start = i; break }
      }
      if (start < 0) return false
      for (let i = start; i < events.length; i += 1) {
        const ev = events[i]!
        if (ev.type === 'tool/call' && (ev.data as Record<string, unknown>)['name'] === 'soulmirror_remember') return true
      }
      return false
    }
    ctx.on('session/event', (session, event) => {
      if (disposed) return
      // 埋点：owner 发话 → 弹「正在倾听/提炼」。
      if (event.type === 'user/message' && classifyUserMessage(event.data) === 'owner') {
        emit({ kind: 'memory', phase: 'extracting', count: 0 })
      }
      // 埋点：owner 回合结束且本回合没调 remember → 弹「没记忆」。
      if (event.type === 'turn/end' && triggerOf(session.events).kind === 'owner' && !turnHadRemember(session.events)) {
        emit({ kind: 'memory', phase: 'extracted', count: 0 })
      }
      if (alterId !== undefined && session.id === alterId) {
        if (ALTER_EVENT_TYPES.has(event.type)) publishAlter()
        return
      }
      if (!AGENT_EVENT_TYPES.has(event.type)) return
      const name = agentNameOfSession(session.id)
      if (name !== undefined) publishAgent(name)
      if (event.type === 'turn/end') enforceGroupReply(session.id, event)
    })
    const agentGroupOfSession = (sessionId: string): { name: string; gid: string } | undefined => {
      for (const [name, byGid] of agentGroupSessionIds) {
        for (const [gid, id] of byGid) if (id === sessionId) return { name, gid }
      }
      return undefined
    }
    ctx.on('agent/status', ({ agent }) => {
      if (disposed) return
      if (alterId !== undefined && agent.id === alterId) {
        publishAlter()
        return
      }
      const name = agentNameOfSession(agent.id)
      if (name !== undefined) publishAgent(name)
      // A group work session went idle: withdraw the group's typing marker.
      if (agent.status !== 'running') {
        const hit = agentGroupOfSession(agent.id)
        if (hit !== undefined && groupWorkTyping.delete(`${hit.name}|${hit.gid}`)) {
          const seat = agentRegistry.get(hit.name)
          if (seat !== undefined) signalGroupWork(seat, hit.gid, false)
        }
      }
    })
  } catch (error: unknown) {
    log('warn', `live alter events unavailable (${String(error)}); the page polls instead`)
  }

  const bootstrapFriends = async (): Promise<void> => {
    await refreshOwner()
    try {
      await refreshGroups()
    } catch (error: unknown) {
      log('warn', `group list unavailable (${String(error)})`)
    }
    const friends = await client.friends.list()
    for (const friend of friends) {
      if (disposed) return
      friendsByFp.set(friend.fp, friend)
      if ((friend.unread ?? 0) > 0) unreadByFp.set(friend.fp, friend.unread)
    }
    const session = alterSession()
    if (session !== undefined) renameSession(session)
    // Every (re)connect re-announces this seat's enabled agent names per group —
    // the metadata is in-memory on every peer, so this is how it converges.
    announceAllGroups()
  }

  /**
   * P5 migration: earlier versions created a dsh workspace for the alter — P2–P4
   * under `<home>/a2a`, P5 under `DSH_HOME/灵镜` — and attached the alter session
   * to it. The page is the alter's home now, so every such workspace is removed:
   * its sessions are detached first (they stay, ungrouped), then the record goes.
   * Matching is by title AND a plugin-owned path (`/a2a` or the mirror dir), so a
   * user's own folder named "灵镜" elsewhere is never touched.
   */
  const removeLegacyWorkspaces = async (): Promise<void> => {
    for (const ws of ctx.workspaceRegistry.list()) {
      const path = ws.path.replace(/\\/g, '/').replace(/\/+$/, '')
      const mirrorPath = mirrorDir.replace(/\\/g, '/').replace(/\/+$/, '')
      if (ws.title !== WORKSPACE_TITLE || (!path.endsWith('/a2a') && path !== mirrorPath)) continue
      try {
        for (const sessionId of [...ws.sessionIds]) {
          await ws.detachSession(sessionId)
          await hideAlterFromSidebar(sessionId)
        }
        await ctx.workspaceRegistry.delete(ws.id)
        log('info', `migration: removed the legacy workspace "${WORKSPACE_TITLE}" (${ws.path}); its sessions stay, hidden from the sidebar`)
      } catch (error: unknown) {
        log('warn', `migration: could not remove workspace ${ws.id} (${ws.path}): ${String(error)}`)
      }
    }
  }

  void (async () => {
    try {
      await mkdir(a2aDir, { recursive: true })
      await installPreset(log, presetId)
      await friendSettings.load()
      await draftStore.load()
      await groupSettings.load()
      await agentRegistry.load()
      try {
        const raw = JSON.parse(await readFile(mapPath, 'utf8')) as SessionMap
        if (typeof raw.alterSessionId === 'string' && raw.alterSessionId !== '') alterId = raw.alterSessionId as SessionId
        if (typeof raw.agentSessions === 'object' && raw.agentSessions !== null) {
          for (const [agentName, id] of Object.entries(raw.agentSessions)) {
            if (typeof id === 'string' && id !== '') agentSessionIds.set(agentName, id)
          }
        }
        if (typeof raw.agentGroupSessions === 'object' && raw.agentGroupSessions !== null) {
          for (const [agentName, byGid] of Object.entries(raw.agentGroupSessions)) {
            if (typeof byGid !== 'object' || byGid === null) continue
            const map = new Map<string, string>()
            for (const [gid, id] of Object.entries(byGid)) {
              if (typeof id === 'string' && id !== '') map.set(gid, id)
            }
            if (map.size > 0) agentGroupSessionIds.set(agentName, map)
          }
        }
        const old = raw.legacyFriendSessions ?? raw.sessions
        if (old !== undefined && Object.keys(old).length > 0) {
          legacy = { ...old }
          if (raw.legacyFriendSessions === undefined) {
            log('info', `migration: ${Object.keys(legacy).length} P3 friend session(s) kept as legacyFriendSessions; no new per-friend sessions are created — all mail now goes to the alter session`)
          }
        }
      } catch {
        // first run
      }
      if (draftStore.count() > 0) log('info', `${draftStore.count()} pending draft(s) loaded from ${draftStore.path}`)
      await removeLegacyWorkspaces()
      await archiveAlterSessions()
      unsubscribe = client.subscribe((event) => {
        if (disposed) return
        switch (event.kind) {
          case 'message':
            void deliver(event.message).catch((error: unknown) => { log('error', `deliver failed: ${String(error)}`) })
            return
          case 'friend_accept':
            log('info', `friend accepted: ${event.friend.name} (${event.friend.fp})`)
            friendsByFp.set(event.friend.fp, event.friend)
            publishAlter()
            return
          case 'friend_request':
            log('info', `friend request from ${event.request.name} (${event.request.fp}): ${event.request.greeting}`)
            return
          case 'group_message':
            void deliverGroup(event.gid, event.message).catch((error: unknown) => { log('error', `group deliver failed: ${String(error)}`) })
            return
          case 'group_update':
            // Joined / roster or profile changed / left: drop the cache and refetch lazily.
            groupsByGid.delete(event.gid)
            void groupOf(event.gid)
            return
          case 'status':
            if (event.status.state === 'ready') {
              // (Re)connected peer: the friend list may have changed while it was down.
              void bootstrapFriends().catch((error: unknown) => { log('error', `friend sync failed: ${String(error)}`) })
            }
            return
          default:
            return
        }
      })
      try {
        await bootstrapFriends()
      } catch (error: unknown) {
        log('warn', `initial friend list unavailable (${String(error)}); waiting for the peer`)
      }
      await ensureAlter()
      for (const seat of agentRegistry.list()) {
        try {
          await ensureAgent(seat)
        } catch (error: unknown) {
          log('error', `agent session for "${seat.name}" failed: ${String(error)}`)
        }
      }
      await persistMap()
    } catch (error: unknown) {
      log('error', `bootstrap failed: ${String(error)}`)
    }
  })()
}
