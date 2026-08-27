/**
 * Pure inbox state: the friend list as the browser sees it (from
 * `/soulmirror/api/state`) folded with the live SSE frames (`message`,
 * `outbound`, `presence`, `typing`, `friend_accept`, `friend_request`,
 * `draft`) so the sidebar badge, the page's middle column and the new-mail
 * cue update instantly — the authoritative `/state` refetch that follows a
 * frame replaces the optimistic fold. No React, no DOM: unit-tested under
 * node (test/inbox-state.test.ts).
 */
import type { ApiDraft, ApiFriend, ApiGroup, ApiPending, ApiState, NetworkEventFrame } from './api.ts'

export interface InboxFriend extends ApiFriend {}

/** The slice of the network state the inbox surfaces read. */
export interface InboxState {
  readonly friends: readonly InboxFriend[]
  readonly groups: readonly ApiGroup[]
  readonly pending: readonly ApiPending[]
  /** Pending drafts of the alter (P4), oldest first. */
  readonly drafts: readonly ApiDraft[]
  /** The alter session id (host), when known. */
  readonly alterSessionId: string | undefined
  /** fp → typing flag (live, never persisted). */
  readonly typing: Readonly<Record<string, boolean>>
  /**
   * gid → unseen join-application count folded from SSE `group_application`
   * frames (the badge on the group row / home). Cleared when the owner opens
   * the group home (`clearGroupApps`); the authoritative list is `group.get`.
   */
  readonly groupApps: Readonly<Record<string, number>>
}

/** What the new-mail cue needs: who wrote, what, and the alter session id (the toast is suppressed while it is on screen). */
export interface MailNotice {
  readonly id: string
  readonly fp: string
  readonly name: string
  readonly body: string
  readonly ts: number
  readonly sessionId: string | undefined
  /** Present for group mail: the group the message landed in (for open/muted suppression). */
  readonly gid?: string
}

export const EMPTY_INBOX: InboxState = { friends: [], groups: [], pending: [], drafts: [], alterSessionId: undefined, typing: {}, groupApps: {} }

/** Unread total over all friends (the sidebar badge number), skipping muted friends. */
export function unreadTotal(friends: readonly Pick<InboxFriend, 'unread' | 'muted'>[]): number {
  let total = 0
  for (const f of friends) total += f.unread > 0 && f.muted !== true ? f.unread : 0
  return total
}

/** Friends sorted for the inbox: unread first, then by last message time (desc), then by name. */
export function sortInbox(friends: readonly InboxFriend[]): InboxFriend[] {
  return [...friends].sort((a, b) => {
    const ua = a.unread > 0 ? 1 : 0
    const ub = b.unread > 0 ? 1 : 0
    if (ua !== ub) return ub - ua
    const ta = a.lastTs ?? 0
    const tb = b.lastTs ?? 0
    if (ta !== tb) return tb - ta
    return a.name.localeCompare(b.name)
  })
}

/** Pending drafts for one friend, oldest first. */
export function draftsFor(state: Pick<InboxState, 'drafts'>, fp: string): ApiDraft[] {
  return state.drafts.filter(d => d.fp === fp)
}

/** Replace the folded state with a fresh `/state` answer (keeps the live typing flags and, for rows without `online`, the last known presence). */
export function fromApiState(previous: InboxState, state: ApiState): InboxState {
  const known = new Map(previous.friends.map(f => [f.fp, f.online]))
  const drafts = state.drafts ?? []
  const counts = new Map<string, number>()
  for (const d of drafts) counts.set(d.fp, (counts.get(d.fp) ?? 0) + 1)
  return {
    friends: state.friends.map((f) => {
      const n = counts.get(f.fp)
      const row = n === undefined ? withoutDrafts(f) : { ...f, drafts: n }
      if (row.online !== undefined) return row
      const was = known.get(f.fp)
      return was === undefined ? row : { ...row, online: was }
    }),
    groups: state.groups ?? [],
    pending: state.pending,
    drafts,
    alterSessionId: state.alter?.sessionId ?? undefined,
    typing: previous.typing,
    groupApps: previous.groupApps,
  }
}

function patchGroup(groups: readonly ApiGroup[], gid: string, patch: (g: ApiGroup) => ApiGroup): ApiGroup[] {
  return groups.map(g => (g.gid === gid ? patch(g) : g))
}

/** The row without its `drafts` count (exactOptionalPropertyTypes: the key must be absent, not undefined). */
function withoutDrafts(f: InboxFriend): InboxFriend {
  if (f.drafts === undefined) return f
  const { drafts: _dropped, ...rest } = f
  return rest
}

function patchFriend(friends: readonly InboxFriend[], fp: string, patch: (f: InboxFriend) => InboxFriend): InboxFriend[] {
  return friends.map(f => (f.fp === fp ? patch(f) : f))
}

/**
 * Fold one SSE frame into the state. Returns the next state and, for
 * `message` frames, the notice the new-mail cue should show.
 */
export function applyFrame(state: InboxState, frame: NetworkEventFrame): { state: InboxState; notice?: MailNotice } {
  switch (frame.kind) {
    case 'message': {
      const { message } = frame
      const known = state.friends.some(f => f.fp === message.from)
      const friends = known
        ? patchFriend(state.friends, message.from, f => ({
          ...f,
          unread: f.unread + 1,
          count: f.count + 1,
          lastTs: message.ts,
          lastBody: message.body,
        }))
        // Mail from a friend the list does not know yet (accepted while the
        // page was away): show a provisional row; the refetch fills it in.
        : [...state.friends, { fp: message.from, name: message.name, unread: 1, count: 1, lastTs: message.ts, lastBody: message.body }]
      const name = state.friends.find(f => f.fp === message.from)?.name ?? message.name
      return {
        state: { ...state, friends },
        notice: { id: message.id, fp: message.from, name, body: message.body, ts: message.ts, sessionId: state.alterSessionId },
      }
    }
    case 'typing':
      return { state: { ...state, typing: { ...state.typing, [frame.fp]: frame.on } } }
    case 'presence':
      return { state: { ...state, friends: patchFriend(state.friends, frame.fp, f => ({ ...f, online: frame.online })) } }
    case 'friend_accept': {
      const exists = state.friends.some(f => f.fp === frame.friend.fp)
      const friends = exists
        ? patchFriend(state.friends, frame.friend.fp, f => ({ ...f, ...frame.friend }))
        : [...state.friends, frame.friend]
      // The request that produced this friend is no longer pending.
      const pending = state.pending.filter(p => p.fp !== frame.friend.fp)
      return { state: { ...state, friends, pending } }
    }
    case 'friend_request': {
      const exists = state.pending.some(p => p.id === frame.request.id)
      return { state: exists ? state : { ...state, pending: [...state.pending, frame.request] } }
    }
    case 'outbound': {
      // The alter (or the debug send) wrote to the friend: the row's preview and
      // time move, the unread count does not.
      const { fp, entry } = frame
      const friends = patchFriend(state.friends, fp, f => ({
        ...f,
        count: f.count + 1,
        ...(entry.ts >= (f.lastTs ?? 0) ? { lastTs: entry.ts, lastBody: entry.body } : {}),
      }))
      return { state: { ...state, friends } }
    }
    case 'draft': {
      const without = state.drafts.filter(d => d.id !== frame.draft.id)
      const drafts = frame.action === 'added' ? [...without, frame.draft] : without
      const n = drafts.filter(d => d.fp === frame.draft.fp).length
      const friends = patchFriend(state.friends, frame.draft.fp, f => (n === 0 ? withoutDrafts(f) : { ...f, drafts: n }))
      return { state: { ...state, drafts, friends } }
    }
    case 'group_message': {
      const { gid, message } = frame
      const groups = patchGroup(state.groups, gid, g => ({
        ...g,
        unread: g.unread + 1,
        count: g.count + 1,
        lastTs: message.ts,
        lastBody: message.body,
      }))
      const group = state.groups.find(g => g.gid === gid)
      const next = { ...state, groups }
      if (group === undefined) return { state: next }
      // The new-mail cue reuses the friend notice shape; name = "sender · group".
      return {
        state: next,
        notice: { id: message.id, fp: message.from, name: `${message.name} · ${group.name}`, body: message.body, ts: message.ts, sessionId: state.alterSessionId, gid },
      }
    }
    case 'group_outbound': {
      const { gid, entry } = frame
      const groups = patchGroup(state.groups, gid, g => ({
        ...g,
        count: g.count + 1,
        ...(entry.ts >= (g.lastTs ?? 0) ? { lastTs: entry.ts, lastBody: entry.body } : {}),
      }))
      return { state: { ...state, groups } }
    }
    case 'group_update':
      // Membership changes need the authoritative list; the refetch that follows handles it.
      return { state }
    case 'group_application': {
      const { gid } = frame
      return { state: { ...state, groupApps: { ...state.groupApps, [gid]: (state.groupApps[gid] ?? 0) + 1 } } }
    }
    case 'status':
    case 'alter':
    default:
      return { state }
  }
}

/** The owner saw a group's applications (opened the home): drop its unseen badge count. */
export function clearGroupApps(state: InboxState, gid: string): InboxState {
  if (state.groupApps[gid] === undefined) return state
  const { [gid]: _dropped, ...rest } = state.groupApps
  return { ...state, groupApps: rest }
}

/** The owner read a group: zero its unread count locally (the host cursor moves in parallel). */
export function markReadGroupLocal(state: InboxState, gid: string): InboxState {
  if (!state.groups.some(g => g.gid === gid && g.unread > 0)) return state
  return { ...state, groups: patchGroup(state.groups, gid, g => ({ ...g, unread: 0 })) }
}

/** The owner read a conversation: zero its unread count locally (the host/peer cursor moves in parallel). */
export function markReadLocal(state: InboxState, fp: string): InboxState {
  if (!state.friends.some(f => f.fp === fp && f.unread > 0)) return state
  return { ...state, friends: patchFriend(state.friends, fp, f => ({ ...f, unread: 0 })) }
}

/** Should the new-mail cue fire? Only while the alter session is not the one on screen. */
export function shouldNotify(notice: MailNotice, currentSessionId: string | undefined): boolean {
  return notice.sessionId === undefined || notice.sessionId !== currentSessionId
}

/** Compact relative time for the inbox row: "now", "5m", "3h", "2d", else a short date. */
export function formatAge(ts: number | undefined, now: number = Date.now()): string {
  if (ts === undefined || ts <= 0) return ''
  const diff = Math.max(0, now - ts)
  const minute = 60_000
  if (diff < minute) return 'now'
  if (diff < 60 * minute) return `${Math.floor(diff / minute)}m`
  if (diff < 24 * 60 * minute) return `${Math.floor(diff / (60 * minute))}h`
  if (diff < 7 * 24 * 60 * minute) return `${Math.floor(diff / (24 * 60 * minute))}d`
  const d = new Date(ts)
  return `${d.getMonth() + 1}/${d.getDate()}`
}

/** One-line preview of a message body for the inbox row. */
export function previewOf(body: string | undefined, max = 60): string {
  if (body === undefined) return ''
  const flat = body.replace(/\s+/g, ' ').trim()
  return flat.length > max ? `${flat.slice(0, max - 1)}…` : flat
}
