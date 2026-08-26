/**
 * `soulnet` backend: spawns the soulnet light peer (Go, ../cmd/soulnet) and
 * drives it over line-delimited JSON-RPC 2.0 on stdio (protocol: cmd/soulnet/
 * README.md, `initialize.protocol === "soulnet/1"`).
 *
 *  - request/response by id, notifications → `subscribe` listeners;
 *  - the process is restarted with exponential backoff when it dies;
 *  - `dispose()` sends `shutdown`, then kills the process if it lingers;
 *  - calls issued while the peer is (re)starting wait for it up to the request
 *    timeout instead of failing immediately.
 *
 * Binary lookup (`resolveSoulnetBinary` / `locateSoulnetBinary`), in order:
 *   1. the explicit `peerBinary` setting (absolute path, or a bare name looked up on PATH);
 *   2. the platform package `soulnet-peer-<os>-<arch>` installed next to
 *      this plugin as an optional dependency (`require.resolve('<pkg>/package.json')`
 *      from this file and from its realpath, so pnpm's symlinked virtual store and
 *      the hoisted layout both work) -> `<pkg>/bin/soulnet[.exe]`;
 *   3. `soulnet` on PATH;
 *   4. `<plugin dir>/bin/soulnet[.exe]` (a hand-dropped binary for development).
 * The winner and its source are reported in `BackendStatus.binary` / `binarySource`.
 */
import { spawn, type ChildProcess } from 'node:child_process'
import { accessSync, appendFileSync, chmodSync, constants, realpathSync } from 'node:fs'
import { createRequire } from 'node:module'
import { homedir } from 'node:os'
import { delimiter, dirname, isAbsolute, join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import type { A2AMessageId, Fingerprint } from '../events.ts'
import { JsonRpcEndpoint, JsonRpcError, JSONRPC_CLOSED, JSONRPC_TIMEOUT } from './jsonrpc.ts'
import {
  NetworkError,
  NetworkErrorCode,
  type BackendStatus,
  type ConversationEntry,
  type Friend,
  type Group,
  type GroupApplication,
  type GroupInfo,
  type GroupPin,
  type GroupProfile,
  type Identity,
  type NetworkClient,
  type NetworkEvent,
  type PendingRequest,
  type SendReceipt,
} from './types.ts'

export const DEFAULT_RELAY = 'https://relay.startupworld.cn'
export const SOULNET_PROTOCOL = 'soulnet/1'

export type SoulnetLogger = (level: 'info' | 'warn' | 'error', message: string) => void

export interface SoulnetSpawnRequest {
  readonly binary: string
  readonly args: readonly string[]
}

export interface SoulnetClientOptions {
  /** Data directory passed as `--home` (`a2a/` lives underneath). */
  readonly home: string
  /** Relay URL passed as `--relay` (only used when the identity is created). */
  readonly relay?: string
  /** Create the identity with this name on first start (`initialize {name}`); empty = wait for the host. */
  readonly displayName?: string
  /** Explicit binary path; when absent {@link resolveSoulnetBinary} runs. */
  readonly peerBinary?: string
  /** Per-request timeout (default 30 s; `message.send` with the relay down can take a while). */
  readonly requestTimeoutMs?: number
  /** Restart backoff (ms). Defaults: 500 → ×2 → max 30 000. */
  readonly backoff?: { readonly initialMs?: number; readonly maxMs?: number; readonly factor?: number }
  /** Test seam: replace `child_process.spawn`. */
  readonly spawn?: (request: SoulnetSpawnRequest) => ChildProcess
  readonly logger?: SoulnetLogger
  /** Extra env for the child (merged over process.env). */
  readonly env?: Record<string, string>
}

/** Default home: `$SOULNET_HOME`, else `~/.soulnet` (same rule as the binary itself). */
export function defaultSoulnetHome(env: NodeJS.ProcessEnv = process.env): string {
  const fromEnv = env['SOULNET_HOME']
  if (fromEnv !== undefined && fromEnv !== '') return fromEnv
  return join(homedir(), '.soulnet')
}

function isExecutable(path: string): boolean {
  try {
    accessSync(path, constants.F_OK)
    return true
  } catch {
    return false
  }
}

/** Where the binary came from (reported in `BackendStatus.binarySource`). */
export type SoulnetBinarySource = 'setting' | 'platform-package' | 'path' | 'plugin-bin'

export interface SoulnetBinaryLocation {
  readonly path: string
  readonly source: SoulnetBinarySource
}

/** Prefix of the per-platform binary packages on npm (`soulnet-peer-<os>-<arch>`). */
export const PLATFORM_PACKAGE_PREFIX = 'soulnet-peer-'
/** The `<process.platform>-<process.arch>` pairs a platform package exists for (must match dsh/packages/soulnet-*). */
export const PLATFORM_PACKAGE_TARGETS: readonly string[] = ['win32-x64', 'darwin-arm64', 'darwin-x64', 'linux-x64', 'linux-arm64']
/**
 * How the `<os>` part of the package name is spelled when it differs from `process.platform`.
 * npm's spam filter rejects new unscoped names ending in `-win32-x64`, so the Windows
 * package is published as `soulnet-peer-windows-x64` (the workspace directory keeps `win32`).
 */
export const PLATFORM_PACKAGE_OS_NAMES: Readonly<Record<string, string>> = { win32: 'windows' }

/** `soulnet-peer-<os>-<arch>` for a supported pair, else `undefined`. */
export function platformPackageName(platform: NodeJS.Platform = process.platform, arch: string = process.arch): string | undefined {
  const target = `${platform}-${arch}`
  if (!PLATFORM_PACKAGE_TARGETS.includes(target)) return undefined
  return `${PLATFORM_PACKAGE_PREFIX}${PLATFORM_PACKAGE_OS_NAMES[platform] ?? platform}-${arch}`
}

/**
 * Resolve an installed package's directory from this plugin's location: first
 * from this file's URL (dsh loads lib/index.js from the profile's node_modules,
 * hoisted or symlinked), then from its realpath (pnpm's isolated virtual store
 * keeps the optional dependency next to the REAL plugin directory).
 */
function defaultResolvePackageDir(name: string): string | undefined {
  const bases: string[] = [import.meta.url]
  try {
    const real = realpathSync(fileURLToPath(import.meta.url))
    const realUrl = pathToFileURL(real).href
    if (realUrl !== import.meta.url) bases.push(realUrl)
  } catch {
    // not a file URL (bundled in memory) or unreadable; the first base still works
  }
  for (const base of bases) {
    try {
      return dirname(createRequire(base).resolve(`${name}/package.json`))
    } catch {
      // not installed from this base
    }
  }
  return undefined
}

function ensureExecutable(path: string, platform: NodeJS.Platform): void {
  if (platform === 'win32') return
  try {
    accessSync(path, constants.X_OK)
  } catch {
    try {
      chmodSync(path, 0o755) // tarballs packed on Windows lose the mode bit
    } catch {
      // read-only install: spawn will report the real error
    }
  }
}

export interface ResolveSoulnetBinaryOptions {
  /** `process.arch` by default. */
  readonly arch?: string
  /** Test seam: package name -> installed package directory (default: `require.resolve` next to this file). */
  readonly resolvePackageDir?: (name: string) => string | undefined
}

/**
 * Find the `soulnet` binary (order documented at the top of this file) and say
 * where it came from. `undefined` when nothing was found (the caller reports a
 * clear error).
 */
export function locateSoulnetBinary(
  explicit: string | undefined,
  env: NodeJS.ProcessEnv = process.env,
  platform: NodeJS.Platform = process.platform,
  options: ResolveSoulnetBinaryOptions = {},
): SoulnetBinaryLocation | undefined {
  const names = platform === 'win32' ? ['soulnet.exe', 'soulnet'] : ['soulnet']
  if (explicit !== undefined && explicit.trim() !== '') {
    const candidate = explicit.trim()
    // A bare name (no separator) is looked up on PATH like the default.
    if (isAbsolute(candidate) || candidate.includes('/') || candidate.includes('\\')) return { path: candidate, source: 'setting' }
    for (const dir of (env['PATH'] ?? '').split(delimiter)) {
      if (dir === '') continue
      const full = join(dir, candidate)
      if (isExecutable(full)) return { path: full, source: 'setting' }
      if (platform === 'win32' && !candidate.toLowerCase().endsWith('.exe') && isExecutable(`${full}.exe`)) return { path: `${full}.exe`, source: 'setting' }
    }
    return { path: candidate, source: 'setting' }
  }
  // 2. the platform package installed as an optional dependency of this plugin
  const pkgName = platformPackageName(platform, options.arch ?? process.arch)
  if (pkgName !== undefined) {
    const dir = (options.resolvePackageDir ?? defaultResolvePackageDir)(pkgName)
    if (dir !== undefined) {
      for (const name of names) {
        const full = join(dir, 'bin', name)
        if (isExecutable(full)) {
          ensureExecutable(full, platform)
          return { path: full, source: 'platform-package' }
        }
      }
    }
  }
  // 3. PATH
  for (const dir of (env['PATH'] ?? '').split(delimiter)) {
    if (dir === '') continue
    for (const name of names) {
      const full = join(dir, name)
      if (isExecutable(full)) return { path: full, source: 'path' }
    }
  }
  // 4. lib/index.js -> ../bin ; src/network/soulnet.ts -> ../../bin
  const here = dirname(fileURLToPath(import.meta.url))
  for (const root of [join(here, '..'), join(here, '..', '..')]) {
    for (const name of names) {
      const full = join(root, 'bin', name)
      if (isExecutable(full)) return { path: full, source: 'plugin-bin' }
    }
  }
  return undefined
}

/** Path-only form of {@link locateSoulnetBinary}. */
export function resolveSoulnetBinary(
  explicit: string | undefined,
  env: NodeJS.ProcessEnv = process.env,
  platform: NodeJS.Platform = process.platform,
  options: ResolveSoulnetBinaryOptions = {},
): string | undefined {
  return locateSoulnetBinary(explicit, env, platform, options)?.path
}

const fp = (s: string): Fingerprint => s as Fingerprint
const mid = (s: string): A2AMessageId => s as A2AMessageId

function toMs(value: unknown): number {
  if (typeof value === 'number') return value
  if (typeof value === 'string') {
    const parsed = Date.parse(value)
    if (!Number.isNaN(parsed)) return parsed
  }
  return Date.now()
}

function str(value: unknown, fallback = ''): string {
  return typeof value === 'string' ? value : fallback
}

function shortFp(value: string): string {
  return value.length > 12 ? `${value.slice(0, 12)}…` : value
}

// ——— wire shapes (subset of what soulnet returns; see cmd/soulnet/rpc.go) ———

interface WireIdentity { name?: string; fingerprint?: string; created_at?: string }
interface WireCard { name?: string }
interface WireMessage {
  id?: string; from?: string; to?: string; ts?: string; type?: string; body?: string; auto?: boolean; by?: string
  agent?: string; artifact_name?: string; card?: WireCard
}
interface WireFriend {
  fingerprint?: string; note?: string; protocol?: string; card?: WireCard; added_at?: string
  count?: number; unread?: number; last?: WireMessage; typing?: boolean
}
interface WirePending { id?: string; peer?: string; incoming?: WireMessage; created_at?: string }
interface WireEntry extends WireMessage { seq?: number; dir?: string; status?: string }
interface WireGroup {
  gid?: string; name?: string; owner_fp?: string; mine?: boolean; version?: number
  members?: number; unread?: number; count?: number; last_ts?: number; last_body?: string
  /** Governance profile (§14.7): the Go `a2a.GroupProfile` as a JSON object or a JSON string. */
  profile?: unknown
}
interface WireGroupPin { id?: string; from?: string; ts?: unknown; body?: string }
interface WireGroupApplication { fp?: string; name?: string; note?: string; ts?: unknown }
interface WireGroupInfo extends WireGroup {
  member_list?: { fp?: string; name?: string; agents?: unknown[] }[]
  pins?: WireGroupPin[]
  my_role?: string
  applications?: WireGroupApplication[]
}

/**
 * Map a wire `profile` value (the Go `a2a.GroupProfile` snake_case JSON, as an
 * object or a JSON string) to the camelCase {@link GroupProfile}; `undefined`
 * for absent/unparseable values (a legacy group without a profile).
 */
export function profileFromWire(value: unknown): GroupProfile | undefined {
  let raw: unknown = value
  if (typeof raw === 'string') {
    if (raw.trim() === '') return undefined
    try {
      raw = JSON.parse(raw)
    } catch {
      return undefined
    }
  }
  if (typeof raw !== 'object' || raw === null) return undefined
  const p = raw as Record<string, unknown>
  const s = (v: unknown): string | undefined => typeof v === 'string' && v !== '' ? v : undefined
  const n = (v: unknown): number | undefined => typeof v === 'number' && Number.isFinite(v) && v !== 0 ? v : undefined
  const list = (v: unknown): string[] | undefined => Array.isArray(v) && v.length > 0 ? v.filter((x): x is string => typeof x === 'string') : undefined
  const speakWho = s(p['speak_who'])
  const join = s(p['join'])
  const wake = s(p['agent_wake'])
  const tier = s(p['agent_tier'])
  return {
    ...(s(p['template']) === undefined ? {} : { template: s(p['template'])! }),
    ...(s(p['room']) === undefined ? {} : { room: s(p['room'])! }),
    speakHumans: p['speak_humans'] === true,
    speakAgents: p['speak_agents'] === true,
    ...(speakWho === 'all' || speakWho === 'owner' || speakWho === 'admins' ? { speakWho } : {}),
    ...(join === 'invite' || join === 'apply' || join === 'open' ? { join } : {}),
    ...(wake === 'mention' || wake === 'always' || wake === 'never' ? { agentWake: wake } : {}),
    ...(tier === 'notify' || tier === 'draft' || tier === 'auto' ? { agentTier: tier } : {}),
    ...(n(p['auto_per_hour']) === undefined ? {} : { autoPerHour: n(p['auto_per_hour'])! }),
    ...(n(p['agent_rounds']) === undefined ? {} : { agentRounds: n(p['agent_rounds'])! }),
    ...(list(p['admins']) === undefined ? {} : { admins: list(p['admins'])! }),
    ...(p['public'] === true ? { public: true } : {}),
    ...(list(p['tags']) === undefined ? {} : { tags: list(p['tags'])! }),
    ...(s(p['rules']) === undefined ? {} : { rules: s(p['rules'])! }),
  }
}

/** Map a camelCase {@link GroupProfile} to the snake_case wire object the peer expects. */
export function profileToWire(p: GroupProfile): Record<string, unknown> {
  return {
    ...(p.template === undefined ? {} : { template: p.template }),
    ...(p.room === undefined ? {} : { room: p.room }),
    speak_humans: p.speakHumans,
    speak_agents: p.speakAgents,
    ...(p.speakWho === undefined ? {} : { speak_who: p.speakWho }),
    ...(p.join === undefined ? {} : { join: p.join }),
    ...(p.agentWake === undefined ? {} : { agent_wake: p.agentWake }),
    ...(p.agentTier === undefined ? {} : { agent_tier: p.agentTier }),
    ...(p.autoPerHour === undefined ? {} : { auto_per_hour: p.autoPerHour }),
    ...(p.agentRounds === undefined ? {} : { agent_rounds: p.agentRounds }),
    ...(p.admins === undefined ? {} : { admins: [...p.admins] }),
    ...(p.public === undefined ? {} : { public: p.public }),
    ...(p.tags === undefined ? {} : { tags: [...p.tags] }),
    ...(p.rules === undefined ? {} : { rules: p.rules }),
  }
}

export function friendFromWire(w: WireFriend): Friend {
  const fingerprint = str(w.fingerprint)
  const note = str(w.note)
  const cardName = str(w.card?.name)
  const name = note !== '' ? note : cardName !== '' ? cardName : shortFp(fingerprint)
  return {
    fp: fp(fingerprint),
    name,
    ...(note === '' ? {} : { remark: note }),
    ...(cardName === '' ? {} : { cardName }),
    ...(str(w.protocol) === '' ? {} : { protocol: str(w.protocol) }),
    unread: typeof w.unread === 'number' ? w.unread : 0,
    count: typeof w.count === 'number' ? w.count : 0,
    ...(w.last?.ts === undefined ? {} : { lastTs: toMs(w.last.ts) }),
    ...(w.last?.body === undefined ? {} : { lastBody: w.last.body }),
    ...(w.typing === true ? { typing: true } : {}),
    ...(w.added_at === undefined ? {} : { addedAt: w.added_at }),
  }
}

export function pendingFromWire(w: WirePending): PendingRequest {
  const peer = str(w.peer)
  const cardName = str(w.incoming?.card?.name)
  return {
    id: str(w.id),
    fp: fp(peer),
    name: cardName !== '' ? cardName : shortFp(peer),
    greeting: str(w.incoming?.body),
    ...(w.created_at === undefined ? {} : { createdAt: w.created_at }),
  }
}

function entryFromWire(w: WireEntry): ConversationEntry {
  return {
    seq: typeof w.seq === 'number' ? w.seq : 0,
    dir: w.dir === 'out' ? 'out' : 'in',
    id: mid(str(w.id)),
    body: str(w.body),
    ts: toMs(w.ts),
    ...(str(w.from) === '' ? {} : { from: str(w.from) }),
    ...(w.type === undefined || w.type === '' ? {} : { type: w.type }),
    ...(w.auto === true ? { auto: true as const } : {}),
    ...(w.by === 'owner' || w.by === 'alter' ? { by: w.by } : {}),
    ...(typeof w.agent === 'string' && w.agent !== '' ? { agent: w.agent } : {}),
    ...(w.status === undefined || w.status === '' ? {} : { status: w.status }),
    ...(w.artifact_name === undefined || w.artifact_name === '' ? {} : { artifactName: w.artifact_name }),
  }
}

export function groupFromWire(w: WireGroup): Group {
  const profile = profileFromWire(w.profile)
  return {
    gid: str(w.gid),
    name: str(w.name),
    ownerFp: fp(str(w.owner_fp)),
    mine: w.mine === true,
    version: typeof w.version === 'number' ? w.version : 1,
    members: typeof w.members === 'number' ? w.members : 0,
    unread: typeof w.unread === 'number' ? w.unread : 0,
    count: typeof w.count === 'number' ? w.count : 0,
    ...(typeof w.last_ts === 'number' && w.last_ts > 0 ? { lastTs: w.last_ts } : {}),
    ...(str(w.last_body) === '' ? {} : { lastBody: str(w.last_body) }),
    ...(profile === undefined ? {} : { profile }),
  }
}

function pinFromWire(w: WireGroupPin): GroupPin {
  return { id: str(w.id), from: fp(str(w.from)), ts: toMs(w.ts), body: str(w.body) }
}

function applicationFromWire(w: WireGroupApplication): GroupApplication {
  const applicant = str(w.fp)
  return {
    fp: fp(applicant),
    name: str(w.name) !== '' ? str(w.name) : shortFp(applicant),
    note: str(w.note),
    ...(w.ts === undefined ? {} : { ts: toMs(w.ts) }),
  }
}

export function groupInfoFromWire(w: WireGroupInfo): GroupInfo {
  return {
    ...groupFromWire(w),
    memberList: (w.member_list ?? []).map(m => ({
      fp: fp(str(m.fp)),
      name: str(m.name) !== '' ? str(m.name) : shortFp(str(m.fp)),
      ...(Array.isArray(m.agents) && m.agents.length > 0 ? { agents: m.agents.filter((a): a is string => typeof a === 'string' && a !== '') } : {}),
    })),
    pins: (w.pins ?? []).map(pinFromWire),
    myRole: w.my_role === 'owner' || w.my_role === 'admin' ? w.my_role : w.mine === true ? 'owner' : 'member',
    ...(w.applications === undefined ? {} : { applications: w.applications.map(applicationFromWire) }),
  }
}

function toNetworkError(error: unknown, method: string): NetworkError {
  if (error instanceof NetworkError) return error
  if (error instanceof JsonRpcError) {
    const code = error.code === JSONRPC_CLOSED || error.code === JSONRPC_TIMEOUT ? NetworkErrorCode.peerUnavailable : error.code
    return new NetworkError(error.message, code, error.data)
  }
  return new NetworkError(`${method}: ${String(error)}`, -32603)
}

/**
 * Create the soulnet-backed NetworkClient. The process is spawned lazily on
 * the first call or on `start()`; `dispose()` stops it.
 */
export function createSoulnetNetworkClient(options: SoulnetClientOptions): NetworkClient & { start(): void } {
  const log: SoulnetLogger = options.logger ?? (() => {})
  const relay = options.relay !== undefined && options.relay.trim() !== '' ? options.relay.trim() : DEFAULT_RELAY
  const requestTimeoutMs = options.requestTimeoutMs ?? 30_000
  const backoffInitial = options.backoff?.initialMs ?? 500
  const backoffMax = options.backoff?.maxMs ?? 30_000
  const backoffFactor = options.backoff?.factor ?? 2

  const listeners = new Set<(event: NetworkEvent) => void>()
  let child: ChildProcess | undefined
  let endpoint: JsonRpcEndpoint | undefined
  let disposed = false
  let started = false
  let restarts = 0
  let backoffMs = backoffInitial
  let restartTimer: NodeJS.Timeout | undefined
  let status: BackendStatus = { backend: 'soulnet', state: 'stopped', restarts: 0, relay, home: options.home }
  let cachedCardUri: string | undefined
  const friendNames = new Map<string, string>()

  // Waiters for "endpoint is up" (calls made while (re)starting).
  let readyWaiters: { resolve: (endpoint: JsonRpcEndpoint) => void; reject: (error: Error) => void }[] = []

  const emit = (event: NetworkEvent): void => {
    for (const listener of listeners) {
      try {
        listener(event)
      } catch (error: unknown) {
        log('warn', `network listener failed: ${String(error)}`)
      }
    }
  }
  const setStatus = (patch: Partial<BackendStatus>): void => {
    status = { ...status, ...patch }
    emit({ kind: 'status', status })
  }
  const clearError = (): void => {
    const { lastError: _dropped, ...rest } = status
    status = rest
  }

  const handleNotification = (method: string, params: unknown): void => {
    const p = (typeof params === 'object' && params !== null ? params : {}) as {
      peer?: string; seq?: number; message?: WireMessage; artifact_path?: string; artifact_name?: string
      pending_id?: string; friend?: WireFriend; on?: boolean; gid?: string
    }
    const peer = str(p.peer)
    switch (method) {
      case 'message.received': {
        const m = p.message ?? {}
        const type = str(m.type, 'text')
        const name = friendNames.get(peer) ?? shortFp(peer)
        const body = str(m.body) !== '' ? str(m.body) : type === 'app_share' ? '[app share]' : ''
        emit({
          kind: 'message',
          message: {
            id: mid(str(m.id)),
            from: fp(peer),
            name,
            body,
            ts: toMs(m.ts),
            ...(typeof p.seq === 'number' ? { seq: p.seq } : {}),
            ...(m.auto === true ? { auto: true as const } : {}),
            ...(type === 'text' ? {} : { type }),
            ...(p.artifact_path === undefined || p.artifact_path === '' ? {} : { artifactPath: p.artifact_path }),
            ...(m.artifact_name === undefined || m.artifact_name === '' ? {} : { artifactName: m.artifact_name }),
          },
        })
        return
      }
      case 'friend.request': {
        const m = p.message ?? {}
        const cardName = str(m.card?.name)
        emit({
          kind: 'friend_request',
          request: { id: str(p.pending_id), fp: fp(peer), name: cardName !== '' ? cardName : shortFp(peer), greeting: str(m.body) },
        })
        return
      }
      case 'friend.accepted': {
        const friend = friendFromWire(p.friend ?? { fingerprint: peer })
        friendNames.set(friend.fp, friend.name)
        emit({ kind: 'friend_accept', friend })
        return
      }
      case 'typing':
        emit({ kind: 'typing', fp: fp(peer), on: p.on === true })
        return
      case 'presence.changed':
        emit({ kind: 'presence', fp: fp(peer), online: p.on === true })
        return
      case 'group.message': {
        const m = p.message ?? {}
        emit({
          kind: 'group_message',
          gid: str(p.gid),
          message: {
            id: mid(str(m.id)),
            from: fp(peer),
            name: friendNames.get(peer) ?? shortFp(peer),
            body: str(m.body),
            ts: toMs(m.ts),
            ...(typeof p.seq === 'number' ? { seq: p.seq } : {}),
            ...(m.auto === true ? { auto: true as const } : {}),
            ...(m.by === 'owner' || m.by === 'alter' ? { by: m.by } : {}),
            ...(typeof m.agent === 'string' && m.agent !== '' ? { agent: m.agent } : {}),
          },
        })
        return
      }
      case 'group.typing': {
        const agentName = str((p as Record<string, unknown>)['agent'] as string | undefined)
        emit({ kind: 'group_typing', gid: str(p.gid), fp: fp(peer), ...(agentName === '' ? {} : { agent: agentName }), on: p.on === true })
        return
      }
      case 'group.updated':
        emit({ kind: 'group_update', gid: str(p.gid) })
        return
      case 'group.application': {
        // A stranger applied to join a group I own: {gid, peer, message} where
        // message.body is the application note and message.card the applicant's card.
        const m = p.message ?? {}
        const cardName = str(m.card?.name)
        emit({
          kind: 'group_application',
          gid: str(p.gid),
          request: { fp: fp(peer), name: cardName !== '' ? cardName : shortFp(peer), note: str(m.body) },
        })
        return
      }
      case 'mission.update':
      case 'artifact.ready':
        log('info', `soulnet notification ${method} from ${shortFp(peer)} (not handled in M1)`)
        return
      default:
        log('warn', `unknown soulnet notification ${method}`)
    }
  }

  const failWaiters = (error: Error): void => {
    const waiters = readyWaiters
    readyWaiters = []
    for (const w of waiters) w.reject(error)
  }

  const scheduleRestart = (reason: string): void => {
    if (disposed) return
    restarts += 1
    const delay = backoffMs
    backoffMs = Math.min(backoffMax, Math.round(backoffMs * backoffFactor))
    setStatus({ state: 'restarting', restarts, lastError: reason })
    log('warn', `soulnet peer died (${reason}); restart #${restarts} in ${delay} ms`)
    restartTimer = setTimeout(() => {
      restartTimer = undefined
      spawnPeer()
    }, delay)
    restartTimer.unref?.()
  }

  const spawnPeer = (): void => {
    if (disposed) return
    const location = locateSoulnetBinary(options.peerBinary)
    if (location === undefined) {
      const pkg = platformPackageName() ?? `${PLATFORM_PACKAGE_PREFIX}<os>-<arch> (none published for ${process.platform}-${process.arch})`
      const message = `soulnet binary not found: the platform package ${pkg} is not installed next to the plugin (reinstall with optional dependencies enabled), or set \`peerBinary\` in the SoulMirror network settings, put \`soulnet\` on PATH, or place it in the plugin's bin/ directory`
      setStatus({ state: 'error', lastError: message })
      log('error', message)
      failWaiters(new NetworkError(message, NetworkErrorCode.peerUnavailable))
      return
    }
    const binary = location.path
    const args = ['--home', options.home, '--relay', relay]
    clearError()
    setStatus({ state: 'starting', binary, binarySource: location.source })
    log('info', `soulnet binary: ${binary} (${location.source})`)
    let proc: ChildProcess
    try {
      proc = options.spawn !== undefined
        ? options.spawn({ binary, args })
        : spawn(binary, args, { stdio: ['pipe', 'pipe', 'pipe'], windowsHide: true, env: { ...process.env, ...(options.env ?? {}) } })
    } catch (error: unknown) {
      scheduleRestart(`spawn failed: ${String(error)}`)
      return
    }
    child = proc
    if (proc.stdout === null || proc.stdin === null) {
      proc.kill()
      scheduleRestart('spawned process has no stdio pipes')
      return
    }
    proc.stderr?.setEncoding('utf8')
    proc.stderr?.on('data', (chunk: string) => {
      for (const line of chunk.split(/\r?\n/)) {
        if (line.trim() === '') continue
        log('info', `[soulnet] ${line.trim()}`)
        // DEBUG: mirror the peer's stderr (its logf) to a file so the
        // [recv-debug]/[grp-debug] trace is visible while the GUI is up.
        try { appendFileSync(join(options.home, 'peer-debug.log'), `${line.trim()}\n`) } catch { /* best effort */ }
      }
    })
    const ep = new JsonRpcEndpoint(proc.stdout, proc.stdin, {
      timeoutMs: requestTimeoutMs,
      onNotification: n => { handleNotification(n.method, n.params) },
      onProtocolError: (error, line) => { log('warn', `soulnet protocol: ${error.message}: ${line.slice(0, 200)}`) },
    })
    endpoint = ep
    let exited = false
    proc.on('error', (error: Error) => {
      if (exited) return
      exited = true
      if (endpoint === ep) endpoint = undefined
      ep.close(error)
      scheduleRestart(`process error: ${error.message}`)
    })
    proc.on('exit', (code, signal) => {
      if (exited) return
      exited = true
      if (endpoint === ep) endpoint = undefined
      if (child === proc) child = undefined
      ep.close()
      if (disposed) {
        setStatus({ state: 'stopped' })
        return
      }
      scheduleRestart(`exit code=${code ?? 'null'} signal=${signal ?? 'none'}`)
    })
    // Handshake: initialize (creates the identity when a display name is configured and none exists).
    const name = options.displayName?.trim() ?? ''
    void ep.request('initialize', name === '' ? {} : { name }, { timeoutMs: 15_000 }).then((result) => {
      const r = (typeof result === 'object' && result !== null ? result : {}) as { protocol?: string; version?: string; identity?: WireIdentity | null; home?: string; relay?: string }
      if (r.protocol !== SOULNET_PROTOCOL) log('warn', `soulnet speaks ${String(r.protocol)}; this plugin was written for ${SOULNET_PROTOCOL}`)
      backoffMs = backoffInitial
      cachedCardUri = undefined
      setStatus({
        state: 'ready',
        ...(proc.pid === undefined ? {} : { pid: proc.pid }),
        ...(r.protocol === undefined ? {} : { protocol: r.protocol }),
        ...(r.version === undefined ? {} : { version: r.version }),
        ...(r.home === undefined ? {} : { home: r.home }),
        ...(r.relay === undefined ? {} : { relay: r.relay }),
      })
      log('info', `soulnet peer ready pid=${proc.pid ?? '?'} protocol=${String(r.protocol)} identity=${r.identity?.fingerprint ?? 'none'}`)
      const waiters = readyWaiters
      readyWaiters = []
      for (const w of waiters) w.resolve(ep)
    }).catch((error: unknown) => {
      if (exited || disposed) return
      log('error', `soulnet initialize failed: ${String(error)}`)
      proc.kill()
    })
  }

  const ready = (timeoutMs: number): Promise<JsonRpcEndpoint> => {
    if (disposed) return Promise.reject(new NetworkError('soulnet backend is disposed', NetworkErrorCode.peerUnavailable))
    if (endpoint !== undefined && !endpoint.isClosed && status.state === 'ready') return Promise.resolve(endpoint)
    if (!started) start()
    if (status.state === 'error') return Promise.reject(new NetworkError(status.lastError ?? 'soulnet backend unavailable', NetworkErrorCode.peerUnavailable))
    return new Promise<JsonRpcEndpoint>((resolve, reject) => {
      const timer = setTimeout(() => {
        readyWaiters = readyWaiters.filter(w => w.resolve !== resolve)
        reject(new NetworkError(`soulnet peer not ready within ${timeoutMs} ms (state=${status.state})`, NetworkErrorCode.peerUnavailable))
      }, timeoutMs)
      timer.unref?.()
      readyWaiters.push({
        resolve: ep => { clearTimeout(timer); resolve(ep) },
        reject: error => { clearTimeout(timer); reject(error) },
      })
    })
  }

  const call = async <T>(method: string, params?: unknown, timeoutMs = requestTimeoutMs): Promise<T> => {
    const ep = await ready(timeoutMs)
    try {
      return (await ep.request(method, params, { timeoutMs })) as T
    } catch (error: unknown) {
      throw toNetworkError(error, method)
    }
  }

  const start = (): void => {
    if (started || disposed) return
    started = true
    spawnPeer()
  }

  const identity = async (): Promise<Identity | undefined> => {
    const r = await call<{ identity?: WireIdentity | null }>('identity.get')
    if (r.identity === undefined || r.identity === null) return undefined
    const cardUri = await card()
    return {
      fp: fp(str(r.identity.fingerprint)),
      name: str(r.identity.name),
      cardUri,
      ...(r.identity.created_at === undefined ? {} : { createdAt: r.identity.created_at }),
    }
  }

  const card = async (): Promise<string> => {
    if (cachedCardUri !== undefined) return cachedCardUri
    const r = await call<{ uri?: string }>('card.get')
    cachedCardUri = str(r.uri)
    return cachedCardUri
  }

  const rememberNames = (friends: readonly Friend[]): void => {
    for (const f of friends) friendNames.set(f.fp, f.name)
  }

  const client: NetworkClient & { start(): void } = {
    backend: 'soulnet',
    start,
    status: () => status,
    identity,
    createIdentity: async (name) => {
      const r = await call<{ identity?: WireIdentity }>('identity.create', { name })
      cachedCardUri = undefined
      const cardUri = await card()
      return { fp: fp(str(r.identity?.fingerprint)), name: str(r.identity?.name, name), cardUri }
    },
    card,
    parseCard: async (uri) => {
      const r = await call<{ uri?: string; fingerprint?: string; card?: WireCard }>('card.parse', { uri })
      return { fp: fp(str(r.fingerprint)), name: str(r.card?.name), uri: str(r.uri, uri) }
    },
    friends: {
      list: async () => {
        const r = await call<{ friends?: WireFriend[] }>('friends.list')
        const friends = (r.friends ?? []).map(friendFromWire)
        rememberNames(friends)
        return friends
      },
      pending: async () => {
        const r = await call<{ pending?: WirePending[] }>('friends.pending')
        return (r.pending ?? []).map(pendingFromWire)
      },
      add: async (cardUri, note) => {
        const r = await call<{ friend?: WireFriend }>('friends.add', { card_uri: cardUri, ...(note === undefined ? {} : { note }) })
        const friend = friendFromWire(r.friend ?? {})
        friendNames.set(friend.fp, friend.name)
        return friend
      },
      accept: async (requestId, note) => {
        const r = await call<{ friend?: WireFriend }>('friends.accept', { id: requestId, ...(note === undefined ? {} : { note }) })
        const friend = friendFromWire(r.friend ?? {})
        friendNames.set(friend.fp, friend.name)
        return friend
      },
      reject: async (requestId) => {
        await call('friends.reject', { id: requestId })
      },
      set: async (target, patch) => {
        const r = await call<{ friend?: WireFriend }>('friends.set', {
          fp: target,
          ...(patch.remark === undefined ? {} : { note: patch.remark }),
          ...(patch.protocol === undefined ? {} : { protocol: patch.protocol }),
        })
        const friend = friendFromWire(r.friend ?? {})
        friendNames.set(friend.fp, friend.name)
        return friend
      },
      remove: async (target) => {
        await call('friends.remove', { fp: target })
        friendNames.delete(target)
      },
      card: async (target) => {
        const r = await call<{ uri?: string; fingerprint?: string; card?: WireCard }>('friends.card', { fp: target })
        return { fp: fp(str(r.fingerprint, target)), name: str(r.card?.name), uri: str(r.uri) }
      },
    },
    groups: {
      list: async () => ((await call<{ groups?: WireGroup[] }>('group.list')).groups ?? []).map(groupFromWire),
      create: async (name, members, profile) =>
        groupInfoFromWire((await call<{ group?: WireGroupInfo }>('group.create', {
          name,
          members: [...members],
          ...(profile === undefined ? {} : { profile: profileToWire(profile) }),
        })).group ?? {}),
      info: async (gid) =>
        groupInfoFromWire((await call<{ group?: WireGroupInfo }>('group.get', { gid })).group ?? {}),
      send: async (gid, body, options) => {
        const r = await call<{ id?: string; seq?: number; status?: string }>('group.send', {
          gid,
          body,
          ...(options?.by === undefined ? {} : { by: options.by }),
          ...(options?.auto === true ? { auto: true } : {}),
          ...(options?.agent === undefined || options.agent === '' ? {} : { agent: options.agent }),
        })
        return { id: mid(str(r.id)), status: str(r.status, 'sent'), ...(typeof r.seq === 'number' ? { seq: r.seq } : {}) }
      },
      conversation: async (gid, opts = {}) => {
        const r = await call<{ entries?: WireEntry[] }>('group.conversation', {
          gid,
          ...(opts.since === undefined ? {} : { since: opts.since }),
          ...(opts.limit === undefined ? {} : { limit: opts.limit }),
        })
        return { entries: (r.entries ?? []).map(entryFromWire) }
      },
      announceVoices: async (gid, voices) => {
        await call('group.voicesAnnounce', { gid, voices: [...voices] })
      },
      typing: async (gid, on, agent) => {
        await call('group.typing', { gid, on, ...(agent === undefined || agent === '' ? {} : { agent }) })
      },
      markRead: async (gid, seq) => {
        await call('group.markRead', { gid, seq })
      },
      leave: async (gid) => {
        await call('group.leave', { gid })
      },
      kick: async (gid, target) => {
        await call('group.kick', { gid, fp: target })
      },
      setProfile: async (gid, profile) => {
        await call('group.setProfile', { gid, profile: profileToWire(profile) })
      },
      pin: async (gid, body) => {
        await call('group.pin', { gid, body })
      },
      unpin: async (gid, id) => {
        await call('group.unpin', { gid, id })
      },
      apply: async (uri, note) => {
        const r = await call<{ ok?: boolean; gid?: string }>('group.apply', { uri, ...(note === undefined ? {} : { note }) })
        return { gid: str(r.gid) }
      },
      applications: async (gid) =>
        ((await call<{ applications?: WireGroupApplication[] }>('group.applications', { gid })).applications ?? []).map(applicationFromWire),
      approve: async (gid, target) => {
        await call('group.approve', { gid, fp: target })
      },
      applicationReject: async (gid, target) => {
        await call('group.applicationReject', { gid, fp: target })
      },
      invite: async (gid, target) => {
        await call('group.invite', { gid, fp: target })
      },
    },
    send: async (to, body, options) => {
      const r = await call<{ id?: string; seq?: number; status?: string }>('message.send', {
        to,
        body,
        ...(options?.file === undefined ? {} : { file: options.file }),
        ...(options?.auto === true ? { auto: true } : {}),
      })
      const receipt: SendReceipt = { id: mid(str(r.id)), status: str(r.status, 'sent'), ...(typeof r.seq === 'number' ? { seq: r.seq } : {}) }
      return receipt
    },
    typing: async (to, on) => {
      await call('message.typing', { to, on }, 10_000)
    },
    conversation: async (target, opts = {}) => {
      const r = await call<{ entries?: WireEntry[]; typing?: boolean }>('conversation.get', {
        fp: target,
        ...(opts.since === undefined ? {} : { since: opts.since }),
        ...(opts.limit === undefined ? {} : { limit: opts.limit }),
      })
      return { entries: (r.entries ?? []).map(entryFromWire), typing: r.typing === true }
    },
    markRead: async (target, seq) => {
      await call('conversation.markRead', { fp: target, seq })
    },
    presence: async (fps) => {
      const r = await call<{ online?: Record<string, boolean> }>('presence', { fps: [...fps] }, 15_000)
      return r.online ?? {}
    },
    subscribe: (listener) => {
      listeners.add(listener)
      return () => { listeners.delete(listener) }
    },
    dispose: async () => {
      if (disposed) return
      disposed = true
      if (restartTimer !== undefined) {
        clearTimeout(restartTimer)
        restartTimer = undefined
      }
      failWaiters(new NetworkError('soulnet backend is disposed', NetworkErrorCode.peerUnavailable))
      const proc = child
      const ep = endpoint
      if (proc === undefined) {
        setStatus({ state: 'stopped' })
        return
      }
      const exited = new Promise<void>((resolve) => {
        if (proc.exitCode !== null || proc.signalCode !== null) {
          resolve()
          return
        }
        proc.once('exit', () => { resolve() })
      })
      if (ep !== undefined && !ep.isClosed) {
        try {
          await ep.request('shutdown', undefined, { timeoutMs: 2_000 })
        } catch {
          // fall through to kill
        }
      }
      const killTimer = setTimeout(() => { proc.kill() }, 2_000)
      killTimer.unref?.()
      await exited
      clearTimeout(killTimer)
      ep?.close()
      setStatus({ state: 'stopped' })
      log('info', 'soulnet peer stopped')
    },
  }
  return client
}
