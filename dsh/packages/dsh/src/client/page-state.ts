/**
 * Pure state of the SoulMirror page (P2b): which friend is selected, the
 * per-friend message thread (archive entries + optimistic outbound bubbles),
 * and the row / bubble helpers. Folds: `conversation.get` answers (merge by
 * seq, dedupe by id), SSE `message` frames (inbound), SSE `outbound` frames
 * (another tab sent), optimistic send → reconcile / fail. No React, no DOM:
 * unit-tested under node (test/page-state.test.ts). The friend list itself
 * is the inbox state (./inbox-state.ts); this module only adds the search
 * filter and the selection rule on top of it.
 */
import type { ApiEntry } from './api.ts'
import { sortInbox, type InboxFriend } from './inbox-state.ts'

/** One bubble of the thread: an archived entry, or an outbound one not yet acknowledged by the peer. */
export interface ThreadEntry {
  /** Archive line number (1-based); 0 while the entry is only optimistic. */
  readonly seq: number
  readonly dir: 'in' | 'out'
  /** A2A message id; for optimistic entries the client id until the receipt arrives. */
  readonly id: string
  readonly body: string
  /** Unix epoch ms. */
  readonly ts: number
  /** Sender fingerprint (group threads: who spoke). */
  readonly from?: string
  readonly type?: string
  readonly auto?: true
  /** Group provenance: the sender's human owner typed it, or their alter composed it. */
  readonly by?: 'owner' | 'alter'
  /** Which of the sender's seat agents composed a by=alter post; absent = their default alter. */
  readonly agent?: string
  /** Outbound delivery state: `sending` (optimistic) | `sent` | `queued` | `error` | `failed` (the send call itself failed). */
  readonly status?: string
  readonly artifactName?: string
  /** Client-side id of an optimistic send (stable across reconcile; the React key). */
  readonly clientId?: string
  /** Error text when the send call failed (status `failed`). */
  readonly error?: string
}

export interface ThreadState {
  readonly entries: readonly ThreadEntry[]
  /** The `limit` of the last archive fetch (grows when the owner scrolls up). */
  readonly window: number
  /** The archive has no older entries than the ones loaded. */
  readonly complete: boolean
  readonly loading: boolean
  readonly loaded: boolean
  readonly error?: string
}

export const EMPTY_THREAD: ThreadState = { entries: [], window: 0, complete: false, loading: false, loaded: false }

/** Entries per archive fetch; the window grows by this much per "load older". */
export const PAGE_SIZE = 50

export function entryFromApi(entry: ApiEntry): ThreadEntry {
  return {
    seq: entry.seq,
    dir: entry.dir === 'out' ? 'out' : 'in',
    id: entry.id,
    body: entry.body,
    ts: entry.ts,
    ...(entry.from === undefined ? {} : { from: entry.from }),
    ...(entry.type === undefined ? {} : { type: entry.type }),
    ...(entry.auto === true ? { auto: true as const } : {}),
    ...(entry.by === undefined ? {} : { by: entry.by }),
    ...(entry.agent === undefined ? {} : { agent: entry.agent }),
    ...(entry.status === undefined ? {} : { status: entry.status }),
    ...(entry.artifactName === undefined ? {} : { artifactName: entry.artifactName }),
  }
}

/** Stable ordering: archived entries by seq, then optimistic ones (seq 0) by time at the tail. */
export function sortThread(entries: readonly ThreadEntry[]): ThreadEntry[] {
  return [...entries].sort((a, b) => {
    const pa = a.seq > 0 ? 0 : 1
    const pb = b.seq > 0 ? 0 : 1
    if (pa !== pb) return pa - pb
    if (pa === 0 && a.seq !== b.seq) return a.seq - b.seq
    if (a.ts !== b.ts) return a.ts - b.ts
    return a.id.localeCompare(b.id)
  })
}

/**
 * Merge archive entries into the thread: an entry with the same seq replaces
 * the loaded one (status may have moved from queued → sent), an entry with
 * the same id as an optimistic bubble absorbs it (keeps its clientId so the
 * React key is stable), everything else is appended. Result sorted.
 */
export function mergeEntries(existing: readonly ThreadEntry[], incoming: readonly ThreadEntry[]): ThreadEntry[] {
  const bySeq = new Map<number, number>()
  const byId = new Map<string, number>()
  const next: ThreadEntry[] = [...existing]
  next.forEach((e, i) => {
    if (e.seq > 0) bySeq.set(e.seq, i)
    byId.set(e.id, i)
  })
  for (const entry of incoming) {
    const seqIndex = entry.seq > 0 ? bySeq.get(entry.seq) : undefined
    const idIndex = byId.get(entry.id)
    const index = seqIndex ?? idIndex
    if (index === undefined) {
      next.push(entry)
      if (entry.seq > 0) bySeq.set(entry.seq, next.length - 1)
      byId.set(entry.id, next.length - 1)
      continue
    }
    const previous = next[index]!
    const merged: ThreadEntry = { ...previous, ...entry, ...(previous.clientId === undefined ? {} : { clientId: previous.clientId }) }
    next[index] = merged
    if (merged.seq > 0) bySeq.set(merged.seq, index)
    byId.set(merged.id, index)
    if (previous.id !== merged.id) byId.delete(previous.id)
  }
  return sortThread(next)
}

/** Fold one `conversation.get` answer (the last `window` entries) into the thread. */
export function applyArchive(state: ThreadState, entries: readonly ApiEntry[], window: number): ThreadState {
  const incoming = entries.map(entryFromApi)
  const { error: _dropped, ...rest } = state
  return {
    ...rest,
    entries: mergeEntries(state.entries, incoming),
    window,
    // Fewer entries than asked for = the archive starts inside the window.
    complete: incoming.length < window,
    loading: false,
    loaded: true,
  }
}

/** Append one inbound message (SSE `message` / `group_message` frame) to the thread. */
export function applyInbound(state: ThreadState, message: { id: string; from?: string; body: string; ts: number; seq?: number; type?: string; auto?: true; by?: 'owner' | 'alter'; agent?: string; artifactName?: string }): ThreadState {
  const entry: ThreadEntry = {
    seq: message.seq ?? 0,
    dir: 'in',
    id: message.id,
    body: message.body,
    ts: message.ts,
    // Group threads label bubbles by the sender; without this the live path showed "?" until a refetch.
    ...(message.from === undefined || message.from === '' ? {} : { from: message.from }),
    ...(message.type === undefined ? {} : { type: message.type }),
    ...(message.auto === true ? { auto: true as const } : {}),
    ...(message.by === undefined ? {} : { by: message.by }),
    ...(message.agent === undefined || message.agent === '' ? {} : { agent: message.agent }),
    ...(message.artifactName === undefined ? {} : { artifactName: message.artifactName }),
  }
  return { ...state, entries: mergeEntries(state.entries, [entry]) }
}

/** Append one archived outbound entry (SSE `outbound` frame from another tab, or our own receipt). */
export function applyOutbound(state: ThreadState, entry: ApiEntry): ThreadState {
  return { ...state, entries: mergeEntries(state.entries, [entryFromApi(entry)]) }
}

/** The owner pressed Send: show the bubble at once (status `sending`). */
export function addOptimistic(state: ThreadState, draft: { clientId: string; body: string; ts: number }): ThreadState {
  const entry: ThreadEntry = { seq: 0, dir: 'out', id: draft.clientId, body: draft.body, ts: draft.ts, status: 'sending', clientId: draft.clientId }
  return { ...state, entries: [...state.entries, entry] }
}

/** The host answered `message.send` with the archived entry: replace the optimistic bubble in place. */
export function reconcileSend(state: ThreadState, clientId: string, entry: ApiEntry): ThreadState {
  const archived = entryFromApi(entry)
  const index = state.entries.findIndex(e => e.clientId === clientId)
  if (index === -1) return { ...state, entries: mergeEntries(state.entries, [archived]) }
  const next = [...state.entries]
  next[index] = { ...archived, clientId }
  // The archive may already hold the same seq (an `outbound` frame raced the
  // answer): drop the duplicate that is not ours.
  const deduped = next.filter((e, i) => i === index || (!(e.seq > 0 && e.seq === archived.seq) && e.id !== archived.id))
  return { ...state, entries: sortThread(deduped) }
}

/** The send call failed: keep the bubble, mark it `failed` with the error. */
export function failSend(state: ThreadState, clientId: string, error: string): ThreadState {
  return {
    ...state,
    entries: state.entries.map(e => (e.clientId === clientId ? { ...e, status: 'failed', error } : e)),
  }
}

/** Remove a failed optimistic bubble (the owner discarded it or retries). */
export function dropEntry(state: ThreadState, clientId: string): ThreadState {
  return { ...state, entries: state.entries.filter(e => e.clientId !== clientId) }
}

/** Highest archived seq in the thread (the read cursor to report). */
export function lastSeq(state: ThreadState): number {
  let max = 0
  for (const e of state.entries) if (e.seq > max) max = e.seq
  return max
}

// ——— friend list helpers ———

/** Case-insensitive filter over name / remark / card name / fingerprint prefix. */
export function filterFriends(friends: readonly InboxFriend[], query: string): InboxFriend[] {
  const q = query.trim().toLowerCase()
  if (q === '') return [...friends]
  return friends.filter(f =>
    f.name.toLowerCase().includes(q)
    || (f.remark?.toLowerCase().includes(q) ?? false)
    || (f.cardName?.toLowerCase().includes(q) ?? false)
    || f.fp.toLowerCase().startsWith(q))
}

/** Rows of the middle column: unread first, then newest, then by name; then the search filter. */
export function listRows(friends: readonly InboxFriend[], query: string): InboxFriend[] {
  return filterFriends(sortInbox(friends), query)
}

/** Selection key of the pinned first item "My alter" (never a fingerprint). */
export const ALTER_KEY = 'alter'

/** Selection-key prefix of a group row ("g:<gid>"; a fingerprint never contains ':'). */
export const GROUP_PREFIX = 'g:'

/** Selection key of one group. */
export function groupKey(gid: string): string {
  return GROUP_PREFIX + gid
}

/** The gid of a group selection key, or undefined for friend/alter keys. */
export function gidOf(selected: string | undefined): string | undefined {
  return selected !== undefined && selected.startsWith(GROUP_PREFIX) ? selected.slice(GROUP_PREFIX.length) : undefined
}

/** Selection-key prefix of a seat-agent row ("a:<name>"; a fingerprint never contains ':'). */
export const AGENT_PREFIX = 'a:'

/** Selection key of one seat agent. */
export function agentKey(name: string): string {
  return AGENT_PREFIX + name
}

/** The agent name of an agent selection key, or undefined for other keys. */
export function agentOf(selected: string | undefined): string | undefined {
  return selected !== undefined && selected.startsWith(AGENT_PREFIX) ? selected.slice(AGENT_PREFIX.length) : undefined
}

/**
 * Which item the page shows on the right: "My alter" (the pinned first item,
 * also the default — selecting it has no side effect), the explicitly
 * selected friend while it is still a friend, or a group while I am still in
 * it (a gone friend/group never re-selects itself; it falls back to the alter).
 */
export function resolveSelection(friends: readonly Pick<InboxFriend, 'fp'>[], groups: readonly { gid: string }[], agents: readonly { name: string }[], selected: string | undefined): string {
  if (selected === undefined || selected === ALTER_KEY) return ALTER_KEY
  const gid = gidOf(selected)
  if (gid !== undefined) return groups.some(g => g.gid === gid) ? selected : ALTER_KEY
  const agent = agentOf(selected)
  if (agent !== undefined) return agents.some(a => a.name === agent) ? selected : ALTER_KEY
  return friends.some(f => f.fp === selected) ? selected : ALTER_KEY
}

// ——— page tabs (col2 = second column, pane = third column body) ———

/** Which section the second column (the list) shows: the message list, or the address book. */
export type Col2Tab = 'messages' | 'contacts'

/** Which panel of the third column (content area) is active. */
export type PaneTab = 'chat' | 'announce' | 'home' | 'members' | 'admin' | 'info' | 'settings' | 'memory'

/** The kind of the current selection, for tab availability. */
export type SelectionKind = 'alter' | 'friend' | 'group' | 'agent'

export function kindOf(selected: string | undefined): SelectionKind {
  if (selected === undefined || selected === ALTER_KEY) return 'alter'
  if (gidOf(selected) !== undefined) return 'group'
  if (agentOf(selected) !== undefined) return 'agent'
  return 'friend'
}

/**
 * The tab set the third column offers for a selection. `admin` (group
 * management) is group-owner/admin only; the rest follow the product's rows:
 * a friend has chat + home, a group has chat / announce / home / members
 * (+ admin), the alter and every agent have a memory page.
 */
export function tabsFor(kind: SelectionKind, canAdmin: boolean): PaneTab[] {
  switch (kind) {
    case 'alter': return ['chat', 'home', 'memory', 'settings']
    case 'friend': return ['chat', 'home']
    case 'group': return canAdmin ? ['chat', 'announce', 'home', 'memory', 'members', 'admin'] : ['chat', 'announce', 'home', 'memory', 'members']
    case 'agent': return ['chat', 'memory', 'info']
  }
}

/** The default tab when a selection is entered. */
export const DEFAULT_PANE_TAB: PaneTab = 'chat'

// ——— bubble helpers ———

/** Local calendar day key (YYYY-MM-DD) for the day separators. */
export function dayKey(ts: number): string {
  const d = new Date(ts)
  const m = String(d.getMonth() + 1).padStart(2, '0')
  const day = String(d.getDate()).padStart(2, '0')
  return `${d.getFullYear()}-${m}-${day}`
}

export type ThreadRow =
  | { readonly kind: 'day'; readonly key: string; readonly ts: number }
  | { readonly kind: 'entry'; readonly key: string; readonly entry: ThreadEntry }

/** Interleave day separators: one before the first entry of every local day. */
export function withDaySeparators(entries: readonly ThreadEntry[]): ThreadRow[] {
  const rows: ThreadRow[] = []
  let lastDay: string | undefined
  for (const entry of entries) {
    const day = dayKey(entry.ts)
    if (day !== lastDay) {
      rows.push({ kind: 'day', key: `day:${day}`, ts: entry.ts })
      lastDay = day
    }
    rows.push({ kind: 'entry', key: entry.clientId ?? (entry.seq > 0 ? `seq:${entry.seq}` : `id:${entry.id}`), entry })
  }
  return rows
}

/** Bubble time: HH:MM (24 h, local). */
export function formatClock(ts: number): string {
  const d = new Date(ts)
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
}

/** Day separator label: "Today" / "Yesterday" (via the dictionary) or a local date. */
export function formatDay(ts: number, now: number, labels: { today: string; yesterday: string }): string {
  const today = dayKey(now)
  const key = dayKey(ts)
  if (key === today) return labels.today
  if (key === dayKey(now - 24 * 60 * 60 * 1000)) return labels.yesterday
  const d = new Date(ts)
  const sameYear = d.getFullYear() === new Date(now).getFullYear()
  return sameYear ? `${d.getMonth() + 1}/${d.getDate()}` : `${d.getFullYear()}/${d.getMonth() + 1}/${d.getDate()}`
}
