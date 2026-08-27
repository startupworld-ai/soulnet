/**
 * Groups §14.7 wire mapping (src/network/soulnet.ts): the snake_case Go
 * `a2a.GroupProfile` ↔ the camelCase client type, and the group.get extras
 * (pins, my_role, applications).
 */
import { describe, expect, it } from 'vitest'
import { groupFromWire, groupInfoFromWire, profileFromWire, profileToWire } from '../src/network/soulnet.ts'
import type { GroupProfile } from '../src/network/types.ts'

const wireProfile = {
  template: 'standard', room: 'chat', speak_humans: true, speak_agents: true,
  speak_who: 'all', join: 'apply', agent_wake: 'mention', agent_tier: 'draft',
  auto_per_hour: 10, agent_rounds: 3, admins: ['fp-a'], public: true,
  tags: ['tasks'], rules: '# be kind',
}

describe('profileFromWire', () => {
  it('maps a snake_case object to camelCase', () => {
    expect(profileFromWire(wireProfile)).toEqual({
      template: 'standard', room: 'chat', speakHumans: true, speakAgents: true,
      speakWho: 'all', join: 'apply', agentWake: 'mention', agentTier: 'draft',
      autoPerHour: 10, agentRounds: 3, admins: ['fp-a'], public: true,
      tags: ['tasks'], rules: '# be kind',
    })
  })

  it('accepts the profile as a JSON string (the Go struct serialized into the row)', () => {
    expect(profileFromWire(JSON.stringify(wireProfile))).toEqual(profileFromWire(wireProfile))
  })

  it('answers undefined for absent / empty / unparseable values (legacy groups)', () => {
    expect(profileFromWire(undefined)).toBeUndefined()
    expect(profileFromWire('')).toBeUndefined()
    expect(profileFromWire('not json')).toBeUndefined()
    expect(profileFromWire(42)).toBeUndefined()
  })

  it('drops zero/empty defaults and out-of-vocabulary enum values', () => {
    const p = profileFromWire({ speak_humans: true, speak_agents: false, auto_per_hour: 0, speak_who: 'everyone', join: '' })
    expect(p).toEqual({ speakHumans: true, speakAgents: false })
  })
})

describe('profileToWire', () => {
  it('round-trips through the wire form', () => {
    const p: GroupProfile = {
      template: 'casual', room: 'chat', speakHumans: true, speakAgents: true,
      speakWho: 'admins', join: 'open', agentWake: 'always', agentTier: 'auto',
      autoPerHour: 60, agentRounds: 10, admins: ['fp-x'], public: true, tags: ['fun'], rules: 'r',
    }
    expect(profileFromWire(profileToWire(p))).toEqual(p)
  })

  it('maps native paid join (join=paid + join_price/join_addr) both ways', () => {
    const p: GroupProfile = {
      speakHumans: true, speakAgents: true, join: 'paid',
      joinPrice: '0.1', joinAddr: '0xD5D21E129B422491cfF103bA875c60dabec02899',
      public: true, rules: 'be kind',
    }
    const wire = profileToWire(p)
    expect(wire['join']).toBe('paid')
    expect(wire['join_price']).toBe('0.1')
    expect(wire['join_addr']).toBe('0xD5D21E129B422491cfF103bA875c60dabec02899')
    // native values survive the round trip (no rules-marker encoding needed)
    const back = profileFromWire(wire)
    expect(back?.join).toBe('paid')
    expect(back?.joinPrice).toBe('0.1')
    expect(back?.joinAddr).toBe('0xD5D21E129B422491cfF103bA875c60dabec02899')
    expect(back?.rules).toBe('be kind')
  })
})

describe('groupFromWire / groupInfoFromWire', () => {
  const row = { gid: 'g1', name: 'G', owner_fp: 'fp-o', mine: true, version: 3, members: 2, unread: 0, count: 5, profile: wireProfile }

  it('attaches the mapped profile to the row', () => {
    const g = groupFromWire(row)
    expect(g.profile?.join).toBe('apply')
    expect(groupFromWire({ gid: 'g2' }).profile).toBeUndefined()
  })

  it('maps pins (ts to ms), my_role, applications', () => {
    const info = groupInfoFromWire({
      ...row,
      member_list: [{ fp: 'fp-o', name: 'Owner' }, { fp: 'fp-m', name: 'Member' }],
      pins: [{ id: 'p1', from: 'fp-o', ts: '2026-08-24T10:00:00Z', body: 'welcome' }],
      my_role: 'owner',
      applications: [{ fp: 'fp-dave-000011112222', name: '', note: 'hi', ts: 1700000000000 }],
    })
    expect(info.myRole).toBe('owner')
    expect(info.pins).toEqual([{ id: 'p1', from: 'fp-o', ts: Date.parse('2026-08-24T10:00:00Z'), body: 'welcome' }])
    expect(info.applications).toHaveLength(1)
    expect(info.applications?.[0]).toMatchObject({ fp: 'fp-dave-000011112222', note: 'hi', ts: 1700000000000 })
    expect(info.applications?.[0]?.name).not.toBe('') // falls back to the short fingerprint
  })

  it('falls back my_role from `mine` when absent', () => {
    expect(groupInfoFromWire({ gid: 'g', mine: true }).myRole).toBe('owner')
    expect(groupInfoFromWire({ gid: 'g', mine: false }).myRole).toBe('member')
    expect(groupInfoFromWire({ gid: 'g', mine: false, my_role: 'admin' }).myRole).toBe('admin')
  })
})
