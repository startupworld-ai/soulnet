/**
 * Browser side of the host HTTP API (see ../api/index.ts): typed fetch
 * helpers, a Server-Sent-Events subscription, and one small shared store
 * (React `useSyncExternalStore` shape) holding the last known network state.
 * Same-origin requests only; dsh web serves the bundle and `/soulmirror/api/`
 * from the same loopback server.
 */
import { applyFrame, clearGroupApps as clearGroupAppsFold, EMPTY_INBOX, fromApiState, markReadGroupLocal, markReadLocal, type InboxState, type MailNotice } from './inbox-state.ts'

export const API_BASE = '/soulmirror/api/'

export interface ApiIdentity { fp: string; name: string; cardUri: string; createdAt?: string }
export type ReplyTier = 'notify' | 'draft' | 'auto'
export interface ApiFriend {
  fp: string; name: string; remark?: string; cardName?: string; online?: boolean
  unread: number; count: number; lastTs?: number; lastBody?: string; typing?: boolean
  /** Do-not-disturb: suppress the unread badge / new-mail toast. */
  muted?: boolean
  /** Per-friend diplomacy protocol override (friends.yaml `protocol`). */
  protocol?: string
  /** Effective reply tier (P3); `tierExplicit` when stored for this friend rather than the global default. */
  tier?: ReplyTier; tierExplicit?: true
  /** Pending drafts of the alter to this friend (P4). */
  drafts?: number
}
/** One stored memory (the memory page). */
export interface ApiMemory {
  id: number
  uid: string
  kind: string
  content: string
  scope: { kind: string; name?: string; fp?: string; gid?: string }
  sourceCh: string
  /** 'auto' = extracted; 'manual' = added by hand. */
  origin: 'auto' | 'manual'
  weight: number
  createdAt: number
  hitCount: number
}
/** What woke a turn of the alter session. */
export interface ApiTrigger { kind: 'owner' | 'inbound' | 'inbound-auto' | 'unknown'; fp?: string; name?: string; messageId?: string }
export type ApiSendOutcome = 'sent' | 'draft-queued' | 'refused' | 'failed'
/** The alter's latest state (host `session.latest`, SSE `alter`; see src/alter-state.ts). */
export interface ApiAlterLatest {
  instruction?: { text: string; ts: number; seq: number }
  note?: { text: string; ts: number; seq: number; turn: number }
  sent?: { body: string; fingerprint?: string; ts: number; outcome?: ApiSendOutcome; gate?: string; draftId?: string; detail?: string }
  turn?: { turn: number; reason: string; failed: boolean; ts: number; message?: string; open: boolean }
  trigger: ApiTrigger
  seq: number
}
export interface ApiAlterState {
  sessionId: string
  status: 'idle' | 'running'
  latest: ApiAlterLatest
}
/** One item of the "My alter" transcript (host `session.history`; see src/alter-state.ts `chatFromEvents`). */
export type ApiChatItem =
  | { kind: 'owner'; key: string; ts: number; text: string; revise?: { name: string; fp: string } }
  | { kind: 'alter'; key: string; ts: number; text: string; turn: number }
  | { kind: 'inbound'; key: string; ts: number; fp: string; name: string; id: string; body: string; auto: boolean; type?: string; gid?: string }
  | { kind: 'send'; key: string; ts: number; fp: string; body: string; outcome?: ApiSendOutcome; gate?: string; draftId?: string; detail?: string; auto: boolean }
  | { kind: 'note'; key: string; ts: number; note: 'draft-approved' | 'draft-rejected' | 'draft-revise'; fp: string; text: string; draftId?: string }
  | { kind: 'turn-failed'; key: string; ts: number; turn: number; reason: string; message?: string }
  | { kind: 'thinking'; key: string; ts: number; text: string; streaming?: boolean }
  | { kind: 'tool'; key: string; ts: number; name: string; args: string }
export interface ApiChat { items: ApiChatItem[]; running: boolean; seq: number }
export interface ApiHistory { sessionId: string | null; status: 'idle' | 'running'; chat: ApiChat }
/** A pending draft of the alter (P4; src/drafts.ts). */
export interface ApiDraft {
  id: string; fp: string; name: string; body: string; createdAt: string; reason: string
  trigger?: { kind: string; fp?: string; name?: string; messageId?: string }
  sessionId?: string
  /** Seat agent that drafted it; absent = the default alter. */
  agent?: string
  /** Set when the draft targets a group (fp then carries the gid too). */
  gid?: string
}
/** Alter-wide settings as the host resolves them (live). */
export interface ApiAlterConfig {
  sessionId: string | null; status: 'idle' | 'running'
  defaultTier: ReplyTier; autoReplyPerHour: number; directSend: boolean; protocolPath: string; protocolExists: boolean
  legacyFriendSessions: Record<string, string>
}
export interface ApiPending { id: string; fp: string; name: string; greeting: string; createdAt?: string }
/** Governance profile of one group (wire spec §14.7), camelCase as the host serves it. */
export interface ApiGroupProfile {
  template?: string
  /** Room module rendering this group; absent/"" = the built-in chat room. */
  room?: string
  speakHumans: boolean
  speakAgents: boolean
  speakWho?: 'all' | 'owner' | 'admins'
  join?: 'invite' | 'apply' | 'open'
  agentWake?: 'mention' | 'always' | 'never'
  agentTier?: 'notify' | 'draft' | 'auto'
  autoPerHour?: number
  agentRounds?: number
  admins?: string[]
  public?: boolean
  tags?: string[]
  rules?: string
}
/** One pinned message on the group home. */
export interface ApiGroupPin { id: string; from: string; ts: number; body: string }
/** One pending join application (owner only). */
export interface ApiGroupApplication { fp: string; name: string; note: string; ts?: number }
/** One group row (sender-key fan-out group; host `state.groups`, `group.get`). */
export interface ApiGroup {
  gid: string; name: string; ownerFp: string
  /** I am the owner (can kick; cannot leave). */
  mine: boolean
  version: number; members: number
  unread: number; count: number; lastTs?: number; lastBody?: string
  /** Do-not-disturb: suppress the unread badge / new-mail toast. */
  muted?: boolean
  /** Governance profile; absent on legacy groups. */
  profile?: ApiGroupProfile
}
export interface ApiGroupInfo extends ApiGroup {
  memberList: { fp: string; name: string; agents?: string[] }[]
  pins: ApiGroupPin[]
  myRole: 'owner' | 'admin' | 'member'
  /** Present for the owner only. */
  applications?: ApiGroupApplication[]
}
/** One archived conversation entry (`conversation.get`, `message.send`, SSE `outbound`). */
export interface ApiEntry {
  seq: number; dir: 'in' | 'out'; id: string; body: string; ts: number
  /** Sender fingerprint (group threads: who spoke). */
  from?: string
  /** Group provenance: the sender's human owner typed it, or their alter composed it. */
  by?: 'owner' | 'alter'
  /** Which of the sender's seat agents composed a by=alter post (e.g. "DevBot"); absent = their default alter. */
  agent?: string
  type?: string; auto?: true; status?: string; artifactName?: string
}
/** One named seat agent of this seat (host `state.agents` / `agents.list`). */
export interface ApiSeatAgent {
  name: string
  sessionId?: string
  status: 'idle' | 'running'
  cwd?: string
  preset?: string
  /** The owner's brief (free-text persona / standing instructions in its system prompt; live). */
  prompt?: string
  /** Its group replies wait for my review (draft flow); absent/false = it answers directly. */
  approval?: boolean
}
/** Per-group voice settings (`group.settings`): which of MY voices participate, each voice's per-group commanders, and the duty slot. */
export interface ApiGroupVoices {
  voices?: Record<string, { on: true; commanders?: string[] }>
  duty?: string
}
export interface ApiStatus {
  backend: 'fake' | 'soulnet'; state: 'starting' | 'ready' | 'restarting' | 'stopped' | 'error'
  pid?: number; restarts: number; lastError?: string; relay?: string; home?: string; protocol?: string; version?: string
  binary?: string; binarySource?: 'setting' | 'platform-package' | 'path' | 'plugin-bin' | string
}
export interface ApiState {
  backend: 'fake' | 'soulnet'
  status: ApiStatus
  home: string
  settingsNamespace: string
  identity: ApiIdentity | null
  friends: ApiFriend[]
  pending: ApiPending[]
  groups?: ApiGroup[]
  drafts: ApiDraft[]
  /** Named seat agents of this seat (registry + session status). */
  agents?: ApiSeatAgent[]
  alter?: ApiAlterConfig
  error?: string
}

export class ApiError extends Error {
  override readonly name = 'ApiError'
  constructor(message: string, readonly code: number, readonly httpStatus: number) {
    super(message)
  }
}

/** Browser-side settle guarantee: no API call may hang past this (a request lost to a
 * server restart would otherwise leave UI loading states frozen forever). */
const CALL_TIMEOUT_MS = 30_000

async function call<T>(route: string, body?: Record<string, unknown>): Promise<T> {
  const ctl = new AbortController()
  const timer = setTimeout(() => { ctl.abort() }, CALL_TIMEOUT_MS)
  let response: Response
  try {
    response = await fetch(`${API_BASE}${route}`, body === undefined && route === 'state'
      ? { method: 'GET', headers: { accept: 'application/json' }, signal: ctl.signal }
      : { method: 'POST', headers: { 'content-type': 'application/json', accept: 'application/json' }, body: JSON.stringify(body ?? {}), signal: ctl.signal })
  } catch (e: unknown) {
    // A human sentence instead of the browser's "signal is aborted without reason".
    if (e instanceof DOMException && e.name === 'AbortError') {
      throw new ApiError(`${route}: no answer within ${CALL_TIMEOUT_MS / 1000}s (server busy or restarting)`, -32603, 0)
    }
    throw e
  } finally {
    clearTimeout(timer)
  }
  const text = await response.text()
  let parsed: unknown = {}
  try {
    parsed = text === '' ? {} : JSON.parse(text)
  } catch {
    throw new ApiError(`bad response from ${route}: ${text.slice(0, 120)}`, -32603, response.status)
  }
  const err = (parsed as { error?: { code?: number; message?: string } }).error
  if (!response.ok || err !== undefined) {
    throw new ApiError(err?.message ?? `${route} failed (${response.status})`, err?.code ?? -32603, response.status)
  }
  return parsed as T
}

export const api = {
  state: () => call<ApiState>('state'),
  createIdentity: (name: string) => call<{ identity: ApiIdentity }>('identity.create', { name }),
  parseCard: (uri: string) => call<{ fp: string; name: string; uri: string }>('card.parse', { uri }),
  addFriend: (cardUri: string, note?: string) => call<{ friend: ApiFriend }>('friends.add', { card_uri: cardUri, ...(note === undefined ? {} : { note }) }),
  accept: (id: string, note?: string) => call<{ friend: ApiFriend }>('friends.accept', { id, ...(note === undefined ? {} : { note }) }),
  reject: (id: string) => call<{ ok: true }>('friends.reject', { id }),
  markRead: (fp: string, seq?: number) => call<{ ok: true }>('conversation.markRead', { fp, ...(seq === undefined ? {} : { seq }) }),
  /** Archive of one friend: `since` = only seq > since, `limit` = the last N entries. */
  conversation: (fp: string, opts: { since?: number; limit?: number } = {}) =>
    call<{ entries: ApiEntry[]; typing: boolean }>('conversation.get', { fp, ...(opts.since === undefined ? {} : { since: opts.since }), ...(opts.limit === undefined ? {} : { limit: opts.limit }) }),
  /** Debug: direct send to a friend through the peer (bypasses the alter); answers the archived entry. */
  send: (fp: string, body: string) => call<{ entry: ApiEntry; receipt: { id: string; seq?: number; status: string } }>('message.send', { fp, body }),
  typing: (fp: string, on: boolean) => call<{ ok: true }>('message.typing', { fp, on }),
  presence: (fps: readonly string[]) => call<{ online: Record<string, boolean> }>('presence', { fps: [...fps] }),
  /** The owner instructs their alter (an owner user/message + a woken turn in the alter session). */
  instruct: (text: string) => call<{ sessionId: string; messageId: string; state: ApiAlterState | null }>('alter.instruct', { text }),
  /** Cancel (delete) extracted pre-memories by uid. */
  cancelMemory: (ids: readonly string[]) => call<{ ok: true; removed: number }>('memory.cancel', { ids: [...ids] }),
  memoryList: (allow: { global?: boolean; agent?: string; friend?: string; group?: string }) =>
    call<{ memories: ApiMemory[] }>('memory.list', { ...allow }),
  memoryAdd: (input: { kind: string; content: string; scope: { kind: string; name?: string; fp?: string; gid?: string } }) =>
    call<{ memory: ApiMemory }>('memory.add', { ...input }),
  memoryUpdate: (uid: string, content: string, scope?: { kind: string; name?: string; fp?: string; gid?: string }) =>
    call<{ ok: boolean }>('memory.update', { uid, content, ...(scope === undefined ? {} : { scope }) }),
  memoryRemove: (uid: string) => call<{ ok: boolean }>('memory.remove', { uid }),
  /** 埋点：进群（未读多）时总结该群最近一段消息并提炼记忆。 */
  memorySummarize: (gid: string) => call<{ ok: boolean }>('memory.summarize', { gid }),
  /** The alter's latest state (null = no session yet). */
  sessionLatest: () => call<{ state: ApiAlterState | null }>('session.latest', {}),
  /** The alter transcript (last `limit` items). */
  sessionHistory: (limit?: number) => call<ApiHistory>('session.history', limit === undefined ? {} : { limit }),
  /** The owner instructs one seat agent directly (its own chat pane). */
  agentInstruct: (name: string, text: string) => call<{ sessionId: string; messageId: string }>('agent.instruct', { name, text }),
  /** One seat agent's transcript — its direct session, or its work session in `gid` (process items included). */
  agentHistory: (name: string, limit?: number, gid?: string) => call<ApiHistory>('agent.history', { name, ...(limit === undefined ? {} : { limit }), ...(gid === undefined ? {} : { gid }) }),
  /** Pending drafts (all, or one friend's). */
  drafts: (fp?: string) => call<{ drafts: ApiDraft[] }>('drafts.list', fp === undefined ? {} : { fp }),
  /** Decide a draft: approve (optionally edited), reject, or send it back to the alter with feedback. */
  decideDraft: (id: string, decision: { action: 'approve'; body?: string } | { action: 'reject' } | { action: 'revise'; feedback: string }) =>
    call<{ ok: true; draft: ApiDraft; entry?: ApiEntry }>('drafts.decide', { id, ...decision }),
  /** Note / protocol override (peer) and reply tier (plugin) of a friend; `tier: ''` = back to the default. */
  friendSet: (fp: string, patch: { note?: string; protocol?: string; tier?: ReplyTier | ''; muted?: boolean }) => call<{ friend: ApiFriend }>('friends.set', { fp, ...patch }),
  friendCard: (fp: string) => call<{ fp: string; name: string; uri: string }>('friends.card', { fp }),
  protocolGet: () => call<{ text: string; path: string; exists: boolean }>('protocol.get', {}),
  protocolSet: (text: string) => call<{ ok: true; text: string; path: string }>('protocol.set', { text }),
  /** Create a group with me as owner plus the given friend fingerprints; `profile` = the governance layer (built from a template). */
  groupCreate: (name: string, members: readonly string[], profile?: ApiGroupProfile) =>
    call<{ group: ApiGroupInfo }>('group.create', { name, members: [...members], ...(profile === undefined ? {} : { profile }) }),
  groupInfo: (gid: string) => call<{ group: ApiGroupInfo }>('group.get', { gid }),
  /** Send into the group; `by` = provenance (`owner` from the composer, `alter` from the sessions plugin). */
  groupSend: (gid: string, body: string, by?: 'owner' | 'alter') =>
    call<{ receipt: { id: string; seq?: number; status: string }; entry: ApiEntry | null }>('group.send', { gid, body, ...(by === undefined ? {} : { by }) }),
  groupConversation: (gid: string, opts: { since?: number; limit?: number } = {}) =>
    call<{ entries: ApiEntry[] }>('group.conversation', { gid, ...(opts.since === undefined ? {} : { since: opts.since }), ...(opts.limit === undefined ? {} : { limit: opts.limit }) }),
  groupMarkRead: (gid: string, seq?: number) => call<{ ok: true }>('group.markRead', { gid, ...(seq === undefined ? {} : { seq }) }),
  groupLeave: (gid: string) => call<{ ok: true }>('group.leave', { gid }),
  groupKick: (gid: string, fp: string) => call<{ ok: true }>('group.kick', { gid, fp }),
  /** Replace the group's governance profile (owner). */
  groupSetProfile: (gid: string, profile: ApiGroupProfile) => call<{ ok: true }>('group.setProfile', { gid, profile }),
  groupPin: (gid: string, body: string) => call<{ ok: true }>('group.pin', { gid, body }),
  groupUnpin: (gid: string, id: string) => call<{ ok: true }>('group.unpin', { gid, id }),
  /** Apply to join a group from its public URI (`soulmirror://group?gid=…&relay=…`). */
  groupApply: (uri: string, note?: string) => call<{ ok: true; gid: string }>('group.apply', { uri, ...(note === undefined ? {} : { note }) }),
  groupApplications: (gid: string) => call<{ applications: ApiGroupApplication[] }>('group.applications', { gid }),
  groupApprove: (gid: string, fp: string) => call<{ ok: true }>('group.approve', { gid, fp }),
  groupApplicationReject: (gid: string, fp: string) => call<{ ok: true }>('group.applicationReject', { gid, fp }),
  groupInvite: (gid: string, fp: string) => call<{ ok: true }>('group.invite', { gid, fp }),
  /** Per-group client settings (v2 voices): which of my voices participate here + the duty slot. */
  groupSettingsGet: (gid: string) => call<{ settings: ApiGroupVoices }>('group.settings', { gid }),
  /** Patch: legacy {alter, mode}, one voice switch (commanders replaces the group whitelist when given), or {duty: name | null}. */
  groupSettingsSet: (gid: string, patch: { alter?: boolean; mode?: 'mention' | 'always'; voice?: { name: string; on: boolean; commanders?: string[] }; duty?: string | null; muted?: boolean }) =>
    call<{ ok: true; settings: ApiGroupVoices }>('group.settings', { gid, ...patch }),
  /** Named seat agents: session info + full registry records. */
  agentsList: () => call<{ agents: ApiSeatAgent[]; registry: { name: string; preset?: string; cwd?: string; approval?: boolean }[] }>('agents.list', {}),
  /** Create or update one seat agent's DEFINITION (who may command it is per group: `groupSettingsSet` voice commanders). */
  agentsSet: (agent: { name: string; preset?: string; cwd?: string; prompt?: string; approval?: boolean }) =>
    call<{ ok: true; agent: { name: string; preset?: string; cwd?: string; prompt?: string; approval?: boolean } }>('agents.set', agent),
  agentsRemove: (name: string) => call<{ ok: true; removed: boolean }>('agents.remove', { name }),
}

/** Memory-extraction frame payload (shared by the SSE frame and onMemory listeners). */
export interface MemoryFrame {
  phase: 'extracting' | 'extracted'
  count: number
  memories?: Array<{ id: string; content: string }>
  clue?: string
}

export type NetworkEventFrame =
  | { kind: 'message'; message: { id: string; from: string; name: string; body: string; ts: number; seq?: number; type?: string; auto?: true; artifactName?: string } }
  /** An outbound entry was archived (the alter sent, a draft was approved, or the debug direct send): `fp` is the friend. */
  | { kind: 'outbound'; fp: string; entry: ApiEntry }
  | { kind: 'typing'; fp: string; on: boolean }
  | { kind: 'friend_request'; request: ApiPending }
  | { kind: 'friend_accept'; friend: ApiFriend }
  | { kind: 'presence'; fp: string; online: boolean }
  | { kind: 'status'; status: ApiStatus }
  /** The alter's state changed (a turn, a note, a send …). */
  | { kind: 'alter'; state: ApiAlterState }
  /** A named seat agent's sessions moved (status / new events; `gids` = groups it is working in right now); its pane refetches. */
  | { kind: 'agent'; name: string; sessionId?: string; status: 'idle' | 'running'; seq?: number; gids?: string[] }
  /** A member's seat is working in the group (`agent` = which of their agents, absent = their alter). */
  | { kind: 'group_typing'; gid: string; fp: string; agent?: string; on: boolean }
  /** A pending draft was stored or decided. */
  | { kind: 'draft'; action: 'added' | 'removed'; draft: ApiDraft; decision?: 'approved' | 'rejected' | 'revise' }
  /** A group message arrived (message.from = the member who spoke; `by` = human or alter; `agent` = their named seat agent). */
  | { kind: 'group_message'; gid: string; message: { id: string; from: string; name: string; body: string; ts: number; seq?: number; auto?: true; by?: 'owner' | 'alter'; agent?: string } }
  /** Joined / roster changed / left one group — the list refetches. */
  | { kind: 'group_update'; gid: string }
  /** An outbound group entry was archived (my own direct send, possibly in another tab). */
  | { kind: 'group_outbound'; gid: string; entry: ApiEntry }
  /** A stranger applied to join one of my groups (owner side). */
  | { kind: 'group_application'; gid: string; request: { fp: string; name: string; note: string } }
  /** Memory extraction progress: the owner's popup shows extracting → extracted(count, memories). */
  | ({ kind: 'memory' } & MemoryFrame)

export interface NetworkStoreSnapshot {
  readonly state: ApiState | undefined
  readonly loading: boolean
  readonly error: string | undefined
  /** fp → typing flag (live, from SSE). */
  readonly typing: Readonly<Record<string, boolean>>
  readonly status: ApiStatus | undefined
  /** Bumps on every SSE frame so lists can refetch. */
  readonly revision: number
  /**
   * Friends / pending / drafts as folded from the last `/state` answer plus
   * the SSE frames since (optimistic; replaced by the next refetch). The
   * sidebar badge, the page's middle column and the toast read this, not
   * `state` directly.
   */
  readonly inbox: InboxState
}

/**
 * One store per browser page: `refresh()` pulls `/state`, `connect()` keeps the
 * SSE stream open (typing, friend events, backend status) while someone
 * subscribes. Components read it through `useSyncExternalStore`.
 */
/**
 * A hidden tab gives its SSE slot back after this long. The browser pools six
 * HTTP/1.1 connections per origin across ALL tabs; every tab left open on this
 * page holds one stream, so a handful of stale tabs starves a freshly opened
 * one — its every request (dsh's own boot calls included) queues behind the
 * streams until the 30 s abort. Closing the stream in hidden tabs keeps the
 * pool free; the tab reconnects and refreshes the moment it is visible again.
 */
const HIDDEN_DISCONNECT_MS = 20_000

export class NetworkStore {
  private snapshot: NetworkStoreSnapshot = { state: undefined, loading: false, error: undefined, typing: {}, status: undefined, revision: 0, inbox: EMPTY_INBOX }
  private readonly listeners = new Set<() => void>()
  private readonly mailListeners = new Set<(notice: MailNotice) => void>()
  private readonly memoryListeners = new Set<(frame: MemoryFrame) => void>()
  private readonly frameListeners = new Set<(frame: NetworkEventFrame) => void>()
  private source: EventSource | undefined
  private refreshTimer: ReturnType<typeof setTimeout> | undefined
  private hiddenTimer: ReturnType<typeof setTimeout> | undefined
  private inflight: Promise<void> | undefined

  constructor() {
    if (typeof document !== 'undefined') {
      document.addEventListener('visibilitychange', () => { this.onVisibility() })
    }
  }

  /** Hidden → release the SSE slot (after a grace so quick tab flips don't churn); visible → take it back and catch up. */
  private onVisibility(): void {
    if (document.hidden) {
      if (this.source === undefined || this.hiddenTimer !== undefined) return
      this.hiddenTimer = setTimeout(() => {
        this.hiddenTimer = undefined
        if (document.hidden && this.source !== undefined) {
          this.source.close()
          this.source = undefined
        }
      }, HIDDEN_DISCONNECT_MS)
      return
    }
    if (this.hiddenTimer !== undefined) {
      clearTimeout(this.hiddenTimer)
      this.hiddenTimer = undefined
    }
    if (this.listeners.size > 0 && this.source === undefined) {
      this.connect()
      // Frames were missed while disconnected: same catch-up as an EventSource reconnect.
      this.scheduleRefresh()
    }
  }

  getSnapshot = (): NetworkStoreSnapshot => this.snapshot

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener)
    if (this.listeners.size === 1) this.connect()
    if (this.snapshot.state === undefined && !this.snapshot.loading) void this.refresh()
    return () => {
      this.listeners.delete(listener)
      if (this.listeners.size === 0) this.disconnect()
    }
  }

  /**
   * Subscribe to new-mail notices (one per inbound `message` frame). The
   * caller decides whether to show a cue (e.g. only when that thread is not
   * on screen). Independent of the snapshot subscription; does not open the
   * SSE stream by itself.
   */
  onMail = (listener: (notice: MailNotice) => void): (() => void) => {
    this.mailListeners.add(listener)
    return () => { this.mailListeners.delete(listener) }
  }

  /** Subscribe to every SSE frame as it arrives (the page's stores fold `message` / `outbound` / `alter` / `draft`). */
  onFrame = (listener: (frame: NetworkEventFrame) => void): (() => void) => {
    this.frameListeners.add(listener)
    return () => { this.frameListeners.delete(listener) }
  }

  /** Subscribe to memory-extraction frames (the owner's popup). Does not open the SSE stream by itself. */
  onMemory = (listener: (frame: MemoryFrame) => void): (() => void) => {
    this.memoryListeners.add(listener)
    return () => { this.memoryListeners.delete(listener) }
  }

  /** Fold a `presence` answer (fp -> online) into the friend rows. */
  applyPresence = (online: Readonly<Record<string, boolean>>): void => {
    let inbox = this.snapshot.inbox
    for (const [fp, on] of Object.entries(online)) inbox = applyFrame(inbox, { kind: 'presence', fp, online: on }).state
    if (inbox !== this.snapshot.inbox) this.set({ inbox })
  }

  /** Fold an outbound entry we just archived ourselves (preview / time of the friend row). */
  applyOutbound = (fp: string, entry: ApiEntry): void => {
    this.set({ inbox: applyFrame(this.snapshot.inbox, { kind: 'outbound', fp, entry }).state })
  }

  private set(patch: Partial<NetworkStoreSnapshot>): void {
    this.snapshot = { ...this.snapshot, ...patch }
    for (const l of this.listeners) l()
  }

  /** The owner read a friend's conversation: zero its unread locally and tell the host. */
  markRead = (fp: string): Promise<void> => {
    this.set({ inbox: markReadLocal(this.snapshot.inbox, fp) })
    return api.markRead(fp).then(() => {}).catch(() => {})
  }

  /** The owner read a group's conversation: zero its unread locally and tell the host. */
  markReadGroup = (gid: string): Promise<void> => {
    this.set({ inbox: markReadGroupLocal(this.snapshot.inbox, gid) })
    return api.groupMarkRead(gid).then(() => {}).catch(() => {})
  }

  /** The owner opened a group's home: drop its unseen application badge (the authoritative list is `group.get`). */
  clearGroupApps = (gid: string): void => {
    this.set({ inbox: clearGroupAppsFold(this.snapshot.inbox, gid) })
  }

  refresh = (): Promise<void> => {
    if (this.inflight !== undefined) return this.inflight
    this.set({ loading: true })
    this.inflight = api.state().then((state) => {
      this.set({ state, loading: false, error: undefined, status: state.status, inbox: fromApiState(this.snapshot.inbox, state) })
    }).catch((error: unknown) => {
      this.set({ loading: false, error: error instanceof Error ? error.message : String(error) })
    }).finally(() => { this.inflight = undefined })
    return this.inflight
  }

  /** Debounced refresh after SSE frames that change lists. */
  private scheduleRefresh(): void {
    if (this.refreshTimer !== undefined) return
    this.refreshTimer = setTimeout(() => {
      this.refreshTimer = undefined
      void this.refresh()
    }, 250)
  }

  private connect(): void {
    if (this.source !== undefined || typeof EventSource === 'undefined') return
    const source = new EventSource(`${API_BASE}events`)
    this.source = source
    const handle = (event: MessageEvent): void => {
      let frame: NetworkEventFrame
      try {
        frame = JSON.parse(String(event.data)) as NetworkEventFrame
      } catch {
        return
      }
      const revision = this.snapshot.revision + 1
      const folded = applyFrame(this.snapshot.inbox, frame)
      for (const l of this.frameListeners) l(frame)
      switch (frame.kind) {
        case 'typing':
          this.set({ typing: { ...this.snapshot.typing, [frame.fp]: frame.on }, revision, inbox: folded.state })
          return
        case 'status':
          this.set({ status: frame.status, revision })
          if (frame.status.state === 'ready') this.scheduleRefresh()
          return
        case 'outbound':
        case 'draft':
          this.set({ revision, inbox: folded.state })
          return
        case 'memory':
          for (const l of this.memoryListeners) l({ phase: frame.phase, count: frame.count, ...(frame.memories === undefined ? {} : { memories: frame.memories }), ...(frame.clue === undefined ? {} : { clue: frame.clue }) })
          return
        case 'alter':
          // Folded by the page store (frame listeners); bump the revision so views re-read.
          this.set({ revision })
          return
        case 'agent':
          // The agent pane refetches via the frame listeners; refresh the list so its status dot follows.
          this.set({ revision })
          this.scheduleRefresh()
          return
        case 'message':
        case 'friend_request':
        case 'friend_accept':
        case 'presence':
        case 'group_message':
        case 'group_update':
        case 'group_outbound':
        case 'group_application':
          this.set({ revision, inbox: folded.state })
          if (folded.notice !== undefined) for (const l of this.mailListeners) l(folded.notice)
          this.scheduleRefresh()
          return
        default:
          return
      }
    }
    for (const kind of ['message', 'outbound', 'typing', 'friend_request', 'friend_accept', 'presence', 'status', 'alter', 'agent', 'draft', 'memory', 'group_message', 'group_typing', 'group_update', 'group_outbound', 'group_application']) {
      source.addEventListener(kind, handle as EventListener)
    }
    source.onerror = () => {
      // EventSource reconnects by itself; refresh once it is back.
      this.scheduleRefresh()
    }
  }

  private disconnect(): void {
    this.source?.close()
    this.source = undefined
    if (this.refreshTimer !== undefined) {
      clearTimeout(this.refreshTimer)
      this.refreshTimer = undefined
    }
  }
}

export const networkStore = new NetworkStore()
