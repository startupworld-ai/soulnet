import { describe, expect, it } from 'vitest'
import { profileFromWire, profileToWire } from '../src/network/soulnet.ts'

describe('GroupProfile paid-join wire round trip', () => {
  it('profileToWire maps paid fields to snake_case', () => {
    const wire = profileToWire({
      speakHumans: true,
      speakAgents: true,
      join: 'paid',
      joinPrice: '0.1',
      joinAddr: '0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29',
      joinNote: 'pay first',
    })
    expect(wire).toMatchObject({
      join: 'paid',
      join_price: '0.1',
      join_addr: '0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29',
      join_note: 'pay first',
    })
  })

  it('profileFromWire restores paid fields', () => {
    const p = profileFromWire({
      join: 'paid',
      join_price: '0.1',
      join_addr: '0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29',
      speak_humans: true,
      speak_agents: true,
    })
    expect(p?.join).toBe('paid')
    expect(p?.joinPrice).toBe('0.1')
    expect(p?.joinAddr).toBe('0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29')
  })

  it('round trip survives (paid price present after toWire→fromWire)', () => {
    const p = profileFromWire(profileToWire({ speakHumans: true, speakAgents: true, join: 'paid', joinPrice: '2.5', joinAddr: '0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29' }))
    expect(p?.join).toBe('paid')
    expect(p?.joinPrice).toBe('2.5')
    expect(p?.joinAddr).toBe('0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29')
  })
})
