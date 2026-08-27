/**
 * SHIM over the frozen network-layer group contract (wire spec §14.7), kept
 * in its own file so the network module can absorb it without touching the
 * alter pipeline:
 *
 *   - `NetworkClient.groups.info(gid)` (and `groups.list()` rows) carry a
 *     `profile` (speakHumans / speakAgents / agentWake / agentTier /
 *     autoPerHour / agentRounds / rules / admins …). The interface in
 *     ./network/types.ts may not declare it yet in this tree, so
 *     `groupProfileOf` reads it structurally and normalizes defaults
 *     (a missing profile = the standard template: agents allowed, wake on
 *     mention, draft tier, 10 auto posts/hour, 3 agent rounds).
 *   - `groups.send(gid, body, opts?: { by?: 'owner' | 'alter'; auto?: boolean })`
 *     — `sendGroupMessage` forwards the provenance options; a backend that
 *     does not accept them yet simply ignores the extra argument.
 *   - Group conversation entries carry `by` (message provenance) next to
 *     `from`; `entryBy` reads it structurally.
 *
 * When ./network/types.ts declares all of this, the casts here collapse and
 * this file can fold into the callers.
 */
import type { ConversationEntry, Group, GroupInfo, NetworkClient, SendReceipt } from './network/types.ts'
import { normalizeGroupWake, normalizeTier, type GroupAgentTier, type GroupAgentWake } from './policy.ts'

/** Profile defaults of the "standard group" template (a2a DefaultGroupProfile). */
export const DEFAULT_GROUP_AUTO_PER_HOUR = 10
export const DEFAULT_GROUP_AGENT_ROUNDS = 3

/**
 * Paid-join compatibility encoding: on relays that only accept
 * join ∈ {invite, apply, open}, the paid config is carried as a convention
 * line in `rules` (`#paid-join <price> <0xaddr>`) — a field old relays keep
 * verbatim. The line is hidden from every user-facing rules display.
 */
export const PAID_JOIN_PREFIX = '#paid-join '

export interface PaidJoinInfo {
  readonly price: string
  readonly addr: string
}

/** Parse the paid-join marker out of rules (any line, trimmed). */
export function paidJoinFromRules(rules: string | undefined): PaidJoinInfo | undefined {
  if (rules === undefined || rules === '') return undefined
  for (const line of rules.split('\n')) {
    const t = line.trim()
    if (t.startsWith(PAID_JOIN_PREFIX)) {
      const rest = t.slice(PAID_JOIN_PREFIX.length).trim().split(/\s+/)
      const price = rest[0]
      const addr = rest[1]
      if (price !== undefined && addr !== undefined && addr.startsWith('0x') && price !== '') return { price, addr }
    }
  }
  return undefined
}

/**
 * The published paid-join config of one group, whether native
 * (profile.joinPrice/joinAddr with join = "paid") or the rules-marker
 * compatibility encoding (`#paid-join <price> <0xaddr>`). The OWNER-side
 * verifier MUST use these published values — never anything an applicant
 * self-reports — so the minimum amount is the published price and the only
 * valid recipient is the published address.
 */
export function paidJoinConfig(profile: { readonly joinPrice?: string; readonly joinAddr?: string; readonly rules?: string; readonly join?: string } | undefined): PaidJoinInfo | undefined {
  if (profile === undefined) return undefined
  if (profile.join === 'paid' && profile.joinPrice !== undefined && profile.joinAddr !== undefined && profile.joinAddr.startsWith('0x')) {
    return { price: profile.joinPrice, addr: profile.joinAddr }
  }
  return paidJoinFromRules(profile.rules)
}

/** rules without the paid-join marker line (for display / prompts). */
export function rulesWithoutPaidJoin(rules: string | undefined): string {
  if (rules === undefined || rules === '') return ''
  return rules.split('\n').filter(l => !l.trim().startsWith(PAID_JOIN_PREFIX)).join('\n').trim()
}

/** Prepends/replaces the marker as the FIRST rules line (invisible to users;
 * the public group card only carries the first ~280 chars of rules, so the
 * marker must lead for strangers to read the price). */
export function rulesWithPaidJoin(rules: string | undefined, price: string, addr: string): string {
  const base = rulesWithoutPaidJoin(rules)
  const marker = `${PAID_JOIN_PREFIX}${price} ${addr}`
  return base === '' ? marker : `${marker}\n${base}`
}

/** The normalized governance view of one group's profile. */
export interface GroupProfileView {
  readonly speakHumans: boolean
  readonly speakAgents: boolean
  /** Which members may post at all: all | owner | admins. */
  readonly speakWho: 'all' | 'owner' | 'admins'
  readonly agentWake: GroupAgentWake
  readonly agentTier: GroupAgentTier
  readonly autoPerHour: number
  readonly agentRounds: number
  readonly rules: string
  readonly admins: readonly string[]
  readonly room: string
  /** Effective join policy: "paid" when the wire says paid or rules carry the marker. */
  readonly join: 'invite' | 'apply' | 'open' | 'paid'
  /** Parsed paid-join price/address (present when join = paid). */
  readonly paidJoin?: PaidJoinInfo
}

const rec = (value: unknown): Record<string, unknown> => (typeof value === 'object' && value !== null ? value as Record<string, unknown> : {})

/** Normalize a raw profile object (or undefined) into the enforced view. */
export function normalizeGroupProfile(raw: unknown): GroupProfileView {
  const p = rec(raw)
  const who = p['speakWho']
  const perHour = p['autoPerHour']
  const rounds = p['agentRounds']
  const rules = typeof p['rules'] === 'string' ? p['rules'] : ''
  const paidJoin = paidJoinFromRules(rules)
  const wireJoin = p['join']
  const join: GroupProfileView['join'] = paidJoin !== undefined
    ? 'paid'
    : wireJoin === 'invite' || wireJoin === 'apply' || wireJoin === 'open' ? wireJoin : 'invite'
  return {
    speakHumans: p['speakHumans'] !== false,
    speakAgents: p['speakAgents'] !== false,
    speakWho: who === 'owner' || who === 'admins' ? who : 'all',
    agentWake: normalizeGroupWake(p['agentWake']),
    agentTier: normalizeTier(p['agentTier'], 'draft'),
    autoPerHour: typeof perHour === 'number' && Number.isFinite(perHour) && perHour > 0 ? Math.floor(perHour) : DEFAULT_GROUP_AUTO_PER_HOUR,
    agentRounds: typeof rounds === 'number' && Number.isFinite(rounds) && rounds > 0 ? Math.floor(rounds) : DEFAULT_GROUP_AGENT_ROUNDS,
    rules,
    admins: Array.isArray(p['admins']) ? p['admins'].filter((a): a is string => typeof a === 'string') : [],
    room: typeof p['room'] === 'string' && p['room'] !== '' ? p['room'] : 'chat',
    join,
    ...(paidJoin === undefined ? {} : { paidJoin }),
  }
}

/** The profile of a group row / info, read structurally (contract field `profile`). */
export function groupProfileOf(group: Group | GroupInfo): GroupProfileView {
  return normalizeGroupProfile(rec(group)['profile'])
}

/** Provenance options of one group send (contract `groups.send` opts). */
export interface GroupSendOptions {
  readonly by?: 'owner' | 'alter'
  readonly auto?: boolean
  /** Seat agent name behind a by=alter post (display provenance, e.g. "DevBot"). */
  readonly agent?: string
}

type GroupsSendWithOptions = (gid: string, body: string, opts?: GroupSendOptions) => Promise<SendReceipt>

/** Send into a group with provenance (`by` / `auto`) forwarded to the backend. */
export function sendGroupMessage(client: NetworkClient, gid: string, body: string, opts?: GroupSendOptions): Promise<SendReceipt> {
  return (client.groups.send as GroupsSendWithOptions)(gid, body, opts)
}

/** Message provenance of one archived group entry (contract field `by`). */
export function entryBy(entry: ConversationEntry): string | undefined {
  const by = rec(entry)['by']
  return typeof by === 'string' && by !== '' ? by : undefined
}
