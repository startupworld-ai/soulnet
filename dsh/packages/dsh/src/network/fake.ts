/**
 * In-memory fake NetworkClient (config `backend: fake`): two friends, one
 * pending request, one canned inbound message shortly after the first
 * subscribe, an auto-reply echo for every send, no I/O. Selectable for tests
 * and UI work; the default backend is the `soulnet` light peer (./soulnet.ts).
 */
import type { A2AMessageId, Fingerprint } from '../events.ts'
import {
  NetworkError,
  NetworkErrorCode,
  type BackendStatus,
  type ConversationEntry,
  type Friend,
  type Group,
  type GroupApplication,
  type GroupInfo,
  type GroupMember,
  type GroupPin,
  type GroupProfile,
  type Identity,
  type NetworkClient,
  type NetworkEvent,
  type PendingRequest,
  type SendReceipt,
} from './types.ts'

const fp = (s: string): Fingerprint => s as Fingerprint
const mid = (): A2AMessageId => `a2a-${crypto.randomUUID()}` as A2AMessageId

export const FAKE_FRIENDS: readonly Friend[] = [
  { fp: fp('fp-alice-1f2e3d4c5b6a7988'), name: 'college friend', cardName: 'Alice', remark: 'college friend', online: true, unread: 0, count: 0 },
  { fp: fp('fp-bob-9a8b7c6d5e4f3021'), name: 'Bob', cardName: 'Bob', online: false, unread: 0, count: 0 },
]

export const FAKE_PENDING: readonly PendingRequest[] = [
  { id: 'req-carol-1', fp: fp('fp-carol-5566778899aabbcc'), name: 'Carol', greeting: 'Hi, Alice gave me your card.' },
]

export interface FakeOptions {
  /** Delay before the canned inbound message fires after the first subscribe (ms); negative = never. */
  readonly firstInboundDelayMs?: number
  /** Start without an identity (first-run onboarding path). Default: identity present. */
  readonly noIdentity?: boolean
}

export function createFakeNetworkClient(options: FakeOptions = {}): NetworkClient {
  const listeners = new Set<(event: NetworkEvent) => void>()
  const friends = new Map<string, Friend>(FAKE_FRIENDS.map(f => [f.fp, f]))
  const pending = new Map<string, PendingRequest>(FAKE_PENDING.map(p => [p.id, p]))
  const conversations = new Map<string, ConversationEntry[]>()
  let identity: Identity | undefined = options.noIdentity === true
    ? undefined
    : { fp: fp('fp-me-0000aaaabbbbcccc'), name: 'dsh tester', cardUri: 'soulmirror://card?v=1&pk=FAKE&xpk=FAKE&name=dsh%20tester' }
  let firstInboundFired = false
  let disposed = false
  const status: BackendStatus = { backend: 'fake', state: 'ready', restarts: 0 }

  const emit = (event: NetworkEvent): void => {
    for (const l of listeners) {
      try {
        l(event)
      } catch {
        // listener errors never kill the fake
      }
    }
  }
  const archive = (peer: string, entry: Omit<ConversationEntry, 'seq'>): ConversationEntry => {
    const list = conversations.get(peer) ?? []
    const full: ConversationEntry = { seq: list.length + 1, ...entry }
    list.push(full)
    conversations.set(peer, list)
    const friend = friends.get(peer)
    if (friend !== undefined) {
      friends.set(peer, {
        ...friend,
        count: list.length,
        unread: entry.dir === 'in' ? friend.unread + 1 : friend.unread,
        lastTs: entry.ts,
        lastBody: entry.body,
      })
    }
    return full
  }
  const deliver = (from: Fingerprint, body: string, auto?: true): void => {
    if (disposed) return
    const friend = friends.get(from)
    const id = mid()
    const ts = Date.now()
    const entry = archive(from, { dir: 'in', id, body, ts, ...(auto ? { auto } : {}) })
    emit({
      kind: 'message',
      message: { id, from, name: friend?.name ?? from, body, ts, seq: entry.seq, ...(auto ? { auto } : {}) },
    })
  }
  const requireIdentity = (): Identity => {
    if (identity === undefined) throw new NetworkError('no identity yet (identity.create first)', NetworkErrorCode.noIdentity)
    return identity
  }
  const friendFromCard = (uri: string, remark?: string): Friend => ({
    fp: fp(`fp-${uri.slice(-8).replace(/[^a-z0-9]/gi, '') || 'card'}`),
    name: remark ?? `card ${uri.slice(-4)}`,
    cardName: `card ${uri.slice(-4)}`,
    ...(remark === undefined ? {} : { remark }),
    unread: 0,
    count: 0,
  })

  /** The "standard group" preset (mirrors Go `a2a.DefaultGroupProfile`). */
  const defaultProfile = (): GroupProfile => ({
    template: 'standard', room: 'chat', speakHumans: true, speakAgents: true,
    speakWho: 'all', join: 'invite', agentWake: 'mention', agentTier: 'draft',
    autoPerHour: 10, agentRounds: 3,
  })

  interface FakeGroup {
    name: string
    ownerFp: Fingerprint
    version: number
    memberList: GroupMember[]
    entries: ConversationEntry[]
    unread: number
    profile: GroupProfile
    pins: GroupPin[]
    applications: GroupApplication[]
    /** The canned stranger application was already delivered once. */
    applicationSeeded: boolean
  }
  const fakeGroups = new Map<string, FakeGroup>()
  const myFp = (): Fingerprint => identity?.fp ?? fp('fp-me-0000aaaabbbbcccc')
  const groupRow = (gid: string, g: FakeGroup): Group => {
    const last = g.entries[g.entries.length - 1]
    return {
      gid,
      name: g.name,
      ownerFp: g.ownerFp,
      mine: g.ownerFp === myFp(),
      version: g.version,
      members: g.memberList.length,
      unread: g.unread,
      count: g.entries.length,
      ...(last === undefined ? {} : { lastTs: last.ts, lastBody: last.body }),
      profile: g.profile,
    }
  }
  const roleOf = (g: FakeGroup): 'owner' | 'admin' | 'member' =>
    g.ownerFp === myFp() ? 'owner' : (g.profile.admins ?? []).includes(myFp()) ? 'admin' : 'member'
  const groupInfoOf = (gid: string): GroupInfo => {
    const g = fakeGroups.get(gid)
    if (g === undefined) throw new NetworkError('unknown group', NetworkErrorCode.notFound)
    const role = roleOf(g)
    return {
      ...groupRow(gid, g),
      memberList: g.memberList,
      pins: [...g.pins],
      myRole: role,
      ...(role === 'owner' ? { applications: [...g.applications] } : {}),
    }
  }
  const groupOf = (gid: string): FakeGroup => {
    const g = fakeGroups.get(gid)
    if (g === undefined) throw new NetworkError('unknown group', NetworkErrorCode.notFound)
    return g
  }
  /** Groups accepting applications get one canned stranger application shortly after (demo of the approval flow). */
  const seedApplication = (gid: string): void => {
    const g = fakeGroups.get(gid)
    if (g === undefined || g.applicationSeeded || g.ownerFp !== myFp()) return
    const join = g.profile.join ?? 'invite'
    if (join !== 'apply' && join !== 'open') return
    g.applicationSeeded = true
    setTimeout(() => {
      if (disposed || !fakeGroups.has(gid)) return
      const request = { fp: fp('fp-dave-77aa88bb99cc00dd'), name: 'Dave', note: 'Saw your group card - can I join?' }
      g.applications.push({ ...request, ts: Date.now() })
      emit({ kind: 'group_application', gid, request })
    }, 1200)
  }
  let pinSeq = 0

  return {
    backend: 'fake',
    status: () => status,
    identity: () => Promise.resolve(identity),
    createIdentity: (name) => {
      if (identity !== undefined) return Promise.reject(new NetworkError('identity already exists', NetworkErrorCode.identityExists))
      identity = { fp: fp('fp-me-0000aaaabbbbcccc'), name, cardUri: `soulmirror://card?v=1&pk=FAKE&xpk=FAKE&name=${encodeURIComponent(name)}` }
      return Promise.resolve(identity)
    },
    card: () => Promise.resolve(requireIdentity().cardUri),
    parseCard: (uri) => {
      if (!uri.startsWith('soulmirror://card')) return Promise.reject(new NetworkError('invalid card link', NetworkErrorCode.badCard))
      const f = friendFromCard(uri)
      return Promise.resolve({ fp: f.fp, name: f.cardName ?? f.name, uri })
    },
    friends: {
      list: () => Promise.resolve([...friends.values()]),
      pending: () => Promise.resolve([...pending.values()]),
      add: (uri, remark) => {
        requireIdentity()
        if (!uri.startsWith('soulmirror://card')) return Promise.reject(new NetworkError('invalid card link', NetworkErrorCode.badCard))
        const f = friendFromCard(uri, remark)
        friends.set(f.fp, f)
        // The peer "accepts" shortly after.
        setTimeout(() => { if (!disposed) emit({ kind: 'friend_accept', friend: f }) }, 300)
        return Promise.resolve(f)
      },
      accept: (requestId, note) => {
        const req = pending.get(requestId)
        if (req === undefined) return Promise.reject(new NetworkError('no such pending request', NetworkErrorCode.notFound))
        pending.delete(requestId)
        const f: Friend = { fp: req.fp, name: note ?? req.name, cardName: req.name, ...(note === undefined ? {} : { remark: note }), unread: 0, count: 0 }
        friends.set(f.fp, f)
        return Promise.resolve(f)
      },
      reject: (requestId) => {
        if (!pending.delete(requestId)) return Promise.reject(new NetworkError('no such pending request', NetworkErrorCode.notFound))
        return Promise.resolve()
      },
      set: (id, patch) => {
        const cur = friends.get(id)
        if (cur === undefined) return Promise.reject(new NetworkError('not a friend', NetworkErrorCode.notFriend))
        const { protocol: _old, ...rest } = cur
        const protocol = patch.protocol === undefined ? cur.protocol : patch.protocol.trim() === '' ? undefined : patch.protocol
        const next: Friend = {
          ...rest,
          ...(patch.remark === undefined ? {} : { remark: patch.remark, name: patch.remark }),
          ...(protocol === undefined ? {} : { protocol }),
        }
        friends.set(id, next)
        return Promise.resolve(next)
      },
      remove: (id) => {
        if (!friends.delete(id)) return Promise.reject(new NetworkError('not a friend', NetworkErrorCode.notFriend))
        return Promise.resolve()
      },
      card: (id) => {
        const cur = friends.get(id)
        if (cur === undefined) return Promise.reject(new NetworkError('not a friend', NetworkErrorCode.notFriend))
        return Promise.resolve({ fp: cur.fp, name: cur.cardName ?? cur.name, uri: `soulmirror://card?v=1&pk=FAKE-${cur.fp}&xpk=FAKE&name=${encodeURIComponent(cur.cardName ?? cur.name)}` })
      },
    },
    groups: {
      list: () => Promise.resolve([...fakeGroups.entries()].map(([gid, g]) => groupRow(gid, g))),
      create: (name, members, profile) => {
        const me = requireIdentity()
        if (profile !== undefined && !profile.speakHumans && !profile.speakAgents) {
          return Promise.reject(new NetworkError('at least one of speakHumans/speakAgents must be true', -32602))
        }
        const memberList: GroupMember[] = [
          { fp: me.fp, name: me.name },
          ...members.map(m => ({ fp: m, name: friends.get(m)?.name ?? m })),
        ]
        const gid = `fake${crypto.randomUUID().replace(/-/g, '').slice(0, 28)}`
        fakeGroups.set(gid, {
          name,
          ownerFp: me.fp,
          version: 1,
          memberList,
          entries: [],
          unread: 0,
          profile: { ...defaultProfile(), ...(profile ?? {}) },
          pins: [],
          applications: [],
          applicationSeeded: false,
        })
        seedApplication(gid)
        return Promise.resolve(groupInfoOf(gid))
      },
      info: (gid) => Promise.resolve(groupInfoOf(gid)),
      send: (gid, body, options) => {
        const g = fakeGroups.get(gid)
        if (g === undefined) return Promise.reject(new NetworkError('unknown group', NetworkErrorCode.notFound))
        const by = options?.by ?? 'owner'
        // The sending node enforces the governance switches (the relay only sees ciphertext).
        if (by === 'alter' && !g.profile.speakAgents) return Promise.reject(new NetworkError('agents do not speak in this group', -32602))
        if (by === 'owner' && !g.profile.speakHumans) return Promise.reject(new NetworkError('humans do not speak in this group (the alter does)', -32602))
        const who = g.profile.speakWho ?? 'all'
        const role = roleOf(g)
        if (who === 'owner' && role !== 'owner') return Promise.reject(new NetworkError('only the owner speaks in this group', -32602))
        if (who === 'admins' && role === 'member') return Promise.reject(new NetworkError('only the owner and admins speak in this group', -32602))
        const id = mid()
        const agent = options?.agent
        const entry: ConversationEntry = { seq: g.entries.length + 1, dir: 'out', id, body, ts: Date.now(), status: 'sent', by, ...(options?.auto === true ? { auto: true } : {}), ...(by === 'alter' && agent !== undefined && agent !== '' ? { agent } : {}) }
        g.entries.push(entry)
        // A member echoes 600 ms later (their alter when agents speak here).
        const other = g.memberList.find(m => m.fp !== identity?.fp)
        if (other !== undefined) {
          setTimeout(() => {
            if (disposed || !fakeGroups.has(gid)) return
            const echoId = mid()
            const ts = Date.now()
            const echoBy = g.profile.speakAgents ? 'alter' as const : 'owner' as const
            const echo: ConversationEntry = { seq: g.entries.length + 1, dir: 'in', id: echoId, body: `(group echo) ${body.slice(0, 40)}`, ts, by: echoBy }
            g.entries.push(echo)
            g.unread += 1
            emit({ kind: 'group_message', gid, message: { id: echoId, from: other.fp, name: other.name, body: echo.body, ts, seq: echo.seq, by: echoBy } })
          }, 600)
        }
        return Promise.resolve({ id, seq: entry.seq, status: 'sent' })
      },
      conversation: (gid, opts = {}) => {
        const g = fakeGroups.get(gid)
        if (g === undefined) return Promise.reject(new NetworkError('unknown group', NetworkErrorCode.notFound))
        let entries = [...g.entries]
        if (opts.since !== undefined) entries = entries.filter(e => e.seq > (opts.since as number))
        if (opts.limit !== undefined && opts.limit > 0 && entries.length > opts.limit) entries = entries.slice(-opts.limit)
        return Promise.resolve({ entries })
      },
      announceVoices: () => Promise.resolve(),
      typing: () => Promise.resolve(),
      markRead: (gid) => {
        const g = fakeGroups.get(gid)
        if (g === undefined) return Promise.reject(new NetworkError('unknown group', NetworkErrorCode.notFound))
        g.unread = 0
        return Promise.resolve()
      },
      leave: (gid) => {
        if (!fakeGroups.delete(gid)) return Promise.reject(new NetworkError('unknown group', NetworkErrorCode.notFound))
        emit({ kind: 'group_update', gid })
        return Promise.resolve()
      },
      kick: (gid, target) => {
        const g = fakeGroups.get(gid)
        if (g === undefined) return Promise.reject(new NetworkError('unknown group', NetworkErrorCode.notFound))
        g.memberList = g.memberList.filter(m => m.fp !== target)
        g.version += 1
        emit({ kind: 'group_update', gid })
        return Promise.resolve()
      },
      setProfile: (gid, profile) => {
        const g = fakeGroups.get(gid)
        if (g === undefined) return Promise.reject(new NetworkError('unknown group', NetworkErrorCode.notFound))
        if (roleOf(g) !== 'owner') return Promise.reject(new NetworkError('only the owner edits the group profile', -32602))
        if (!profile.speakHumans && !profile.speakAgents) {
          return Promise.reject(new NetworkError('at least one of speakHumans/speakAgents must be true', -32602))
        }
        g.profile = { ...profile }
        g.version += 1
        emit({ kind: 'group_update', gid })
        seedApplication(gid)
        return Promise.resolve()
      },
      pin: (gid, body) => {
        const g = fakeGroups.get(gid)
        if (g === undefined) return Promise.reject(new NetworkError('unknown group', NetworkErrorCode.notFound))
        if (roleOf(g) === 'member') return Promise.reject(new NetworkError('only the owner and admins pin in this group', -32602))
        pinSeq += 1
        g.pins.push({ id: `pin-${pinSeq}`, from: myFp(), ts: Date.now(), body })
        emit({ kind: 'group_update', gid })
        return Promise.resolve()
      },
      unpin: (gid, id) => {
        const g = fakeGroups.get(gid)
        if (g === undefined) return Promise.reject(new NetworkError('unknown group', NetworkErrorCode.notFound))
        g.pins = g.pins.filter(p => p.id !== id)
        emit({ kind: 'group_update', gid })
        return Promise.resolve()
      },
      apply: (uri, note) => {
        void note
        const me = requireIdentity()
        if (!uri.startsWith('soulmirror://group?')) return Promise.reject(new NetworkError('not a group link', NetworkErrorCode.badCard))
        const q = new URLSearchParams(uri.slice('soulmirror://group?'.length))
        const gid = q.get('gid') ?? `fake${crypto.randomUUID().replace(/-/g, '').slice(0, 28)}`
        // The fake owner approves instantly: the group appears with me as a member.
        const owner: GroupMember = { fp: fp('fp-owner-1122334455667788'), name: 'Group owner' }
        fakeGroups.set(gid, {
          name: q.get('name') ?? 'joined group',
          ownerFp: owner.fp,
          version: 1,
          memberList: [owner, { fp: me.fp, name: me.name }],
          entries: [],
          unread: 0,
          profile: { ...defaultProfile(), join: 'apply', public: true },
          pins: [],
          applications: [],
          applicationSeeded: true,
        })
        emit({ kind: 'group_update', gid })
        return Promise.resolve({ gid })
      },
      applications: (gid) => Promise.resolve([...groupOf(gid).applications]),
      approve: (gid, target) => {
        const g = groupOf(gid)
        const app = g.applications.find(a => a.fp === target)
        if (app === undefined) return Promise.reject(new NetworkError('no such application', NetworkErrorCode.notFound))
        g.applications = g.applications.filter(a => a.fp !== target)
        g.memberList.push({ fp: app.fp, name: app.name })
        g.version += 1
        emit({ kind: 'group_update', gid })
        return Promise.resolve()
      },
      applicationReject: (gid, target) => {
        const g = groupOf(gid)
        g.applications = g.applications.filter(a => a.fp !== target)
        emit({ kind: 'group_update', gid })
        return Promise.resolve()
      },
      invite: (gid, target) => {
        const g = groupOf(gid)
        const friend = friends.get(target)
        if (friend === undefined) return Promise.reject(new NetworkError('not a friend', NetworkErrorCode.notFriend))
        if (g.memberList.some(m => m.fp === target)) return Promise.resolve()
        g.memberList.push({ fp: friend.fp, name: friend.name })
        g.version += 1
        emit({ kind: 'group_update', gid })
        return Promise.resolve()
      },
    },
    send: (to, body, options): Promise<SendReceipt> => {
      requireIdentity()
      if (!friends.has(to)) return Promise.reject(new NetworkError('not a friend (friends.add first)', NetworkErrorCode.notFriend))
      const id = mid()
      const entry = archive(to, { dir: 'out', id, body, ts: Date.now(), status: 'sent', ...(options?.auto === true ? { auto: true as const } : {}) })
      // Peer auto-reply 600 ms later, marked `auto` (loop-guard demo).
      setTimeout(() => { deliver(to, `(auto-reply) got it: "${body.slice(0, 40)}"`, true) }, 600)
      return Promise.resolve({ id, seq: entry.seq, status: 'sent' })
    },
    typing: (to, on) => {
      if (!friends.has(to)) return Promise.reject(new NetworkError('not a friend', NetworkErrorCode.notFriend))
      void on
      return Promise.resolve()
    },
    conversation: (target, opts = {}) => {
      let entries = conversations.get(target) ?? []
      if (opts.since !== undefined) entries = entries.filter(e => e.seq > (opts.since as number))
      if (opts.limit !== undefined && opts.limit > 0 && entries.length > opts.limit) entries = entries.slice(-opts.limit)
      return Promise.resolve({ entries, typing: false })
    },
    markRead: (target) => {
      const cur = friends.get(target)
      if (cur === undefined) return Promise.reject(new NetworkError('not a friend', NetworkErrorCode.notFriend))
      friends.set(target, { ...cur, unread: 0 })
      return Promise.resolve()
    },
    presence: (fps) => Promise.resolve(Object.fromEntries(fps.map(f => [f, friends.get(f)?.online ?? false]))),
    subscribe: (listener) => {
      listeners.add(listener)
      if (!firstInboundFired) {
        firstInboundFired = true
        const delay = options.firstInboundDelayMs ?? 1500
        if (delay >= 0) {
          const alice = FAKE_FRIENDS[0]!
          setTimeout(() => { deliver(alice.fp, 'Hey, are you around? Hiking this weekend - bring your alter ego too!') }, delay)
        }
      }
      return () => { listeners.delete(listener) }
    },
    dispose: () => {
      disposed = true
      listeners.clear()
      return Promise.resolve()
    },
    debug: { inject: (from, body) => { deliver(from, body) } },
  }
}
