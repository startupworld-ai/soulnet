import { describe, expect, it } from 'vitest'
import { DEFAULT_PAYGATE_PORT, resolveSettings } from '../src/settings.ts'

describe('resolveSettings paygate/CDP defaults', () => {
  it('fills paygate + CDP defaults for an empty section', () => {
    const s = resolveSettings(undefined)
    expect(s.paygateBinary).toBe('')
    expect(s.paygatePort).toBe(DEFAULT_PAYGATE_PORT)
    expect(s.cdpKeyId).toBe('')
    expect(s.cdpKeySecret).toBe('')
    expect(s.cdpWalletSecret).toBe('')
    expect(s.cdpNetwork).toBe('base-sepolia')
  })

  it('normalizes cdpNetwork and keeps provided CDP secrets', () => {
    const s = resolveSettings({
      cdpNetwork: 'base' as const,
      cdpKeyId: 'organizations/x/apiKeys/y',
      cdpKeySecret: 'secret',
      cdpWalletSecret: 'wallet',
      paygatePort: 0, // invalid → default
      paygateBinary: '/usr/local/bin/paygate',
    })
    expect(s.cdpNetwork).toBe('base')
    expect(s.cdpKeyId).toBe('organizations/x/apiKeys/y')
    expect(s.cdpKeySecret).toBe('secret')
    expect(s.cdpWalletSecret).toBe('wallet')
    expect(s.paygatePort).toBe(DEFAULT_PAYGATE_PORT)
    expect(s.paygateBinary).toBe('/usr/local/bin/paygate')
  })

  it('keeps older fields intact', () => {
    const s = resolveSettings({ defaultTier: 'auto', autoReplyPerHour: 3, directSend: true })
    expect(s.defaultTier).toBe('auto')
    expect(s.autoReplyPerHour).toBe(3)
    expect(s.directSend).toBe(true)
    expect(s.paygatePort).toBe(DEFAULT_PAYGATE_PORT)
  })
})

import { paygatePlatformPackageName } from '../src/network/paygate.ts'

describe('paygatePlatformPackageName', () => {
  it('names the platform package for supported pairs', () => {
    expect(paygatePlatformPackageName('darwin', 'arm64')).toBe('soulnet-paygate-darwin-arm64')
    expect(paygatePlatformPackageName('win32', 'x64')).toBe('soulnet-paygate-windows-x64')
    expect(paygatePlatformPackageName('linux', 'arm64')).toBe('soulnet-paygate-linux-arm64')
  })

  it('answers undefined for unsupported platforms', () => {
    expect(paygatePlatformPackageName('freebsd', 'x64')).toBeUndefined()
    expect(paygatePlatformPackageName('darwin', 'mips')).toBeUndefined()
  })
})
