/**
 * soulmirror-tools — model-facing tools over `ctx.soulmirror`:
 *
 *   soulmirror_friends            list friends with presence / unread
 *   soulmirror_add_friend         send a friend request (approval first)
 *   soulmirror_send_message       send a text to a friend — directly on the
 *                                 owner's instruction or in the friend's `auto`
 *                                 tier, otherwise STORED AS A PENDING DRAFT the
 *                                 owner reviews on the SoulMirror page (P4)
 *   soulmirror_send_group_message post into a group as the alter (by=alter) —
 *                                 gated by the group profile's agentTier and
 *                                 the mechanical caps autoPerHour /
 *                                 agentRounds, both counted from the group
 *                                 conversation archive (wire spec §14.7)
 *   soulmirror_read_conversation  read the archived conversation with a friend
 *   soulmirror_card               own card URI
 *
 * P4 — the send gate (../policy.ts `sendGate`): the decision depends on what
 * woke the CURRENT turn of the calling session (read back from its log,
 * ../alter-state.ts `triggerOf`) and on the TARGET friend's reply tier: an
 * owner instruction → send now (SoulMirror rule: friend messages on the
 * owner's word are direct; only tasks / money need confirmation); mail from
 * the target friend in the `auto` tier under the hourly cap → send now,
 * flagged `auto` on the wire (loop guard) and counted against
 * `autoReplyPerHour`; everything else (draft / notify tier, over the cap, a
 * turn woken by an auto-flagged mail, an unattributed trigger, answering
 * friend A by writing to friend B) → `queueDraft` on the sessions face: the
 * text is stored in `<home>/a2a/dsh-pending.json`, nothing leaves the machine,
 * and the result says `draft-queued`. The dsh approval panel is no longer in
 * this path; it remains only as the fallback when no sessions face exists
 * (no drafts store) — and for `soulmirror_add_friend`.
 *
 * Tools are registered as raw `ToolDefinition` objects through the small
 * local `defineTool` helper in ./define.ts (JSON-Schema parameters, typed
 * arguments, own validation) instead of `@deepseek-ai/dsh-tools`' `defineTool`:
 * this host half keeps zero @deepseek-ai VALUE imports (see ../index.ts and
 * README.md — a value import of dsh-tools would pull a second copy of
 * cordis/dsh-llm/dsh-session under a linked install).
 */
import type { Context } from '@deepseek-ai/cordis'
import type { SessionId } from '@deepseek-ai/dsh-session'
import type { ToolRunContext } from '@deepseek-ai/dsh-tools'
// Type-only: the ctx.tools / ctx.approval Context merges.
import type {} from '@deepseek-ai/dsh-tools'
import type {} from '@deepseek-ai/dsh-user-approval'
import type { Fingerprint } from '../events.ts'
import { entryBy, groupProfileOf, sendGroupMessage } from '../group-contract.ts'
import { sendAndArchive } from '../network/send.ts'
import { NetworkError, NetworkErrorCode, type ConversationEntry, type Friend } from '../network/types.ts'
import { agentRoundsExceeded, countAutoInWindow, DEFAULT_AUTO_REPLY_PER_HOUR, DEFAULT_REPLY_TIER, effectiveAgentTier, groupSendGate, mentionsAgent, mentionsMe, sendGate, UNKNOWN_TRIGGER, type GroupCapEntry, type SendDecision, type TurnTrigger } from '../policy.ts'
import type { SoulmirrorSettings } from '../settings.ts'
import { defineTool } from './define.ts'
import type {} from '../index.ts'
import type { AlterSessions } from '../sessions/index.ts'
import type { MemoryKind, MemoryScope } from '../memory/store.ts'

export const name = 'soulmirror-tools'
export const inject = ['tools', 'soulmirror']

type ApprovalOutcome = 'allowed-once' | 'rejected' | 'cancelled' | 'unavailable'

function friendView(f: Friend): Record<string, unknown> {
  return {
    fingerprint: f.fp,
    name: f.name,
    ...(f.remark === undefined ? {} : { note: f.remark }),
    ...(f.cardName === undefined ? {} : { card_name: f.cardName }),
    online: f.online === true,
    unread: f.unread,
    messages: f.count,
    ...(f.lastTs === undefined ? {} : { last_at: new Date(f.lastTs).toISOString() }),
    ...(f.typing === true ? { typing: true } : {}),
  }
}

/** The seams the send gate reads (injected for tests; defaults read the live services). */
export interface SendGateSeams {
  readonly sessions: () => AlterSessions | undefined
  readonly settings: () => Pick<SoulmirrorSettings, 'autoReplyPerHour'>
}

/**
 * Decide how a `soulmirror_send_message` call from `sessionId` to `target`
 * is gated (exported for tests): `allow` (send now) or `draft` (store for the
 * owner). Without a sessions face or a session the trigger is unknown → draft.
 */
export function decideSend(seams: SendGateSeams, sessionId: SessionId | undefined, target: Fingerprint): { decision: SendDecision; trigger: TurnTrigger } {
  const face = seams.sessions()
  const trigger = face === undefined || sessionId === undefined ? UNKNOWN_TRIGGER : face.triggerOf(sessionId)
  const decision = sendGate({
    trigger,
    target,
    tier: face?.tierOf(target) ?? DEFAULT_REPLY_TIER,
    autoSentInWindow: face?.autoReplies.count(target) ?? 0,
    limit: seams.settings().autoReplyPerHour,
  })
  return { decision, trigger }
}

export function apply(ctx: Context): void {
  const net = ctx.soulmirror
  const loose = ctx as unknown as { get(name: string): unknown }
  const seams: SendGateSeams = {
    sessions: () => loose.get('soulmirrorSessions') as AlterSessions | undefined,
    settings: () => {
      const live = loose.get('soulmirrorConfig') as { current(): SoulmirrorSettings } | undefined
      return { autoReplyPerHour: live?.current().autoReplyPerHour ?? DEFAULT_AUTO_REPLY_PER_HOUR }
    },
  }

  /** Ask the human through dsh's approval seam. `unavailable` (no answerer) fails closed. */
  const approve = async (exec: ToolRunContext, toolName: string, reason: string): Promise<ApprovalOutcome> => {
    const approval = ctx.get('approval')
    if (approval === undefined || exec.agent === undefined) return 'unavailable'
    try {
      return await approval.request({ agent: exec.agent, toolName, callId: exec.callId, reason, signal: exec.signal })
    } catch (error: unknown) {
      ctx.logger.warn(`soulmirror-tools: approval request failed: ${String(error)}`)
      return 'unavailable'
    }
  }

  const notApproved = (outcome: ApprovalOutcome, action: string, gate?: string): { ok: false; outcome: ApprovalOutcome; message: string; gate?: string } => ({
    ok: false,
    outcome,
    ...(gate === undefined ? {} : { gate }),
    message: outcome === 'rejected'
      ? `The user rejected the request to ${action}; nothing was sent.`
      : outcome === 'cancelled'
        ? `The approval request to ${action} was cancelled; nothing was sent.`
        : `No one could approve the request to ${action} (approval unavailable in this session); nothing was sent.`,
  })

  const requireFriend = async (fingerprint: string): Promise<Friend> => {
    const friend = (await net.friends.list()).find(f => f.fp === fingerprint || f.fp.startsWith(fingerprint))
    if (friend === undefined) throw new NetworkError(`${fingerprint} is not a friend — add them first (soulmirror_add_friend with their card URI)`, NetworkErrorCode.notFriend)
    return friend
  }

  const friends = defineTool({
    name: 'soulmirror_friends',
    description: 'List the SoulMirror friends of this user: fingerprint, display name, note, presence (online), unread count, message count, last activity. Read-only; no side effects.',
    parameters: {
      online_only: { type: 'boolean', description: 'Return only friends currently online.', optional: true },
    },
    output: { type: 'array', items: { type: 'object' } },
    async execute(args) {
      const list = await net.friends.list()
      let online: Record<string, boolean> = {}
      try {
        online = await net.presence(list.map(f => f.fp))
      } catch {
        // presence is best effort (relay reachability); fall back to the list's own flag
      }
      return list
        .map((f): Friend => ({ ...f, ...(online[f.fp] === undefined ? {} : { online: online[f.fp] }) }))
        .filter(f => args.online_only !== true || f.online === true)
        .map(friendView)
    },
  })

  const card = defineTool({
    name: 'soulmirror_card',
    description: 'Return this user\'s own SoulMirror card URI (soulmirror://card?...) and fingerprint. Others paste the URI to send a friend request. Read-only.',
    parameters: {},
    output: { type: 'object' },
    async execute() {
      const identity = await net.identity()
      if (identity === undefined) throw new NetworkError('no SoulMirror identity yet — the user must create one in Settings → SoulMirror network', NetworkErrorCode.noIdentity)
      return { fingerprint: identity.fp, name: identity.name, card_uri: identity.cardUri }
    },
  })

  const addFriend = defineTool({
    name: 'soulmirror_add_friend',
    description: 'Send a SoulMirror friend request to the owner of a card URI (soulmirror://card?...). Asks the user for approval first; when the approval is rejected, cancelled or unavailable nothing is sent. The peer must accept before messages can be exchanged.',
    parameters: {
      card_uri: { type: 'string', description: 'The other party\'s card URI (soulmirror://card?...).' },
      note: { type: 'string', description: 'Local note/name for this friend; also sent as the greeting.', optional: true },
    },
    output: { type: 'object' },
    async execute(args, exec) {
      const parsed = await net.parseCard(args.card_uri)
      const who = parsed.name !== '' ? `${parsed.name} (${parsed.fp})` : parsed.fp
      const outcome = await approve(exec, 'soulmirror_add_friend', `Send a SoulMirror friend request to ${who}${args.note === undefined ? '' : ` with note "${args.note}"`}`)
      if (outcome !== 'allowed-once') return notApproved(outcome, `send a friend request to ${who}`)
      const friend = await net.friends.add(args.card_uri, args.note)
      return { ok: true, outcome, friend: friendView(friend), message: `Friend request sent to ${who}; messages can be exchanged once they accept.` }
    },
  })

  const sendMessage = defineTool({
    name: 'soulmirror_send_message',
    description: 'Send a text message to a SoulMirror friend (by fingerprint) on the user\'s behalf. When the user instructed you to write to a friend, the message goes out directly. When you are answering a friend\'s mail, it goes out directly only in that friend\'s "auto" reply tier (rate-limited); otherwise the text is stored as a PENDING DRAFT for the user to review on the SoulMirror page (result outcome "draft-queued": nothing was sent yet — tell the user a draft is waiting and do not call the tool again for the same reply). Fails if the fingerprint is not a friend.',
    parameters: {
      fingerprint: { type: 'string', description: 'Fingerprint of the friend (from soulmirror_friends or the roster).' },
      body: { type: 'string', description: 'Message text.' },
    },
    output: { type: 'object' },
    async execute(args, exec) {
      const face = seams.sessions()
      // Context rule for named seat agents: a turn woken BY A GROUP answers in
      // that group (soulmirror_send_group_message) — private mail there would
      // leak a group conversation into a DM in the owner's voice. On the
      // OWNER's direct instruction (trigger 'owner', its own chat) the agent
      // may send private mail like the alter does; the normal send gate applies.
      const voice = face === undefined || exec.agent === undefined ? undefined : face.voiceOf(exec.agent.id)
      if (voice?.kind === 'agent' && face !== undefined && exec.agent !== undefined) {
        const agentTrigger = face.triggerOf(exec.agent.id)
        if (agentTrigger.kind !== 'owner') {
          ctx.logger.info(`soulmirror-tools: private send refused for agent "${voice.agent.name}" (trigger=${agentTrigger.kind}; group turns answer in the group)`)
          return {
            ok: false,
            outcome: 'refused',
            gate: 'agent-group-context',
            message: agentTrigger.kind === 'group'
              ? `You were woken by a group — answer THERE with soulmirror_send_group_message (gid ${agentTrigger.gid ?? '?'}); private mail is not for group turns. Nothing was sent.`
              : "Private mail from an agent is only allowed on your owner's direct instruction in your own chat. Nothing was sent.",
          }
        }
      }
      const friend = await requireFriend(args.fingerprint)
      const preview = args.body.length > 200 ? `${args.body.slice(0, 200)}…` : args.body
      const { decision, trigger } = decideSend(seams, exec.agent?.id, friend.fp)
      if (decision.kind === 'draft') {
        if (face !== undefined) {
          const draft = await face.queueDraft({ fp: friend.fp, body: args.body, reason: decision.reason, trigger, ...(exec.agent === undefined ? {} : { sessionId: exec.agent.id }) })
          ctx.logger.info(`soulmirror-tools: draft ${draft.id} queued for ${friend.fp} (gate=${decision.reason})`)
          return {
            ok: true,
            outcome: 'draft-queued',
            gate: decision.reason,
            auto: false,
            draftId: draft.id,
            to: friendView(friend),
            message: `Nothing was sent: your text to ${friend.name} is stored as draft ${draft.id} and waits for the user's review on the SoulMirror page (reason: ${decision.reason}). Tell the user in one line; do not call this tool again for the same reply.`,
          }
        }
        // No sessions face (no drafts store): fall back to dsh's approval seam, fail closed.
        const outcome = await approve(exec, 'soulmirror_send_message', `Send to ${friend.name} (${friend.fp}): "${preview}"`)
        if (outcome !== 'allowed-once') return notApproved(outcome, `send a message to ${friend.name}`, decision.reason)
        const { entry, receipt } = await sendAndArchive(net, friend.fp, args.body)
        ctx.logger.info(`soulmirror-tools: sent ${receipt.id} to ${friend.fp} (${receipt.status}; gate=${decision.reason}; approved)`)
        return { ok: true, outcome: 'sent', approval: outcome, gate: decision.reason, auto: false, id: receipt.id, seq: receipt.seq, status: receipt.status, to: friendView(friend), entry }
      }
      const auto = decision.auto
      const { entry, receipt } = await sendAndArchive(net, friend.fp, args.body, auto ? { auto: true } : undefined)
      if (face !== undefined) {
        if (auto) face.autoReplies.record(friend.fp)
        face.emit({ kind: 'outbound', fp: friend.fp, entry })
      }
      ctx.logger.info(`soulmirror-tools: sent ${receipt.id} to ${friend.fp} (${receipt.status}; gate=${decision.reason}${auto ? '; auto' : ''})`)
      return { ok: true, outcome: 'sent', gate: decision.reason, auto, id: receipt.id, seq: receipt.seq, status: receipt.status, to: friendView(friend) }
    },
  })

  const sendGroup = defineTool({
    name: 'soulmirror_send_group_message',
    description: 'Post a text message into one of the user\'s SoulMirror groups (by gid) as the user\'s alter. On the user\'s instruction the post goes out directly. When you are answering a group message, the group profile\'s agent tier decides: "auto" posts directly (capped by the group\'s hourly and round limits, over-cap posts become drafts), "draft" stores a PENDING DRAFT for the user\'s review (outcome "draft-queued": nothing was sent yet — do not call the tool again for the same reply), "notify" refuses (the alter does not speak in that group). Fails if the gid is not one of the user\'s groups.',
    parameters: {
      gid: { type: 'string', description: 'Group id (from the group roster or the message that woke you).' },
      body: { type: 'string', description: 'Message text.' },
    },
    output: { type: 'object' },
    async execute(args, exec) {
      const info = await net.groups.info(args.gid)
      const profile = groupProfileOf(info)
      const face = seams.sessions()
      const trigger = face === undefined || exec.agent === undefined ? UNKNOWN_TRIGGER : face.triggerOf(exec.agent.id)
      // Which of the seat's voices is calling: a named seat agent stamps its provenance; the alter (or an unknown session) does not.
      const voice = face === undefined || exec.agent === undefined ? undefined : face.voiceOf(exec.agent.id)
      const agentName = voice?.kind === 'agent' ? voice.agent.name : undefined
      const ownFp = face?.ownerFp() ?? ''
      // The mechanical caps (autoPerHour, agentRounds) read the group archive;
      // an unreachable archive counts as empty and the gate still applies the
      // tier and profile switches.
      let entries: readonly ConversationEntry[] = []
      try {
        entries = (await net.groups.conversation(args.gid, { limit: 200 })).entries
      } catch {
        // best effort
      }
      const caps: GroupCapEntry[] = entries.map((e) => {
        const by = entryBy(e)
        return { dir: e.dir, ts: e.ts, ...(e.auto === true ? { auto: true } : {}), ...(by === undefined ? {} : { by }) }
      })
      const triggerEntry = trigger.messageId === undefined ? undefined : entries.find(e => e.id === trigger.messageId)
      let myName = ''
      try {
        myName = (await net.identity())?.name ?? ''
      } catch {
        // no identity name: the mention exception simply cannot fire
      }
      const decision = groupSendGate({
        trigger,
        gid: args.gid,
        // Own-post hook: the owner mentioned this voice in their OWN group post — an instruction in group clothing, sends directly.
        fromOwner: trigger.kind === 'group' && trigger.fp !== undefined && ownFp !== '' && trigger.fp === ownFp,
        speakAgents: profile.speakAgents,
        // A named seat agent without the approval switch answers directly (its 'draft' lifts to 'auto'); the alter keeps the group tier.
        tier: voice?.kind === 'agent' ? effectiveAgentTier(profile.agentTier, voice.agent.approval === true) : profile.agentTier,
        autoSentInWindow: countAutoInWindow(caps),
        autoPerHour: profile.autoPerHour,
        roundsExceeded: agentRoundsExceeded(caps, profile.agentRounds),
        mentioned: triggerEntry !== undefined && (agentName !== undefined ? mentionsAgent(triggerEntry.body, agentName) : mentionsMe(triggerEntry.body, myName)),
      })
      const to = { gid: args.gid, name: info.name }
      if (decision.kind === 'refuse') {
        ctx.logger.info(`soulmirror-tools: group post to ${args.gid} refused (gate=${decision.reason})`)
        return {
          ok: false,
          outcome: 'refused',
          gate: decision.reason,
          to,
          message: decision.reason === 'agents-muted'
            ? `Agents do not speak in group "${info.name}" (the group profile mutes them); nothing can be posted.`
            : `The agent tier of group "${info.name}" is "notify": you do not post there on your own. Tell the user what happened in the group and what you would say; only the user's own instruction can have it posted.`,
        }
      }
      if (decision.kind === 'draft') {
        if (face !== undefined) {
          const draft = await face.queueDraft({ fp: args.gid as Fingerprint, gid: args.gid, name: info.name, body: args.body, reason: decision.reason, trigger, ...(exec.agent === undefined ? {} : { sessionId: exec.agent.id }), ...(agentName === undefined ? {} : { agent: agentName }) })
          ctx.logger.info(`soulmirror-tools: group draft ${draft.id} queued for ${args.gid} (gate=${decision.reason})`)
          return {
            ok: true,
            outcome: 'draft-queued',
            gate: decision.reason,
            auto: false,
            draftId: draft.id,
            to,
            message: `Nothing was sent: your post to group "${info.name}" is stored as draft ${draft.id} and waits for the user's review on the SoulMirror page (reason: ${decision.reason}). Tell the user in one line; do not call this tool again for the same reply.`,
          }
        }
        // No sessions face (no drafts store): fall back to dsh's approval seam, fail closed.
        const preview = args.body.length > 200 ? `${args.body.slice(0, 200)}…` : args.body
        const outcome = await approve(exec, 'soulmirror_send_group_message', `Post to group ${info.name} (${args.gid}): "${preview}"`)
        if (outcome !== 'allowed-once') return notApproved(outcome, `post to group ${info.name}`, decision.reason)
        const receipt = await sendGroupMessage(net, args.gid, args.body, { by: 'alter', ...(agentName === undefined ? {} : { agent: agentName }) })
        ctx.logger.info(`soulmirror-tools: posted ${receipt.id} into group ${args.gid} (${receipt.status}; gate=${decision.reason}; approved)`)
        return { ok: true, outcome: 'sent', approval: outcome, gate: decision.reason, auto: false, id: receipt.id, seq: receipt.seq, status: receipt.status, to }
      }
      const receipt = await sendGroupMessage(net, args.gid, args.body, { by: 'alter', ...(decision.auto ? { auto: true } : {}), ...(agentName === undefined ? {} : { agent: agentName }) })
      if (face !== undefined) {
        const entry: ConversationEntry = { seq: receipt.seq ?? 0, dir: 'out', id: receipt.id, body: args.body, ts: Date.now(), status: receipt.status, ...(decision.auto ? { auto: true as const } : {}), ...(agentName === undefined ? {} : { agent: agentName }) }
        face.emit({ kind: 'outbound', fp: args.gid as Fingerprint, gid: args.gid, entry })
        if (agentName !== undefined) {
          // Conversation receipts: whom did this post address? Their next
          // agent-authored post wakes this agent even without an @ back.
          const expects: { fp: string; token: string }[] = []
          for (const m of info.memberList ?? []) {
            if (ownFp !== '' && m.fp === ownFp) continue
            for (const token of [m.name, ...(m.agents ?? [])]) {
              if (token !== '' && mentionsAgent(args.body, token)) expects.push({ fp: m.fp, token })
            }
          }
          face.noteAwaitReply(args.gid, agentName, expects)
        }
      }
      ctx.logger.info(`soulmirror-tools: posted ${receipt.id} into group ${args.gid} (${receipt.status}; gate=${decision.reason}${decision.auto ? '; auto' : ''})`)
      return { ok: true, outcome: 'sent', gate: decision.reason, auto: decision.auto, id: receipt.id, seq: receipt.seq, status: receipt.status, to }
    },
  })

  const readConversation = defineTool({
    name: 'soulmirror_read_conversation',
    description: 'Read the archived SoulMirror conversation with one friend (by fingerprint): entries with seq, direction (in = from the friend, out = from this user), timestamp and body. Use `since` (a seq) to read only newer entries, `limit` for the last N. Read-only.',
    parameters: {
      fingerprint: { type: 'string', description: 'Fingerprint of the friend.' },
      since: { type: 'integer', description: 'Return only entries with seq greater than this.', optional: true },
      limit: { type: 'integer', description: 'Keep only the last N entries (default 50).', optional: true },
    },
    output: { type: 'object' },
    async execute(args) {
      const friend = await requireFriend(args.fingerprint)
      const { entries, typing } = await net.conversation(friend.fp, {
        ...(args.since === undefined ? {} : { since: args.since }),
        limit: args.limit ?? 50,
      })
      return {
        friend: friendView(friend),
        typing,
        entries: entries.map(e => ({
          seq: e.seq,
          dir: e.dir,
          at: new Date(e.ts).toISOString(),
          body: e.body,
          ...(e.type === undefined ? {} : { type: e.type }),
          ...(e.auto === true ? { auto: true } : {}),
          ...(e.status === undefined ? {} : { status: e.status }),
          ...(e.artifactName === undefined ? {} : { attachment: e.artifactName }),
        })),
      }
    },
  })

  const remember = defineTool({
    name: 'soulmirror_remember',
    description: 'Save one long-term memory the user just told you — a fact about them, a preference, a decision, or a promise. Call this whenever the user says something worth keeping for later (in the language they used). Do NOT claim you remembered something without actually calling this tool.',
    parameters: {
      content: { type: 'string', description: 'One concrete, self-contained sentence (in the user\'s language) capturing what to remember.' },
      kind: { type: 'string', description: 'fact | preference | decision | promise | summary (default fact).', optional: true },
    },
    output: { type: 'object' },
    async execute(args, exec) {
      const face = seams.sessions()
      if (face === undefined || exec.agent === undefined) {
        return { ok: false, message: 'memory store is unavailable in this session.' }
      }
      const content = args.content.trim()
      if (content === '') return { ok: false, message: 'content must not be empty.' }
      const voice = face.voiceOf(exec.agent.id)
      const trigger = face.triggerOf(exec.agent.id)
      // 归属：alter → global；seat agent 群会话 → shared-group:<gid>；seat agent 直聊 → agent:<name>。
      let scope: MemoryScope
      if (voice?.kind === 'agent') {
        scope = trigger.kind === 'group' && trigger.gid !== undefined
          ? { kind: 'shared-group', gid: trigger.gid }
          : { kind: 'agent', name: voice.agent.name }
      } else {
        scope = { kind: 'global' }
      }
      const kinds: MemoryKind[] = ['fact', 'preference', 'decision', 'promise', 'summary']
      const kind: MemoryKind = kinds.includes(args.kind as MemoryKind) ? args.kind as MemoryKind : 'fact'
      const record = face.memoryRemember({ kind, content, scope })
      face.emit({ kind: 'memory', phase: 'extracted', count: 1, memories: [{ id: record.uid, content: record.content }] })
      ctx.logger.info(`soulmirror-tools: remembered -> ${scope.kind} : ${content}`)
      return { ok: true, id: record.uid, scope: scope.kind, content: record.content }
    },
  })

  for (const tool of [friends, card, addFriend, sendMessage, sendGroup, readConversation, remember]) ctx.tools.register(tool)
  ctx.logger.info('soulmirror-tools: registered soulmirror_friends, soulmirror_card, soulmirror_add_friend, soulmirror_send_message, soulmirror_send_group_message, soulmirror_read_conversation, soulmirror_remember')
}
