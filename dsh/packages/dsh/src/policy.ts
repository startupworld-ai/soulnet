/**
 * Reply policy of the alter (P3, reshaped in P4 for the single alter session)
 * — pure, no I/O, unit-tested in test/policy.test.ts:
 *
 *   - `ReplyTier` per friend: `notify` (inbound mail is appended to the alter
 *     session, no turn), `draft` (a turn is woken; the alter's reply is stored
 *     as a PENDING DRAFT the owner reviews on the SoulMirror page), `auto` (a
 *     turn is woken; the reply is sent without review, rate-limited per
 *     friend per hour).
 *   - `routeInbound()` — what to do with one inbound mail: append only, or
 *     append AND wake the alter. The loop guard lives here: mail flagged
 *     `auto` (another alter's automatic reply) never wakes a turn, and mail
 *     from a non-friend never does either.
 *   - `sendGate()` — whether `soulmirror_send_message` may send now or must
 *     queue a draft. Owner-initiated turns send freely (SoulMirror rule:
 *     friend messages on the owner's instruction are direct; only tasks /
 *     money need confirmation). Inbound-triggered turns send freely only to
 *     the friend who wrote, in the `auto` tier and under the hourly cap;
 *     everything else becomes a draft for the owner. There is no dsh
 *     approval panel in this path any more (P4).
 *   - `HourlyWindow` — the per-friend sliding-window counter behind the cap.
 *   - Groups (wire spec §14.7): `mentionsMe` / `wakeForGroup` decide whether a
 *     group message wakes the alter; `groupSendGate` gates
 *     `soulmirror_send_group_message` by the group profile's agentTier;
 *     `countAutoInWindow` / `agentRoundsExceeded` compute the mechanical caps
 *     (autoPerHour, agentRounds) from the group conversation archive.
 */

export type ReplyTier = 'notify' | 'draft' | 'auto'

export const REPLY_TIERS: readonly ReplyTier[] = ['notify', 'draft', 'auto']

export const DEFAULT_REPLY_TIER: ReplyTier = 'draft'

export const DEFAULT_AUTO_REPLY_PER_HOUR = 20

export function isReplyTier(value: unknown): value is ReplyTier {
  return value === 'notify' || value === 'draft' || value === 'auto'
}

export function normalizeTier(value: unknown, fallback: ReplyTier = DEFAULT_REPLY_TIER): ReplyTier {
  return isReplyTier(value) ? value : fallback
}

/**
 * What woke the current turn of the alter session:
 *   - `owner`: the owner typed an instruction (a `user/message` with
 *     `source.kind === 'user'`) — from the SoulMirror page composer or dsh's
 *     own input bar. `fp` is never set: an instruction that names a friend
 *     is still the owner speaking (the persona resolves the friend as "none");
 *   - `inbound`: mail from a friend woke the turn; `fp` / `name` identify
 *     that friend (the per-friend tier and protocol override are resolved from
 *     it, per turn);
 *   - `inbound-auto`: that mail was itself flagged `auto` (should never wake a
 *     turn — kept for the belt-and-braces branch in the gate);
 *   - `group`: a group message woke the turn; `gid` identifies the group and
 *     `fp` / `name` the member who spoke (wire spec §14.7);
 *   - `unknown`: no user message in the current turn could be attributed (a
 *     tool call from another session, a steer, a resumed turn …).
 */
export interface TurnTrigger {
  readonly kind: 'owner' | 'inbound' | 'inbound-auto' | 'group' | 'unknown'
  /** Fingerprint of the friend (or group member) whose mail woke the turn. */
  readonly fp?: string
  /** Display name of that friend at delivery time. */
  readonly name?: string
  /** A2A id of that mail. */
  readonly messageId?: string
  /** Group id when a group message woke the turn (kind `group`). */
  readonly gid?: string
}

export const UNKNOWN_TRIGGER: TurnTrigger = { kind: 'unknown' }

export interface InboundRoute {
  /** Wake a model turn (the mail is delivered through the agent inbox) or append only. */
  readonly action: 'wake' | 'append'
  /** Why it was not woken (logged). */
  readonly reason?: 'tier-notify' | 'loop-guard-auto' | 'not-a-friend'
}

export interface RouteInboundInput {
  readonly tier: ReplyTier
  /** The mail carries the A2A `auto` flag (another alter's automatic reply). */
  readonly auto: boolean
  /** The sender is in the friend list (the peer only archives friend mail, but the session layer may lag). */
  readonly isFriend: boolean
}

export function routeInbound(input: RouteInboundInput): InboundRoute {
  if (!input.isFriend) return { action: 'append', reason: 'not-a-friend' }
  if (input.auto) return { action: 'append', reason: 'loop-guard-auto' }
  if (input.tier === 'notify') return { action: 'append', reason: 'tier-notify' }
  return { action: 'wake' }
}

export type SendGateReason =
  | 'owner-initiated' | 'auto-tier'
  | 'draft-tier' | 'notify-tier' | 'rate-limited' | 'loop-guard-auto' | 'unknown-trigger' | 'other-friend' | 'group-trigger'

export type SendDecision =
  /** Send now; `auto` says whether to flag the wire message as an automatic reply. */
  | { readonly kind: 'allow'; readonly auto: boolean; readonly reason: 'owner-initiated' | 'auto-tier' }
  /** Do not send: store a pending draft for the owner to review on the page. */
  | { readonly kind: 'draft'; readonly reason: Exclude<SendGateReason, 'owner-initiated' | 'auto-tier'> }

export interface SendGateInput {
  readonly trigger: TurnTrigger
  /** The target friend of this send. */
  readonly target: string
  /** Effective reply tier of the TARGET friend. */
  readonly tier: ReplyTier
  /** Automatic replies already sent to the target in the current hour window. */
  readonly autoSentInWindow: number
  /** `autoReplyPerHour` (<= 0 disables automatic sends entirely). */
  readonly limit: number
}

export function sendGate(input: SendGateInput): SendDecision {
  switch (input.trigger.kind) {
    case 'owner':
      return { kind: 'allow', auto: false, reason: 'owner-initiated' }
    case 'inbound-auto':
      return { kind: 'draft', reason: 'loop-guard-auto' }
    case 'inbound':
      // The alter answering friend A by writing to friend B is not an auto reply to A.
      if (input.trigger.fp !== undefined && input.trigger.fp !== input.target) return { kind: 'draft', reason: 'other-friend' }
      if (input.tier === 'auto') {
        if (input.limit > 0 && input.autoSentInWindow < input.limit) return { kind: 'allow', auto: true, reason: 'auto-tier' }
        return { kind: 'draft', reason: 'rate-limited' }
      }
      return { kind: 'draft', reason: input.tier === 'draft' ? 'draft-tier' : 'notify-tier' }
    case 'group':
      // A group message is answered into that group (soulmirror_send_group_message), not by private mail.
      return { kind: 'draft', reason: 'group-trigger' }
    case 'unknown':
    default:
      return { kind: 'draft', reason: 'unknown-trigger' }
  }
}

export const HOUR_MS = 3_600_000

/** Sliding one-hour window of timestamps per key (friend fingerprint). */
export class HourlyWindow {
  private readonly hits = new Map<string, number[]>()

  constructor(private readonly windowMs: number = HOUR_MS) {}

  /** Timestamps still inside the window for `key`, oldest first. */
  count(key: string, now: number = Date.now()): number {
    return this.prune(key, now).length
  }

  /** Record one hit for `key`; returns the new count. */
  record(key: string, now: number = Date.now()): number {
    const list = this.prune(key, now)
    list.push(now)
    this.hits.set(key, list)
    return list.length
  }

  /** Milliseconds until the oldest hit leaves the window (0 when none). */
  retryAfter(key: string, now: number = Date.now()): number {
    const list = this.prune(key, now)
    if (list.length === 0) return 0
    return Math.max(0, list[0]! + this.windowMs - now)
  }

  private prune(key: string, now: number): number[] {
    const list = (this.hits.get(key) ?? []).filter(ts => now - ts < this.windowMs)
    if (list.length === 0) this.hits.delete(key)
    else this.hits.set(key, list)
    return list
  }
}

// ——— Groups (wire spec §14.7): mechanical enforcement of the group profile ———

/** Group wake policies (profile agentWake). */
export type GroupAgentWake = 'mention' | 'always' | 'never'

/** Group reply tiers reuse the friend vocabulary (profile agentTier). */
export type GroupAgentTier = ReplyTier

export function normalizeGroupWake(value: unknown, fallback: GroupAgentWake = 'mention'): GroupAgentWake {
  return value === 'mention' || value === 'always' || value === 'never' ? value : fallback
}

/**
 * Does `body` mention this alter? True for `@all` and for `@<name>`
 * (case-insensitive). A token ending in an ASCII word character requires a
 * word boundary after the match (`@ann` must not fire on `@anne`); a token
 * ending in a non-ASCII rune matches bare, because scripts without spaces
 * cannot delimit the mention.
 */
export function mentionsMe(body: string, name: string): boolean {
  const lower = body.toLowerCase()
  if (hasMentionToken(lower, 'all')) return true
  const n = name.trim().toLowerCase()
  if (n === '') return false
  return hasMentionToken(lower, n)
}

/**
 * Does `body` mention this NAMED seat agent? Same token grammar as
 * {@link mentionsMe} but WITHOUT the `@all` shortcut: `@all` greets the room,
 * it does not command every named agent into action.
 */
export function mentionsAgent(body: string, name: string): boolean {
  const n = name.trim().toLowerCase()
  if (n === '') return false
  return hasMentionToken(body.toLowerCase(), n)
}

const isAsciiWordChar = (ch: string | undefined): boolean => ch !== undefined && /[a-z0-9_]/.test(ch)

function hasMentionToken(lowerBody: string, lowerName: string): boolean {
  const token = `@${lowerName}`
  const needsBoundary = isAsciiWordChar(lowerName[lowerName.length - 1])
  for (let at = lowerBody.indexOf(token); at >= 0; at = lowerBody.indexOf(token, at + 1)) {
    if (!needsBoundary || !isAsciiWordChar(lowerBody[at + token.length])) return true
  }
  return false
}

export interface GroupWakeInput {
  /** The group profile allows agents to speak (speakAgents). */
  readonly speakAgents: boolean
  /** The per-group client toggle (dsh-groups.json `alter`); default OFF. */
  readonly enabled: boolean
  /** The sender is this identity (own fan-out echo). */
  readonly fromSelf: boolean
  /** The group profile's agentWake policy. */
  readonly wake: GroupAgentWake
  /**
   * MY participation strategy (dsh-groups.json `mode`): the member's own ceiling on top
   * of the group's. 'mention' (default) wakes only when named even in an `always` group;
   * 'always' follows the group policy. The stricter of the two wins.
   */
  readonly myMode?: 'mention' | 'always'
  /** The body mentions this alter (`@<name>` / `@all`). */
  readonly mentioned: boolean
}

export interface GroupWakeRoute {
  readonly wake: boolean
  /** Why it was not woken (logged). */
  readonly reason?: 'self' | 'agents-muted' | 'alter-disabled' | 'wake-never' | 'not-mentioned' | 'voice-disabled' | 'not-commander'
}

export interface AgentWakeInput {
  /** The group profile allows agents to speak (speakAgents). */
  readonly speakAgents: boolean
  /** The per-group voice switch of this agent (dsh-groups.json `voices`); default OFF. */
  readonly enabled: boolean
  /** The sender is this identity (own fan-out echo; own posts reach agents through the own-post hook instead). */
  readonly fromSelf: boolean
  /** The group profile's agentWake policy (the ceiling). */
  readonly wake: GroupAgentWake
  /** This agent holds the group's DUTY slot: it answers unmentioned traffic (still capped by the group's wake policy). */
  readonly duty: boolean
  /** The body names this agent (`@<name>`; `@all` does not count — see mentionsAgent). */
  readonly mentioned: boolean
  /** The sender is on this agent's commander whitelist (or the list holds `*`). */
  readonly commander: boolean
}

/**
 * Should a group message wake one NAMED seat agent? Like {@link wakeForGroup}
 * for the alter, plus the commander whitelist: an agent executes work, so a
 * sender outside its whitelist never wakes it — mentioned or not. The duty
 * slot stands in for the alter's `always` mode and is per group unique
 * (../group-settings.ts `duty`).
 */
export function wakeAgentForGroup(input: AgentWakeInput): GroupWakeRoute {
  if (input.fromSelf) return { wake: false, reason: 'self' }
  if (!input.speakAgents) return { wake: false, reason: 'agents-muted' }
  if (!input.enabled) return { wake: false, reason: 'voice-disabled' }
  if (!input.commander) return { wake: false, reason: 'not-commander' }
  // The group's wake policy is the ceiling: duty can lift mention-only up to
  // the group's `always`, never past it.
  const effective = input.wake === 'always' && !input.duty ? 'mention' : input.wake
  switch (effective) {
    case 'never':
      return { wake: false, reason: 'wake-never' }
    case 'always':
      return { wake: true }
    case 'mention':
    default:
      return input.mentioned ? { wake: true } : { wake: false, reason: 'not-mentioned' }
  }
}

/** Should a group message wake the alter? Every condition must hold; the STRICTER of the
 * group's wake policy and my own strategy wins. */
export function wakeForGroup(input: GroupWakeInput): GroupWakeRoute {
  if (input.fromSelf) return { wake: false, reason: 'self' }
  if (!input.speakAgents) return { wake: false, reason: 'agents-muted' }
  if (!input.enabled) return { wake: false, reason: 'alter-disabled' }
  // Effective policy: my strategy can only tighten the group's ceiling, never exceed it.
  const effective = input.wake === 'always' && (input.myMode ?? 'mention') === 'mention' ? 'mention' : input.wake
  switch (effective) {
    case 'never':
      return { wake: false, reason: 'wake-never' }
    case 'always':
      return { wake: true }
    case 'mention':
    default:
      return input.mentioned ? { wake: true } : { wake: false, reason: 'not-mentioned' }
  }
}

/** The slice of a group conversation entry the mechanical caps read. */
export interface GroupCapEntry {
  readonly dir: 'in' | 'out'
  readonly ts: number
  readonly auto?: boolean
  /** Message provenance: `alter`, `owner`, or undefined (human / legacy). */
  readonly by?: string
}

/** My automatic alter posts (dir=out, auto, by=alter) still inside the window. */
export function countAutoInWindow(entries: readonly GroupCapEntry[], now: number = Date.now(), windowMs: number = HOUR_MS): number {
  let count = 0
  for (const e of entries) {
    if (e.dir === 'out' && e.auto === true && e.by === 'alter' && now - e.ts < windowMs && now - e.ts >= 0) count += 1
  }
  return count
}

/**
 * agentRounds tail rule: the last `rounds` or more consecutive archive
 * entries are all agent posts (by=alter) with no human message in between →
 * agents go quiet until a human speaks (or one is mentioned). A non-positive
 * `rounds` never trips (the caller normalizes the profile default first).
 */
export function agentRoundsExceeded(entries: readonly GroupCapEntry[], rounds: number): boolean {
  if (rounds <= 0) return false
  let streak = 0
  for (let i = entries.length - 1; i >= 0; i -= 1) {
    if (entries[i]!.by !== 'alter') break
    streak += 1
    if (streak >= rounds) return true
  }
  return false
}

/**
 * The tier a group voice (the alter or one NAMED seat agent) answers a group
 * with. `draft` is a per-seat review requirement, not the group's veto — it
 * lifts to `auto` (direct, flagged automatic, capped) for the ALTER always
 * (owner decision 2026-08-26: alter group replies need no review) and for a
 * working agent without the per-agent approval switch; only an agent WITH the
 * switch keeps `draft`. The group's `notify` (agents observe only) and `auto`
 * pass through untouched: what the group forbids stays forbidden, and the
 * mechanical caps still apply.
 */
export function effectiveAgentTier(tier: GroupAgentTier, approval: boolean): GroupAgentTier {
  return !approval && tier === 'draft' ? 'auto' : tier
}

export type GroupSendDecision =
  /** Send now as the alter (`by: 'alter'`); `auto` additionally flags a group-triggered automatic post. */
  | { readonly kind: 'allow'; readonly auto: boolean; readonly reason: 'owner-initiated' | 'auto-tier' }
  /** Store a pending draft (keyed by the gid) for the owner to review. */
  | { readonly kind: 'draft'; readonly reason: 'draft-tier' | 'rate-limited' | 'agent-rounds' | 'other-group' | 'unknown-trigger' }
  /** Refuse the call: the alter must not attempt to speak here. */
  | { readonly kind: 'refuse'; readonly reason: 'agents-muted' | 'notify-tier' }

/** Every reason the group gate can answer with (machine-readable in the tool result). */
export type GroupSendGateReason = GroupSendDecision['reason']

/** Why a pending draft was stored: the friend gate's or the group gate's reason. */
export type DraftReason = SendGateReason | GroupSendGateReason

export interface GroupSendGateInput {
  readonly trigger: TurnTrigger
  /** The target group of this send. */
  readonly gid: string
  /**
   * The group message that woke this turn was the OWNER's own post (the
   * own-post hook: the owner mentioned one of their voices in their own
   * message). That is an instruction in group clothing — it sends directly,
   * like an owner-initiated turn.
   */
  readonly fromOwner?: boolean
  /** Group profile: agents may speak at all (speakAgents). */
  readonly speakAgents: boolean
  /** Group profile agentTier (normalized). */
  readonly tier: GroupAgentTier
  /** My automatic posts already inside the group's hour window (archive-counted). */
  readonly autoSentInWindow: number
  /** Group profile autoPerHour (normalized; <= 0 disables automatic sends). */
  readonly autoPerHour: number
  /** The agentRounds tail rule trips (see `agentRoundsExceeded`). */
  readonly roundsExceeded: boolean
  /** The triggering group message mentioned this alter (rounds exception). */
  readonly mentioned: boolean
}

/**
 * Gate one `soulmirror_send_group_message` call: owner-instructed turns send
 * directly; turns woken by the target group resolve the profile's agentTier —
 * `auto` sends flagged auto when the caps pass and falls to a draft
 * otherwise, `draft` always drafts, `notify` refuses (the alter should not
 * attempt to speak). Any other trigger drafts. NOTE the caller resolves the
 * tier through {@link effectiveAgentTier} first, so `draft` only reaches this
 * gate for a named seat agent with its approval switch on — the alter's group
 * replies go out directly (as `auto`).
 */
export function groupSendGate(input: GroupSendGateInput): GroupSendDecision {
  if (!input.speakAgents) return { kind: 'refuse', reason: 'agents-muted' }
  switch (input.trigger.kind) {
    case 'owner':
      return { kind: 'allow', auto: false, reason: 'owner-initiated' }
    case 'group': {
      if (input.trigger.gid !== input.gid) return { kind: 'draft', reason: 'other-group' }
      if (input.fromOwner === true) return { kind: 'allow', auto: false, reason: 'owner-initiated' }
      if (input.tier === 'notify') return { kind: 'refuse', reason: 'notify-tier' }
      if (input.tier === 'draft') return { kind: 'draft', reason: 'draft-tier' }
      if (input.autoPerHour <= 0 || input.autoSentInWindow >= input.autoPerHour) return { kind: 'draft', reason: 'rate-limited' }
      if (input.roundsExceeded && !input.mentioned) return { kind: 'draft', reason: 'agent-rounds' }
      return { kind: 'allow', auto: true, reason: 'auto-tier' }
    }
    case 'inbound':
    case 'inbound-auto':
      // A friend's mail is answered to that friend, not into a group.
      return { kind: 'draft', reason: 'other-group' }
    case 'unknown':
    default:
      return { kind: 'draft', reason: 'unknown-trigger' }
  }
}
