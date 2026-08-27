/**
 * Browser-facing HTTP API of the host half, mounted on dsh's web server
 * (`ctx.webServer.register`, prefix `/soulmirror/api/`). The client bundle
 * uses it for everything the SoulMirror page / settings / onboarding need
 * that is not a session event: identity, card, friends, pending requests,
 * read cursors, the conversation archive, presence, the debug direct send,
 * the owner → alter channel (P4: `alter.instruct {text}`, `session.latest`,
 * `session.history`), pending drafts (`drafts.list`, `drafts.decide`),
 * per-friend settings (`friends.set` with tier / protocol override), the
 * global diplomacy protocol (`protocol.get` / `protocol.set`), and a
 * Server-Sent-Events stream of live events (inbound mail, outbound archive,
 * typing, friend requests, presence, backend status, `alter` = the alter's
 * state changed, `draft` = a draft was stored / decided).
 *
 * Why not dsh's Typert remotes: those are generated build artifacts selected
 * by the web app at build time; an out-of-repo plugin cannot add one (see
 * dsh-api-remotes README "capability set is fixed by explicit build-time
 * value imports"). Why not `ctx.sessionProjections` for typing: projections
 * are pure folds over COMMITTED session events and typing must not be logged
 * (SPIKE.md §1), so there is no event to fold. This route is the plugin's own
 * channel; it is served by the same loopback web server as `/api`.
 */
import type { IncomingMessage, ServerResponse } from 'node:http'
import { join } from 'node:path'
import type { Context } from '@deepseek-ai/cordis'
import type {} from '@deepseek-ai/dsh-host-webserver'
import type { Fingerprint } from '../events.ts'
import { ProtocolFile } from '../friend-settings.ts'
import { GroupSettingsStore } from '../group-settings.ts'
import { sendAndArchive } from '../network/send.ts'
import { NetworkError, type ConversationEntry, type GroupProfile, type NetworkClient, type NetworkEvent } from '../network/types.ts'
import { isReplyTier } from '../policy.ts'
import { checkUpgrade, isValidVersion, runUpgrade, UPGRADE_REGISTRIES, upgradeRunning } from './upgrade.ts'
import type { AlterSessions, SessionsEvent } from '../sessions/index.ts'
import type { SoulmirrorSettings } from '../settings.ts'

export const API_PREFIX = '/soulmirror/api/'

/**
 * What the SSE stream carries: every NetworkEvent of the backend plus the
 * sessions plugin's own frames (`outbound`, `alter`, `draft`).
 */
export type ApiFrame = NetworkEvent | SessionsEvent
  | { kind: 'group_outbound'; gid: string; entry: ConversationEntry }

export interface ApiOptions {
  readonly client: NetworkClient
  readonly home: string
  readonly settingsNamespace: string
  /** Resolved lazily: the sessions plugin may mount after the network plugin. */
  readonly sessions: () => AlterSessions | undefined
  /** Live settings (the alter fields apply without restart). */
  readonly settings: () => SoulmirrorSettings
  readonly log: (level: 'info' | 'warn' | 'error', message: string) => void
}

type Json = Record<string, unknown>

function readJson(req: IncomingMessage, limit = 256 * 1024): Promise<Json> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = []
    let size = 0
    req.on('data', (chunk: Buffer) => {
      size += chunk.length
      if (size > limit) {
        reject(new Error('request body too large'))
        req.destroy()
        return
      }
      chunks.push(chunk)
    })
    req.on('end', () => {
      if (chunks.length === 0) {
        resolve({})
        return
      }
      try {
        const parsed: unknown = JSON.parse(Buffer.concat(chunks).toString('utf8'))
        resolve(typeof parsed === 'object' && parsed !== null ? parsed as Json : {})
      } catch (error: unknown) {
        reject(error instanceof Error ? error : new Error(String(error)))
      }
    })
    req.on('error', reject)
  })
}

function send(res: ServerResponse, status: number, body: unknown): void {
  const payload = JSON.stringify(body)
  res.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'cache-control': 'no-store',
    'content-length': Buffer.byteLength(payload),
  })
  res.end(payload)
}

function errorBody(error: unknown): Json {
  if (error instanceof NetworkError) return { error: { code: error.code, message: error.message } }
  return { error: { code: -32603, message: error instanceof Error ? error.message : String(error) } }
}

const bad = (message: string): { status: number; body: unknown } => ({ status: 400, body: { error: { code: -32602, message } } })
const text = (value: unknown): string | undefined => typeof value === 'string' && value.trim() !== '' ? value.trim() : undefined
const num = (value: unknown): number | undefined => {
  if (typeof value === 'number' && Number.isFinite(value)) return value
  if (typeof value === 'string' && value.trim() !== '' && Number.isFinite(Number(value))) return Number(value)
  return undefined
}
/** Parse a memory scope from the wire: { kind, name?/fp?/gid? }. */
const memoryScope = (value: unknown): { kind: 'global' } | { kind: 'agent'; name: string } | { kind: 'shared-friend'; fp: string } | { kind: 'shared-group'; gid: string } | undefined => {
  if (typeof value !== 'object' || value === null) return undefined
  const v = value as Record<string, unknown>
  const kind = text(v['kind'])
  if (kind === 'global') return { kind: 'global' }
  if (kind === 'agent') { const name = text(v['name']); return name === undefined ? undefined : { kind: 'agent', name } }
  if (kind === 'shared-friend') { const fp = text(v['fp']); return fp === undefined ? undefined : { kind: 'shared-friend', fp } }
  if (kind === 'shared-group') { const gid = text(v['gid']); return gid === undefined ? undefined : { kind: 'shared-group', gid } }
  return undefined
}
/** `fps` as a JSON array (POST) or a comma-separated query value (GET). */
const fpList = (value: unknown): string[] => {
  if (Array.isArray(value)) return value.filter((v): v is string => typeof v === 'string' && v !== '')
  if (typeof value === 'string') return value.split(',').map(v => v.trim()).filter(v => v !== '')
  return []
}

/** GET routes whose query string stands in for the JSON body (`?fp=…&since=…&limit=…`, `?fps=a,b`). */
const QUERY_ROUTES = new Set(['state', 'conversation.get', 'presence', 'session.latest', 'session.history', 'protocol.get', 'drafts.list', 'group.get', 'group.conversation', 'group.applications', 'group.settings', 'agents.list', 'agent.history', 'upgrade.check'])

/** The `profile` body field as a camelCase {@link GroupProfile}, when it is a plausible object. */
function profileOf(value: unknown): GroupProfile | undefined {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return undefined
  const p = value as Record<string, unknown>
  return { ...p, speakHumans: p['speakHumans'] === true, speakAgents: p['speakAgents'] === true } as GroupProfile
}

export interface ApiHandler {
  (req: IncomingMessage, res: ServerResponse): Promise<void>
  /** Push one frame to every SSE client (the sessions plugin's events are forwarded through this). */
  broadcast(frame: ApiFrame): void
  dispose(): void
}

export function createApiHandler(options: ApiOptions): ApiHandler {
  const { client } = options
  const sseClients = new Set<ServerResponse>()
  const protocol = ProtocolFile.at(join(options.home, 'a2a'))
  const groupSettings = GroupSettingsStore.at(options.home)

  const broadcast = (event: ApiFrame): void => {
    if (sseClients.size === 0) return
    const frame = `event: ${event.kind}\ndata: ${JSON.stringify(event)}\n\n`
    for (const res of sseClients) {
      try {
        res.write(frame)
      } catch {
        sseClients.delete(res)
      }
    }
  }
  const unsubscribe = client.subscribe(broadcast)

  /** Direct send through the peer (the settings' debug "send as myself"), broadcast as `outbound` to every SSE client. */
  const sendDirect = async (fp: Fingerprint, body: string): Promise<{ entry: ConversationEntry; receipt: { id: string; seq?: number; status: string } }> => {
    const result = await sendAndArchive(client, fp, body)
    broadcast({ kind: 'outbound', fp, entry: result.entry })
    return result
  }

  /** The friend row as the browser sees it: the peer's record + the plugin's tier + pending draft count. */
  const friendRow = (friend: Record<string, unknown>, sessions: AlterSessions | undefined): Record<string, unknown> => {
    const fp = friend['fp'] as Fingerprint
    const tier = sessions?.tierOf(fp) ?? options.settings().defaultTier
    const explicit = sessions?.tierStored(fp) !== undefined
    const drafts = sessions?.drafts.count(fp) ?? 0
    const muted = sessions?.friendMuted(fp) === true
    return { ...friend, tier, ...(explicit ? { tierExplicit: true } : {}), ...(drafts > 0 ? { drafts } : {}), ...(muted ? { muted: true } : {}) }
  }

  const state = async (): Promise<Json> => {
    const status = client.status()
    let identity: Json | null = null
    let friends: unknown[] = []
    let pending: unknown[] = []
    let groups: unknown[] = []
    let error: string | undefined
    const sessions = options.sessions()
    try {
      const id = await client.identity()
      if (id !== undefined) {
        identity = { fp: id.fp, name: id.name, cardUri: id.cardUri, ...(id.createdAt === undefined ? {} : { createdAt: id.createdAt }) }
        const [f, p, g] = await Promise.all([
          client.friends.list(),
          client.friends.pending(),
          client.groups.list().catch(() => [] as const),
        ])
        pending = [...p]
        groups = g.map(grp => {
          const muted = sessions?.groupMuted(grp.gid) === true
          return muted ? { ...grp, muted: true } : grp
        })
        // `friends.list` carries no presence; ask the peer (cached 10 s there) so
        // every row has `online` and the page header / dots are authoritative.
        let online: Record<string, boolean> = {}
        if (f.length > 0) {
          try {
            online = await client.presence(f.map(x => x.fp))
          } catch {
            // best effort: rows keep whatever the client folded from SSE
          }
        }
        friends = f.map(x => friendRow((online[x.fp] === undefined ? x : { ...x, online: online[x.fp] }) as unknown as Record<string, unknown>, sessions))
      }
    } catch (e: unknown) {
      error = e instanceof Error ? e.message : String(e)
    }
    const settings = options.settings()
    const alterState = sessions?.latest()
    return {
      backend: client.backend,
      status,
      home: options.home,
      settingsNamespace: options.settingsNamespace,
      identity,
      friends,
      pending,
      groups,
      drafts: sessions?.drafts.list() ?? [],
      agents: sessions === undefined ? [] : sessions.agentsInfo().map((info) => {
        const seat = sessions.agents().find(a => a.name === info.name)
        return { ...info, ...(seat === undefined ? {} : { cwd: seat.cwd, preset: seat.preset, ...(seat.prompt === undefined ? {} : { prompt: seat.prompt }), ...(seat.approval === true ? { approval: true } : {}) }) }
      }),
      alter: {
        sessionId: sessions?.sessionId() ?? null,
        status: alterState?.status ?? 'idle',
        defaultTier: settings.defaultTier,
        autoReplyPerHour: settings.autoReplyPerHour,
        directSend: settings.directSend,
        protocolPath: protocol.path,
        protocolExists: protocol.exists(),
        legacyFriendSessions: sessions?.legacyFriendSessions() ?? {},
      },
      ...(error === undefined ? {} : { error }),
    }
  }

  const handle = async (route: string, method: string, body: Json): Promise<{ status: number; body: unknown }> => {
    switch (route) {
      case 'state':
        return { status: 200, body: await state() }
      case 'identity.create': {
        const name = text(body['name'])
        if (name === undefined) return bad('name must not be empty')
        const id = await client.createIdentity(name)
        return { status: 200, body: { identity: id } }
      }
      case 'card.parse': {
        const uri = text(body['uri'])
        if (uri === undefined) return bad('uri must not be empty')
        return { status: 200, body: await client.parseCard(uri) }
      }
      case 'friends.add': {
        const cardUri = text(body['card_uri'])
        if (cardUri === undefined) return bad('card_uri must not be empty')
        const friend = await client.friends.add(cardUri, text(body['note']))
        options.log('info', `friend request sent to ${friend.name} (${friend.fp}) via settings/command`)
        return { status: 200, body: { friend } }
      }
      case 'friends.accept': {
        const id = text(body['id'])
        if (id === undefined) return bad('id must not be empty')
        const friend = await client.friends.accept(id, text(body['note']))
        options.sessions()?.noteFriend(friend)
        return { status: 200, body: { friend: friendRow(friend as unknown as Record<string, unknown>, options.sessions()) } }
      }
      case 'friends.reject': {
        const id = text(body['id'])
        if (id === undefined) return bad('id must not be empty')
        await client.friends.reject(id)
        return { status: 200, body: { ok: true } }
      }
      case 'friends.set': {
        // Note / protocol override live in the peer (friends.yaml); the tier in the plugin's dsh-friends.json.
        const fp = text(body['fp'])
        if (fp === undefined) return bad('fp must not be empty')
        const note = text(body['note'])
        const protocolOverride = typeof body['protocol'] === 'string' ? body['protocol'] : undefined
        const tierValue = body['tier']
        if (tierValue !== undefined && tierValue !== null && tierValue !== '' && !isReplyTier(tierValue)) {
          return bad('tier must be notify | draft | auto (empty = default)')
        }
        const sessions = options.sessions()
        let friend = (await client.friends.list()).find(f => f.fp === fp)
        if (friend === undefined) return { status: 404, body: { error: { code: -32002, message: 'not a friend' } } }
        if (note !== undefined || protocolOverride !== undefined) {
          friend = await client.friends.set(fp as Fingerprint, { ...(note === undefined ? {} : { remark: note }), ...(protocolOverride === undefined ? {} : { protocol: protocolOverride }) })
          sessions?.noteFriend(friend)
        }
        if (tierValue !== undefined && sessions !== undefined) {
          await sessions.setTier(fp as Fingerprint, isReplyTier(tierValue) ? tierValue : undefined)
        }
        if ((body['muted'] === true || body['muted'] === false) && sessions !== undefined) {
          await sessions.setFriendMuted(fp as Fingerprint, body['muted'] === true)
        }
        options.log('info', `friends.set ${fp}: ${[note !== undefined ? 'note' : '', protocolOverride !== undefined ? 'protocol' : '', tierValue !== undefined ? `tier=${String(tierValue)}` : ''].filter(s => s !== '').join(' ')}`)
        return { status: 200, body: { friend: friendRow(friend as unknown as Record<string, unknown>, sessions) } }
      }
      case 'friends.card': {
        const fp = text(body['fp'])
        if (fp === undefined) return bad('fp must not be empty')
        return { status: 200, body: await client.friends.card(fp as Fingerprint) }
      }
      case 'conversation.markRead': {
        const fp = text(body['fp'])
        if (fp === undefined) return bad('fp required')
        await client.markRead(fp as Fingerprint, typeof body['seq'] === 'number' ? body['seq'] : 0)
        await options.sessions()?.markRead(fp as Fingerprint)
        return { status: 200, body: { ok: true } }
      }
      case 'conversation.get': {
        const fp = text(body['fp'])
        if (fp === undefined) return bad('fp must not be empty')
        const since = num(body['since'])
        const limit = num(body['limit'])
        return { status: 200, body: await client.conversation(fp as Fingerprint, { ...(since === undefined ? {} : { since }), ...(limit === undefined ? {} : { limit }) }) }
      }
      case 'message.send': {
        // Debug only ("send as myself" in Settings): bypasses the alter.
        const fp = text(body['fp'])
        if (fp === undefined) return bad('fp must not be empty')
        const msg = typeof body['body'] === 'string' ? body['body'].replace(/\s+$/, '') : ''
        if (msg === '') return bad('body must not be empty')
        const result = await sendDirect(fp as Fingerprint, msg)
        options.log('info', `direct send to ${fp} (debug): ${result.receipt.id} (${result.receipt.status}, seq ${result.receipt.seq ?? '?'})`)
        return { status: 200, body: result }
      }
      case 'message.typing': {
        const fp = text(body['fp'])
        if (fp === undefined) return bad('fp must not be empty')
        await client.typing(fp as Fingerprint, body['on'] !== false && body['on'] !== 'false' && body['on'] !== 0)
        return { status: 200, body: { ok: true } }
      }
      case 'presence': {
        const fps = fpList(body['fps'])
        const online = await client.presence(fps as Fingerprint[])
        return { status: 200, body: { online } }
      }
      case 'alter.instruct': {
        // The owner instructs their alter: an owner user/message + a woken turn in the alter session.
        const instruction = typeof body['text'] === 'string' ? body['text'].replace(/\s+$/, '') : ''
        if (instruction === '') return bad('text must not be empty')
        const sessions = options.sessions()
        if (sessions === undefined) return { status: 503, body: { error: { code: -32603, message: 'sessions plugin not mounted' } } }
        const result = await sessions.instruct(instruction)
        options.log('info', `owner → alter: session ${result.sessionId}, message ${result.messageId}`)
        return { status: 200, body: { ...result, state: sessions.latest() ?? null } }
      }
      case 'session.latest':
        return { status: 200, body: { state: options.sessions()?.latest() ?? null } }
      case 'session.history': {
        const sessions = options.sessions()
        const limit = num(body['limit'])
        if (sessions === undefined) return { status: 200, body: { sessionId: null, status: 'idle', chat: { items: [], running: false, seq: 0 } } }
        const h = sessions.history(limit)
        return { status: 200, body: { sessionId: h.sessionId ?? null, status: h.status, chat: h.chat } }
      }
      case 'agent.instruct': {
        // The owner instructs one named seat agent directly (its own chat pane).
        const name = text(body['name'])
        if (name === undefined) return bad('name must not be empty')
        const instruction = typeof body['text'] === 'string' ? body['text'].replace(/\s+$/, '') : ''
        if (instruction === '') return bad('text must not be empty')
        const sessions = options.sessions()
        if (sessions === undefined) return { status: 503, body: { error: { code: -32603, message: 'sessions plugin not mounted' } } }
        try {
          const result = await sessions.instructAgent(name, instruction)
          options.log('info', `owner → agent "${name}": session ${result.sessionId}, message ${result.messageId}`)
          return { status: 200, body: result }
        } catch (error: unknown) {
          return bad(error instanceof Error ? error.message : String(error))
        }
      }
      case 'agent.history': {
        const name = text(body['name'])
        if (name === undefined) return bad('name must not be empty')
        const sessions = options.sessions()
        const limit = num(body['limit'])
        const gid = text(body['gid'])
        if (sessions === undefined) return { status: 200, body: { sessionId: null, status: 'idle', chat: { items: [], running: false, seq: 0 } } }
        const h = sessions.agentHistory(name, limit, gid)
        return { status: 200, body: { sessionId: h.sessionId ?? null, status: h.status, chat: h.chat } }
      }
      case 'drafts.list': {
        const fp = text(body['fp'])
        const sessions = options.sessions()
        return { status: 200, body: { drafts: sessions?.drafts.list(fp) ?? [] } }
      }
      case 'drafts.decide': {
        const id = text(body['id'])
        if (id === undefined) return bad('id must not be empty')
        const action = text(body['action'])
        const sessions = options.sessions()
        if (sessions === undefined) return { status: 503, body: { error: { code: -32603, message: 'sessions plugin not mounted' } } }
        if (action === 'approve') {
          const edited = typeof body['body'] === 'string' ? body['body'] : undefined
          const result = await sessions.decideDraft(id, { action: 'approve', ...(edited === undefined ? {} : { body: edited }) })
          return { status: 200, body: { ok: true, ...result } }
        }
        if (action === 'reject') return { status: 200, body: { ok: true, ...(await sessions.decideDraft(id, { action: 'reject' })) } }
        if (action === 'revise') {
          const feedback = text(body['feedback'])
          if (feedback === undefined) return bad('feedback must not be empty')
          return { status: 200, body: { ok: true, ...(await sessions.decideDraft(id, { action: 'revise', feedback })) } }
        }
        return bad('action must be approve | reject | revise')
      }
      case 'agents.list': {
        const sessions = options.sessions()
        return { status: 200, body: { agents: sessions?.agentsInfo() ?? [], registry: sessions?.agents() ?? [] } }
      }
      case 'agents.set': {
        // Create or update one seat agent: {name, preset?, cwd?, commanders?: [fp | '*']}.
        const sessions = options.sessions()
        if (sessions === undefined) return { status: 503, body: { error: { code: -32603, message: 'sessions plugin not mounted' } } }
        const name = text(body['name'])
        if (name === undefined) return bad('name must not be empty')
        const preset = text(body['preset'])
        const cwd = text(body['cwd'])
        const prompt = typeof body['prompt'] === 'string' ? body['prompt'].trim() : undefined
        const approval = body['approval'] === true || body['approval'] === 'true'
        try {
          const stored = await sessions.setAgent({
            name,
            ...(preset === undefined ? {} : { preset }),
            ...(cwd === undefined ? {} : { cwd }),
            ...(prompt === undefined || prompt === '' ? {} : { prompt }),
            ...(approval ? { approval: true } : {}),
          })
          options.log('info', `seat agent saved: "${stored.name}" (cwd=${stored.cwd ?? '(default)'}, preset=${stored.preset ?? 'default'})`)
          return { status: 200, body: { ok: true, agent: stored } }
        } catch (error: unknown) {
          return bad(error instanceof Error ? error.message : String(error))
        }
      }
      case 'agents.remove': {
        const sessions = options.sessions()
        if (sessions === undefined) return { status: 503, body: { error: { code: -32603, message: 'sessions plugin not mounted' } } }
        const name = text(body['name'])
        if (name === undefined) return bad('name must not be empty')
        const removed = await sessions.removeAgent(name)
        if (removed) options.log('info', `seat agent removed: "${name}" (its dsh session stays until deleted there)`)
        return { status: 200, body: { ok: true, removed } }
      }
      case 'memory.cancel': {
        const sessions = options.sessions()
        if (sessions === undefined) return { status: 503, body: { error: { code: -32603, message: 'sessions plugin not mounted' } } }
        const ids = Array.isArray(body['ids']) ? body['ids'].filter((x): x is string => typeof x === 'string') : []
        if (ids.length === 0) return bad('ids must name at least one memory')
        const removed = await sessions.cancelMemory(ids)
        options.log('info', `memory cancelled: ${removed}/${ids.length} removed`)
        return { status: 200, body: { ok: true, removed } }
      }
      case 'memory.list': {
        const sessions = options.sessions()
        if (sessions === undefined) return { status: 503, body: { error: { code: -32603, message: 'sessions plugin not mounted' } } }
        const allow = {
          ...(body['global'] === true ? { global: true } : {}),
          ...(typeof body['agent'] === 'string' && body['agent'] !== '' ? { agent: body['agent'] } : {}),
          ...(typeof body['friend'] === 'string' && body['friend'] !== '' ? { friend: body['friend'] } : {}),
          ...(typeof body['group'] === 'string' && body['group'] !== '' ? { group: body['group'] } : {}),
        }
        return { status: 200, body: { memories: sessions.memoryList(allow) } }
      }
      case 'memory.add': {
        const sessions = options.sessions()
        if (sessions === undefined) return { status: 503, body: { error: { code: -32603, message: 'sessions plugin not mounted' } } }
        const kind = text(body['kind']) ?? 'fact'
        const content = text(body['content'])
        if (content === undefined || content.trim() === '') return bad('content must not be empty')
        const scope = memoryScope(body['scope'])
        if (scope === undefined) return bad('scope must be global | agent | shared-friend | shared-group')
        try {
          const record = sessions.memoryAdd({ kind: kind as never, content, scope })
          return { status: 200, body: { memory: record } }
        } catch (e: unknown) {
          return bad(e instanceof Error ? e.message : String(e))
        }
      }
      case 'memory.update': {
        const sessions = options.sessions()
        if (sessions === undefined) return { status: 503, body: { error: { code: -32603, message: 'sessions plugin not mounted' } } }
        const uid = text(body['uid'])
        const content = text(body['content'])
        if (uid === undefined || content === undefined || content.trim() === '') return bad('uid and content required')
        // Optional scope: moving a memory between global / agent / group while editing.
        let scope: ReturnType<typeof memoryScope>
        if (body['scope'] !== undefined) {
          scope = memoryScope(body['scope'])
          if (scope === undefined) return bad('scope must be global | agent | shared-friend | shared-group')
        }
        return { status: 200, body: { ok: sessions.memoryUpdate(uid, content, scope) } }
      }
      case 'memory.remove': {
        const sessions = options.sessions()
        if (sessions === undefined) return { status: 503, body: { error: { code: -32603, message: 'sessions plugin not mounted' } } }
        const uid = text(body['uid'])
        if (uid === undefined) return bad('uid required')
        return { status: 200, body: { ok: sessions.memoryRemove(uid) } }
      }
      case 'memory.summarize': {
        // 埋点：进群（未读多）时由客户端触发，总结该群最近一段消息并提炼记忆。
        const sessions = options.sessions()
        if (sessions === undefined) return { status: 503, body: { error: { code: -32603, message: 'sessions plugin not mounted' } } }
        const gid = text(body['gid'])
        if (gid === undefined) return bad('gid required')
        sessions.memorySummarizeGroup(gid)
        return { status: 200, body: { ok: true } }
      }
      case 'group.create': {
        const name = text(body['name'])
        if (name === undefined) return bad('name must not be empty')
        const members = fpList(body['members'])
        if (members.length === 0) return bad('members must name at least one friend fingerprint')
        // The browser resolves a template into a full profile (client/group-templates.ts)
        // and sends it camelCase; a bare `template` id is stamped into the profile.
        const template = text(body['template'])
        let profile = profileOf(body['profile'])
        if (profile === undefined && template !== undefined) profile = { speakHumans: true, speakAgents: true, template }
        else if (profile !== undefined && template !== undefined && profile.template === undefined) profile = { ...profile, template }
        const group = await client.groups.create(name, members as Fingerprint[], profile)
        options.log('info', `group created: "${group.name}" (${group.gid}, ${String(group.members)} members, template ${profile?.template ?? 'default'})`)
        return { status: 200, body: { group } }
      }
      case 'group.get': {
        const gid = text(body['gid'])
        if (gid === undefined) return bad('gid must not be empty')
        return { status: 200, body: { group: await client.groups.info(gid) } }
      }
      case 'group.send': {
        // Direct send with provenance: by=owner (the composer, default) or by=alter (the sessions plugin).
        const gid = text(body['gid'])
        if (gid === undefined) return bad('gid must not be empty')
        const msg = typeof body['body'] === 'string' ? body['body'].replace(/\s+$/, '') : ''
        if (msg === '') return bad('body must not be empty')
        const by = body['by'] === 'alter' ? 'alter' as const : body['by'] === 'owner' ? 'owner' as const : undefined
        const receipt = await client.groups.send(gid, msg, { ...(by === undefined ? {} : { by }), ...(body['auto'] === true ? { auto: true } : {}) })
        let entry: ConversationEntry | undefined
        if (receipt.seq !== undefined && receipt.seq > 0) {
          const r = await client.groups.conversation(gid, { since: receipt.seq - 1, limit: 1 }).catch(() => ({ entries: [] as const }))
          entry = r.entries.find(e => e.seq === receipt.seq)
        }
        if (entry !== undefined) broadcast({ kind: 'group_outbound', gid, entry })
        if (by !== 'alter') {
          // The owner's own post may name one of their seat agents (@DevBot …) — wake it locally (own posts never fan back in).
          const s = options.sessions()
          if (s !== undefined) void s.ownGroupPost(gid, msg).catch((error: unknown) => { options.log('warn', `own-post hook failed: ${String(error)}`) })
        }
        options.log('info', `group send to ${gid}: ${receipt.id} (${receipt.status}, seq ${receipt.seq ?? '?'})`)
        return { status: 200, body: { receipt, entry: entry ?? null } }
      }
      case 'group.conversation': {
        const gid = text(body['gid'])
        if (gid === undefined) return bad('gid must not be empty')
        const since = num(body['since'])
        const limit = num(body['limit'])
        return { status: 200, body: await client.groups.conversation(gid, { ...(since === undefined ? {} : { since }), ...(limit === undefined ? {} : { limit }) }) }
      }
      case 'group.markRead': {
        const gid = text(body['gid'])
        if (gid === undefined) return bad('gid must not be empty')
        await client.groups.markRead(gid, typeof body['seq'] === 'number' ? body['seq'] : 0)
        return { status: 200, body: { ok: true } }
      }
      case 'group.leave': {
        const gid = text(body['gid'])
        if (gid === undefined) return bad('gid must not be empty')
        await client.groups.leave(gid)
        return { status: 200, body: { ok: true } }
      }
      case 'group.kick': {
        const gid = text(body['gid'])
        const fp = text(body['fp'])
        if (gid === undefined || fp === undefined) return bad('gid and fp must not be empty')
        await client.groups.kick(gid, fp as Fingerprint)
        return { status: 200, body: { ok: true } }
      }
      case 'group.setProfile': {
        const gid = text(body['gid'])
        if (gid === undefined) return bad('gid must not be empty')
        const profile = profileOf(body['profile'])
        if (profile === undefined) return bad('profile must be an object')
        await client.groups.setProfile(gid, profile)
        options.log('info', `group ${gid}: profile updated (room ${profile.room ?? 'chat'}, join ${profile.join ?? 'invite'})`)
        return { status: 200, body: { ok: true } }
      }
      case 'group.pin': {
        const gid = text(body['gid'])
        const pinBody = typeof body['body'] === 'string' ? body['body'].trim() : ''
        if (gid === undefined || pinBody === '') return bad('gid and body must not be empty')
        await client.groups.pin(gid, pinBody)
        return { status: 200, body: { ok: true } }
      }
      case 'group.unpin': {
        const gid = text(body['gid'])
        const id = text(body['id'])
        if (gid === undefined || id === undefined) return bad('gid and id must not be empty')
        await client.groups.unpin(gid, id)
        return { status: 200, body: { ok: true } }
      }
      case 'group.apply': {
        const uri = text(body['uri'])
        if (uri === undefined) return bad('uri must not be empty')
        const { gid } = await client.groups.apply(uri, text(body['note']))
        options.log('info', `applied to group ${gid} via URI`)
        return { status: 200, body: { ok: true, gid } }
      }
      case 'group.applications': {
        const gid = text(body['gid'])
        if (gid === undefined) return bad('gid must not be empty')
        return { status: 200, body: { applications: await client.groups.applications(gid) } }
      }
      case 'group.approve': {
        const gid = text(body['gid'])
        const fp = text(body['fp'])
        if (gid === undefined || fp === undefined) return bad('gid and fp must not be empty')
        await client.groups.approve(gid, fp as Fingerprint)
        return { status: 200, body: { ok: true } }
      }
      case 'group.applicationReject': {
        const gid = text(body['gid'])
        const fp = text(body['fp'])
        if (gid === undefined || fp === undefined) return bad('gid and fp must not be empty')
        await client.groups.applicationReject(gid, fp as Fingerprint)
        return { status: 200, body: { ok: true } }
      }
      case 'group.invite': {
        const gid = text(body['gid'])
        const fp = text(body['fp'])
        if (gid === undefined || fp === undefined) return bad('gid and fp must not be empty')
        await client.groups.invite(gid, fp as Fingerprint)
        return { status: 200, body: { ok: true } }
      }
      case 'group.settings': {
        // Per-group CLIENT settings (<home>/dsh-groups.json, v2 voices): GET
        // {gid} reads; POST writes voice switches — legacy {alter, mode} (the
        // composer's alter toggle), {voice: {name, on}} for one seat agent,
        // {duty: name | null} for the duty slot (who answers unmentioned
        // traffic; null clears it).
        const gid = text(body['gid'])
        if (gid === undefined) return bad('gid must not be empty')
        if (!groupSettings.isLoaded) await groupSettings.load()
        const mode = body['mode'] === 'always' ? 'always' as const : body['mode'] === 'mention' ? 'mention' as const : undefined
        const voiceRaw = body['voice'] as Record<string, unknown> | undefined
        const voice = typeof voiceRaw === 'object' && voiceRaw !== null && typeof voiceRaw['name'] === 'string' && voiceRaw['name'] !== ''
          ? {
              name: voiceRaw['name'],
              on: voiceRaw['on'] === true || voiceRaw['on'] === 'true',
              // Per-group whitelist of the voice ('*' = any member); omitted = keep the stored list.
              ...(Array.isArray(voiceRaw['commanders']) ? { commanders: fpList(voiceRaw['commanders']) } : {}),
            }
          : undefined
        const duty = body['duty'] === null || body['duty'] === '' ? null : typeof body['duty'] === 'string' ? body['duty'] : undefined
        const muted = body['muted'] === true || body['muted'] === false ? body['muted'] === true : undefined
        // Single-writer rule: go through the sessions plugin's store when it is
        // mounted (its in-memory copy is what the routing reads); this route's
        // own store is only the fallback for a sessions-less composition.
        const sessions = options.sessions()
        if (method === 'POST' && (body['alter'] !== undefined || mode !== undefined || voice !== undefined || duty !== undefined || muted !== undefined)) {
          const patch = {
            ...(body['alter'] === undefined ? {} : { alter: body['alter'] === true || body['alter'] === 'true' }),
            ...(mode === undefined ? {} : { mode }),
            ...(voice === undefined ? {} : { voice }),
            ...(duty === undefined ? {} : { duty }),
            ...(muted === undefined ? {} : { muted }),
          }
          const settings = sessions !== undefined ? await sessions.setGroupVoices(gid, patch) : await groupSettings.set(gid, patch)
          options.log('info', `group ${gid}: voices [${Object.keys(settings.voices ?? {}).join(', ')}]${settings.duty === undefined ? '' : ` · duty ${settings.duty}`}`)
          return { status: 200, body: { ok: true, settings } }
        }
        return { status: 200, body: { settings: sessions !== undefined ? sessions.groupVoices(gid) : groupSettings.get(gid) } }
      }
      case 'upgrade.check': {
        // Latest published soulnet-dsh vs. the running version (registry order
        // npmjs → npmmirror, 5 s each; see ./upgrade.ts).
        return { status: 200, body: await checkUpgrade() }
      }
      case 'upgrade.run': {
        // One-click self-upgrade: pnpm add soulnet-dsh@<version> in the dsh
        // profile directory. Only THIS package, only a strict-semver version —
        // nothing else ever reaches the pnpm command line. On success the peer
        // is asked to relaunch the host (host.relaunch); when it can, the host
        // exits shortly after this response and the browser waits for the
        // restarted server. An old peer without the method = restarting:false,
        // the UI shows the manual-restart hint instead.
        const version = text(body['version'])
        if (version === undefined || !isValidVersion(version)) return bad('version must be strict semver (X.Y.Z)')
        const registry = text(body['registry'])
        if (registry !== undefined && !(UPGRADE_REGISTRIES as readonly string[]).includes(registry)) return bad('registry not in the allowed list')
        if (upgradeRunning()) return { status: 409, body: { error: { code: -32000, message: 'an upgrade is already running' } } }
        options.log('info', `upgrade.run: installing soulnet-dsh@${version}${registry === undefined ? '' : ` via ${registry}`}`)
        const result = await runUpgrade(version, registry === undefined ? {} : { registry })
        if (!result.ok) {
          options.log('error', `upgrade.run failed (exit ${result.exitCode}); output tail:
${result.output.slice(-1500)}`)
          return { status: 200, body: { ...result, restarting: false } }
        }
        options.log('info', `upgrade.run: soulnet-dsh@${version} installed into ${result.profileDir}`)
        let restarting = false
        if (client.relaunch !== undefined) {
          try {
            await client.relaunch({ pid: process.pid, exec: process.execPath, argv: process.argv.slice(1), cwd: process.cwd() })
            restarting = true
            options.log('info', 'upgrade.run: relaunch helper armed — this host exits in 500 ms and comes back on the new version')
          } catch (error: unknown) {
            options.log('warn', `upgrade.run: peer relaunch unavailable (${error instanceof Error ? error.message : String(error)}); ask the user to restart dsh`)
          }
        }
        if (restarting) {
          // Let this HTTP response reach the browser first, then exit; the
          // detached helper waits for us (and the old peer) and starts dsh again.
          setTimeout(() => { process.exit(0) }, 500)
        }
        return { status: 200, body: { ...result, restarting } }
      }
      case 'protocol.get':
        return { status: 200, body: { text: protocol.read(), path: protocol.path, exists: protocol.exists() } }
      case 'protocol.set': {
        if (typeof body['text'] !== 'string') return bad('text must be a string')
        protocol.write(body['text'])
        options.log('info', `diplomacy protocol saved (${body['text'].length} chars) → ${protocol.path}`)
        return { status: 200, body: { ok: true, text: protocol.read(), path: protocol.path } }
      }
      default:
        return { status: 404, body: { error: { code: -32601, message: `unknown route ${method} ${route}` } } }
    }
  }

  const handler = (async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    const url = new URL(req.url ?? '/', 'http://localhost')
    const route = url.pathname.startsWith(API_PREFIX) ? url.pathname.slice(API_PREFIX.length) : ''
    if (route === 'events' && req.method === 'GET') {
      res.writeHead(200, {
        'content-type': 'text/event-stream',
        'cache-control': 'no-store',
        connection: 'keep-alive',
      })
      res.write(`event: status\ndata: ${JSON.stringify({ kind: 'status', status: client.status() })}\n\n`)
      sseClients.add(res)
      const keepAlive = setInterval(() => {
        try {
          res.write(': keep-alive\n\n')
        } catch {
          clearInterval(keepAlive)
        }
      }, 25_000)
      keepAlive.unref?.()
      req.on('close', () => {
        clearInterval(keepAlive)
        sseClients.delete(res)
      })
      return
    }
    if (req.method !== 'POST' && !(req.method === 'GET' && QUERY_ROUTES.has(route))) {
      send(res, 405, { error: { code: -32600, message: 'use POST with application/json (GET only for state / conversation.get / presence / session.latest / session.history / drafts.list / protocol.get / events)' } })
      return
    }
    try {
      const body: Json = req.method === 'POST' ? await readJson(req) : Object.fromEntries(url.searchParams.entries())
      const result = await handle(route, req.method ?? 'GET', body)
      send(res, result.status, result.body)
    } catch (error: unknown) {
      options.log('warn', `api ${route} failed: ${String(error)}`)
      send(res, error instanceof NetworkError ? 400 : 500, errorBody(error))
    }
  }) as ApiHandler
  handler.broadcast = broadcast
  handler.dispose = () => {
    unsubscribe()
    for (const res of sseClients) {
      try {
        res.end()
      } catch {
        // ignore
      }
    }
    sseClients.clear()
  }
  return handler
}

/** Mount the API on `ctx.webServer` when (and for as long as) that service exists. */
export function mountApi(ctx: Context, options: ApiOptions): void {
  ctx.inject(['webServer'], (wctx) => {
    const handler = createApiHandler(options)
    // The web server matches a prefix route as `/path` or `/path/...`, so register it without the trailing slash.
    const dispose = wctx.webServer.register({ kind: 'prefix', path: API_PREFIX.slice(0, -1), handler })
    options.log('info', `browser API mounted at ${API_PREFIX} (port ${wctx.webServer.port})`)
    // The sessions plugin's live events (alter state, the alter's own sends,
    // drafts) → SSE frames, for as long as both services exist.
    wctx.inject(['soulmirrorSessions'], (sctx) => {
      const off = sctx.soulmirrorSessions.on((event: SessionsEvent) => { handler.broadcast(event) })
      sctx.effect(() => off, 'soulmirror: alter events → SSE')
    })
    wctx.effect(() => () => {
      dispose()
      handler.dispose()
    }, 'soulmirror: browser API')
  })
}
