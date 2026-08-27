import { describe, expect, it } from 'vitest'
import { paidJoinConfig, paidJoinFromRules, rulesWithPaidJoin, rulesWithoutPaidJoin, normalizeGroupProfile } from '../src/group-contract.ts'

describe('paid-join rules marker (old-relay compatible encoding)', () => {
  it('round trips price + address through rules', () => {
    const rules = rulesWithPaidJoin('be kind', '1.00', '0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29')
    expect(paidJoinFromRules(rules)).toEqual({ price: '1.00', addr: '0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29' })
    // marker is appended; user text preserved (minus marker when stripped)
    expect(rulesWithoutPaidJoin(rules)).toBe('be kind')
  })

  it('replaces an existing marker on re-pricing', () => {
    const rules = rulesWithPaidJoin('be kind', '1.00', '0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29')
    const again = rulesWithPaidJoin(rules, '2.50', '0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29')
    expect(paidJoinFromRules(again)?.price).toBe('2.50')
    expect(rulesWithoutPaidJoin(again)).toBe('be kind')
  })

  it('treats a rules-marker group as join=paid in the profile view', () => {
    const view = normalizeGroupProfile({ join: 'apply', rules: rulesWithPaidJoin('be kind', '0.1', '0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29') })
    expect(view.join).toBe('paid')
    expect(view.paidJoin).toEqual({ price: '0.1', addr: '0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29' })
  })

  it('strips the marker from the displayed rules', () => {
    const rules = rulesWithPaidJoin('be kind', '1.00', '0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29')
    expect(rulesWithoutPaidJoin(rules)).not.toContain('#paid-join')
  })
})

describe('paidJoinConfig (published config the owner verifies against)', () => {
  const addr = '0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29'

  it('reads native join=paid price + address', () => {
    expect(paidJoinConfig({ join: 'paid', joinPrice: '1.00', joinAddr: addr })).toEqual({ price: '1.00', addr })
  })

  it('falls back to the rules marker for compat-encoded groups', () => {
    expect(paidJoinConfig({ join: 'apply', rules: rulesWithPaidJoin('be kind', '0.1', addr) })).toEqual({ price: '0.1', addr })
  })

  it('returns undefined for non-paid groups', () => {
    expect(paidJoinConfig({ join: 'apply', rules: 'be kind' })).toBeUndefined()
    expect(paidJoinConfig(undefined)).toBeUndefined()
  })

  it('prefers native values over the rules marker', () => {
    const cfg = paidJoinConfig({ join: 'paid', joinPrice: '2.00', joinAddr: addr, rules: rulesWithPaidJoin('x', '0.1', '0x1111111111111111111111111111111111111111') })
    expect(cfg).toEqual({ price: '2.00', addr })
  })

  it('ignores a rules marker whose address is not 0x', () => {
    expect(paidJoinConfig({ join: 'apply', rules: '#paid-join 1.00 not-an-address' })).toBeUndefined()
  })
})
