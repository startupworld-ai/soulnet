/**
 * Group gates (wire spec §14.7, src/policy.ts + src/group-contract.ts):
 * mention parsing (unicode names included), the wake matrix, tier resolution
 * through groupSendGate, autoPerHour window math, the agentRounds tail rule
 * and the profile normalization defaults.
 */
import { describe, expect, it } from 'vitest'
import { DEFAULT_GROUP_AGENT_ROUNDS, DEFAULT_GROUP_AUTO_PER_HOUR, normalizeGroupProfile } from '../src/group-contract.ts'
import { agentRoundsExceeded, countAutoInWindow, groupSendGate, HOUR_MS, mentionsMe, sendGate, wakeForGroup, type GroupCapEntry, type GroupSendGateInput, type TurnTrigger } from '../src/policy.ts'

const GID = 'a'.repeat(32)
const OTHER_GID = 'b'.repeat(32)
// "Xiao Ming" in CJK, built from char codes (the repository is English-only on source bytes).
const CJK_NAME = String.fromCharCode(0x5c0f, 0x660e)
// "ni hao" (hello), appended right after a mention with no separator.
const CJK_HELLO = String.fromCharCode(0x4f60, 0x597d)

const owner: TurnTrigger = { kind: 'owner' }
const fromGroup: TurnTrigger = { kind: 'group', gid: GID, fp: 'fp-bob', name: 'Bob', messageId: 'm1' }
const fromOtherGroup: TurnTrigger = { kind: 'group', gid: OTHER_GID, fp: 'fp-bob', name: 'Bob', messageId: 'm2' }
const fromFriend: TurnTrigger = { kind: 'inbound', fp: 'fp-bob', name: 'Bob', messageId: 'm3' }

describe('mentionsMe', () => {
  it('matches @name case-insensitively', () => {
    expect(mentionsMe('hey @bob, what do you think?', 'Bob')).toBe(true)
    expect(mentionsMe('hey @BOB!', 'bob')).toBe(true)
    expect(mentionsMe('hey bob', 'Bob')).toBe(false)
    expect(mentionsMe('', 'Bob')).toBe(false)
  })

  it('requires a word boundary after an ASCII name (@ann must not fire on @anne)', () => {
    expect(mentionsMe('ping @anne about it', 'ann')).toBe(false)
    expect(mentionsMe('ping @ann about it', 'ann')).toBe(true)
    expect(mentionsMe('ping @ann, thanks', 'ann')).toBe(true)
    expect(mentionsMe('(@ann)', 'ann')).toBe(true)
    expect(mentionsMe('@ann', 'ann')).toBe(true)
    // a later clean occurrence still matches
    expect(mentionsMe('@anne and @ann', 'ann')).toBe(true)
  })

  it('matches unicode names without a trailing boundary (no spaces in such scripts)', () => {
    expect(mentionsMe(`@${CJK_NAME}${CJK_HELLO}`, CJK_NAME)).toBe(true)
    expect(mentionsMe(`hello @${CJK_NAME}`, CJK_NAME)).toBe(true)
    expect(mentionsMe('hello all', CJK_NAME)).toBe(false)
  })

  it('matches @all for everyone, with a boundary (@allison is not @all)', () => {
    expect(mentionsMe('@all standup in 5', 'whoever')).toBe(true)
    expect(mentionsMe('@ALL', '')).toBe(true)
    expect(mentionsMe('ping @allison', 'bob')).toBe(false)
    expect(mentionsMe('no mention', '')).toBe(false)
  })

  it('ignores an empty / whitespace name (only @all can match then)', () => {
    expect(mentionsMe('@ hello', '  ')).toBe(false)
  })
})

describe('wakeForGroup (wake matrix)', () => {
  const base = { speakAgents: true, enabled: true, fromSelf: false, wake: 'mention' as const, mentioned: true }

  it('wakes only when every condition holds', () => {
    expect(wakeForGroup(base)).toEqual({ wake: true })
    expect(wakeForGroup({ ...base, fromSelf: true })).toEqual({ wake: false, reason: 'self' })
    expect(wakeForGroup({ ...base, speakAgents: false })).toEqual({ wake: false, reason: 'agents-muted' })
    expect(wakeForGroup({ ...base, enabled: false })).toEqual({ wake: false, reason: 'alter-disabled' })
  })

  it('applies the wake policy: always / never / mention, tightened by my strategy', () => {
    // My default strategy is 'mention': even an always-group only wakes me when named.
    expect(wakeForGroup({ ...base, wake: 'always', mentioned: false })).toEqual({ wake: false, reason: 'not-mentioned' })
    expect(wakeForGroup({ ...base, wake: 'always', mentioned: true })).toEqual({ wake: true })
    // Opting into 'always' follows the group policy.
    expect(wakeForGroup({ ...base, wake: 'always', myMode: 'always', mentioned: false })).toEqual({ wake: true })
    // My strategy can never LOOSEN the group's ceiling.
    expect(wakeForGroup({ ...base, wake: 'mention', myMode: 'always', mentioned: false })).toEqual({ wake: false, reason: 'not-mentioned' })
    expect(wakeForGroup({ ...base, wake: 'never', myMode: 'always', mentioned: true })).toEqual({ wake: false, reason: 'wake-never' })
    expect(wakeForGroup({ ...base, wake: 'mention', mentioned: false })).toEqual({ wake: false, reason: 'not-mentioned' })
    expect(wakeForGroup({ ...base, wake: 'mention', mentioned: true })).toEqual({ wake: true })
  })
})

describe('countAutoInWindow (autoPerHour window math)', () => {
  const t0 = 10 * HOUR_MS
  const e = (patch: Partial<GroupCapEntry> & { ts: number }): GroupCapEntry => ({ dir: 'out', auto: true, by: 'alter', ...patch })

  it('counts only my automatic alter posts inside the window', () => {
    const entries: GroupCapEntry[] = [
      e({ ts: t0 - HOUR_MS }), // exactly one hour old: outside
      e({ ts: t0 - HOUR_MS + 1 }), // just inside
      e({ ts: t0 - 1 }),
      e({ ts: t0 - 10, dir: 'in' }), // someone else's post
      e({ ts: t0 - 10, auto: false }), // my manual post
      { dir: 'out', ts: t0 - 10, auto: true, by: 'owner' }, // human-flagged: not the alter
      { dir: 'out', ts: t0 - 10, auto: true }, // no provenance: not the alter
    ]
    expect(countAutoInWindow(entries, t0)).toBe(2)
    expect(countAutoInWindow([], t0)).toBe(0)
  })

  it('honours a custom window and ignores future timestamps', () => {
    expect(countAutoInWindow([e({ ts: 50 }), e({ ts: 150 })], 100, 100)).toBe(1)
    expect(countAutoInWindow([e({ ts: 200 })], 100)).toBe(0)
  })
})

describe('agentRoundsExceeded (tail rule)', () => {
  const alter = (ts: number): GroupCapEntry => ({ dir: 'in', ts, by: 'alter' })
  const human = (ts: number): GroupCapEntry => ({ dir: 'in', ts })

  it('trips when the last K consecutive entries are all agent posts', () => {
    expect(agentRoundsExceeded([alter(1), alter(2), alter(3)], 3)).toBe(true)
    expect(agentRoundsExceeded([human(0), alter(1), alter(2), alter(3)], 3)).toBe(true)
    expect(agentRoundsExceeded([alter(1), alter(2)], 3)).toBe(false)
    expect(agentRoundsExceeded([alter(1), human(2), alter(3), alter(4)], 3)).toBe(false)
  })

  it('a human message at the very tail resets the streak', () => {
    expect(agentRoundsExceeded([alter(1), alter(2), alter(3), human(4)], 3)).toBe(false)
    expect(agentRoundsExceeded([alter(1), alter(2), alter(3), { dir: 'in', ts: 4, by: 'owner' }], 3)).toBe(false)
  })

  it('never trips on an empty archive or a non-positive rounds value', () => {
    expect(agentRoundsExceeded([], 3)).toBe(false)
    expect(agentRoundsExceeded([alter(1)], 0)).toBe(false)
    expect(agentRoundsExceeded([alter(1)], -1)).toBe(false)
    expect(agentRoundsExceeded([alter(1)], 1)).toBe(true)
  })
})

describe('groupSendGate (tier resolution + caps)', () => {
  const base: GroupSendGateInput = {
    trigger: fromGroup, gid: GID, speakAgents: true, tier: 'auto',
    autoSentInWindow: 0, autoPerHour: 10, roundsExceeded: false, mentioned: false,
  }

  it('refuses whenever agents are muted, whatever the trigger', () => {
    expect(groupSendGate({ ...base, speakAgents: false })).toEqual({ kind: 'refuse', reason: 'agents-muted' })
    expect(groupSendGate({ ...base, speakAgents: false, trigger: owner })).toEqual({ kind: 'refuse', reason: 'agents-muted' })
  })

  it('owner-instructed turns post directly (not flagged auto) in every tier', () => {
    for (const tier of ['notify', 'draft', 'auto'] as const) {
      expect(groupSendGate({ ...base, trigger: owner, tier })).toEqual({ kind: 'allow', auto: false, reason: 'owner-initiated' })
    }
  })

  it('group-triggered turns resolve the profile agentTier', () => {
    expect(groupSendGate({ ...base, tier: 'notify' })).toEqual({ kind: 'refuse', reason: 'notify-tier' })
    // The pure gate keeps 'draft' drafting — but the tool resolves the tier
    // through effectiveAgentTier first, so only a seat agent with its approval
    // switch on ever passes 'draft' in here; the alter's group replies arrive
    // as 'auto' and go out directly (see tools-gate.test.ts).
    expect(groupSendGate({ ...base, tier: 'draft' })).toEqual({ kind: 'draft', reason: 'draft-tier' })
    expect(groupSendGate({ ...base, tier: 'auto' })).toEqual({ kind: 'allow', auto: true, reason: 'auto-tier' })
  })

  it('the autoPerHour cap turns auto posts into drafts (0 disables auto entirely)', () => {
    expect(groupSendGate({ ...base, autoSentInWindow: 9 })).toEqual({ kind: 'allow', auto: true, reason: 'auto-tier' })
    expect(groupSendGate({ ...base, autoSentInWindow: 10 })).toEqual({ kind: 'draft', reason: 'rate-limited' })
    expect(groupSendGate({ ...base, autoPerHour: 0 })).toEqual({ kind: 'draft', reason: 'rate-limited' })
  })

  it('the agentRounds tail rule drafts auto posts unless the triggering message mentioned me', () => {
    expect(groupSendGate({ ...base, roundsExceeded: true })).toEqual({ kind: 'draft', reason: 'agent-rounds' })
    expect(groupSendGate({ ...base, roundsExceeded: true, mentioned: true })).toEqual({ kind: 'allow', auto: true, reason: 'auto-tier' })
  })

  it('other triggers draft: another group, a friend turn, an unknown turn', () => {
    expect(groupSendGate({ ...base, trigger: fromOtherGroup })).toEqual({ kind: 'draft', reason: 'other-group' })
    expect(groupSendGate({ ...base, trigger: fromFriend })).toEqual({ kind: 'draft', reason: 'other-group' })
    expect(groupSendGate({ ...base, trigger: { kind: 'unknown' } })).toEqual({ kind: 'draft', reason: 'unknown-trigger' })
  })
})

describe('sendGate on a group-triggered turn (friend tool)', () => {
  it('drafts a private send during a group turn (the group tool answers groups)', () => {
    expect(sendGate({ trigger: fromGroup, target: 'fp-bob', tier: 'auto', autoSentInWindow: 0, limit: 20 })).toEqual({ kind: 'draft', reason: 'group-trigger' })
  })
})

describe('normalizeGroupProfile (contract defaults)', () => {
  it('a missing profile means the standard template', () => {
    expect(normalizeGroupProfile(undefined)).toEqual({
      speakHumans: true, speakAgents: true, speakWho: 'all', agentWake: 'mention', agentTier: 'draft',
      autoPerHour: DEFAULT_GROUP_AUTO_PER_HOUR, agentRounds: DEFAULT_GROUP_AGENT_ROUNDS, rules: '', admins: [], room: 'chat', join: 'invite',
    })
  })

  it('keeps explicit values and normalizes garbage back to the defaults', () => {
    const p = normalizeGroupProfile({
      speakHumans: false, speakAgents: true, speakWho: 'admins', agentWake: 'always', agentTier: 'auto',
      autoPerHour: 5.9, agentRounds: 2, rules: 'be brief', admins: ['fp-a', 7], room: 'chat',
    })
    expect(p).toMatchObject({ speakHumans: false, speakWho: 'admins', agentWake: 'always', agentTier: 'auto', autoPerHour: 5, agentRounds: 2, rules: 'be brief', admins: ['fp-a'] })
    const bad = normalizeGroupProfile({ speakWho: 'nobody', agentWake: 'loud', agentTier: 'shout', autoPerHour: -1, agentRounds: 0, rules: 7, room: '' })
    expect(bad).toMatchObject({ speakWho: 'all', agentWake: 'mention', agentTier: 'draft', autoPerHour: DEFAULT_GROUP_AUTO_PER_HOUR, agentRounds: DEFAULT_GROUP_AGENT_ROUNDS, rules: '', room: 'chat' })
  })
})
