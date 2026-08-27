/**
 * Self-upgrade host logic (src/api/upgrade.ts): version comparison, the
 * npmjs → npmmirror registry fallback, the package/version injection gate,
 * profile-directory resolution and the pnpm invocation — all with the network
 * and child_process mocked.
 */
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { describe, expect, it, vi } from 'vitest'
import {
  checkUpgrade,
  compareVersions,
  fetchLatest,
  isValidVersion,
  ownVersion,
  resolveWebProfileDir,
  runUpgrade,
  UPGRADE_PACKAGE,
  UPGRADE_REGISTRIES,
} from '../src/api/upgrade.ts'

const okResponse = (latest: string) => ({
  ok: true,
  status: 200,
  json: () => Promise.resolve({ 'dist-tags': { latest } }),
})

describe('isValidVersion (the injection gate)', () => {
  it('accepts strict semver', () => {
    for (const v of ['0.1.3', '1.0.0', '10.20.30', '1.0.0-rc.1', '0.2.0-beta-2.x']) {
      expect(isValidVersion(v), v).toBe(true)
    }
  })
  it('rejects everything a command line must never see', () => {
    for (const v of ['', '1.2', 'v1.2.3', '1.2.3 ', ' 1.2.3', '01.2.3', '1.2.3+build', 'latest', '^1.2.3', '~1.2.3', '1.2.3;rm -rf /', '1.2.3 && echo pwned', '1.2.3`x`', '1.2.3$(x)', 'file:../evil', 'https://evil/x.tgz', '../../x']) {
      expect(isValidVersion(v), JSON.stringify(v)).toBe(false)
    }
  })
})

describe('compareVersions', () => {
  it('orders numerically, not lexically', () => {
    expect(compareVersions('0.1.10', '0.1.9')).toBeGreaterThan(0)
    expect(compareVersions('0.2.0', '0.1.3')).toBeGreaterThan(0)
    expect(compareVersions('1.0.0', '0.99.99')).toBeGreaterThan(0)
    expect(compareVersions('0.1.3', '0.1.3')).toBe(0)
    expect(compareVersions('0.1.2', '0.1.3')).toBeLessThan(0)
  })
  it('ranks prereleases below their release', () => {
    expect(compareVersions('1.0.0-rc.1', '1.0.0')).toBeLessThan(0)
    expect(compareVersions('1.0.0', '1.0.0-rc.1')).toBeGreaterThan(0)
    expect(compareVersions('1.0.0-rc.2', '1.0.0-rc.1')).toBeGreaterThan(0)
    expect(compareVersions('1.0.0-rc.1.1', '1.0.0-rc.1')).toBeGreaterThan(0)
    expect(compareVersions('1.0.0-alpha', '1.0.0-beta')).toBeLessThan(0)
    expect(compareVersions('1.0.0-2', '1.0.0-11')).toBeLessThan(0) // numeric identifiers compare numerically
  })
})

describe('fetchLatest (registry fallback)', () => {
  it('uses the first registry when it answers', async () => {
    const fetchImpl = vi.fn().mockResolvedValue(okResponse('0.9.9'))
    const r = await fetchLatest({ fetchImpl })
    expect(r).toEqual({ latest: '0.9.9', registry: UPGRADE_REGISTRIES[0] })
    expect(fetchImpl).toHaveBeenCalledTimes(1)
    expect(fetchImpl.mock.calls[0]?.[0]).toBe(`${UPGRADE_REGISTRIES[0]}/${UPGRADE_PACKAGE}`)
  })
  it('falls back to the mirror when npmjs is unreachable', async () => {
    const fetchImpl = vi.fn()
      .mockRejectedValueOnce(new Error('ETIMEDOUT'))
      .mockResolvedValueOnce(okResponse('0.9.9'))
    const r = await fetchLatest({ fetchImpl })
    expect(r.registry).toBe(UPGRADE_REGISTRIES[1])
    expect(fetchImpl).toHaveBeenCalledTimes(2)
  })
  it('falls back on a bad answer (HTTP error / junk dist-tags) too', async () => {
    const fetchImpl = vi.fn()
      .mockResolvedValueOnce({ ok: false, status: 503, json: () => Promise.resolve({}) })
      .mockResolvedValueOnce(okResponse('1.2.3'))
    const r = await fetchLatest({ fetchImpl })
    expect(r).toEqual({ latest: '1.2.3', registry: UPGRADE_REGISTRIES[1] })
  })
  it('rejects a non-semver latest instead of trusting the registry', async () => {
    const fetchImpl = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ 'dist-tags': { latest: '1.2.3 && evil' } }),
    })
    await expect(fetchLatest({ fetchImpl })).rejects.toThrow(/could not reach any npm registry/)
  })
  it('reports all failures when every registry is down', async () => {
    const fetchImpl = vi.fn().mockRejectedValue(new Error('offline'))
    await expect(fetchLatest({ fetchImpl })).rejects.toThrow(/offline/)
    expect(fetchImpl).toHaveBeenCalledTimes(UPGRADE_REGISTRIES.length)
  })
})

/** A fake installed layout: <root>/profiles/web/node_modules/soulnet-dsh/lib */
function fakeInstall(): { profileDir: string; moduleDir: string } {
  const root = mkdtempSync(join(tmpdir(), 'soulnet-upgrade-'))
  const profileDir = join(root, 'profiles', 'web')
  const moduleDir = join(profileDir, 'node_modules', UPGRADE_PACKAGE, 'lib')
  mkdirSync(moduleDir, { recursive: true })
  writeFileSync(join(profileDir, 'package.json'), JSON.stringify({ name: 'dsh-profile-web', dependencies: { [UPGRADE_PACKAGE]: '0.1.2' } }))
  writeFileSync(join(profileDir, 'node_modules', UPGRADE_PACKAGE, 'package.json'), JSON.stringify({ name: UPGRADE_PACKAGE, version: '0.1.2' }))
  return { profileDir, moduleDir }
}

describe('ownVersion / checkUpgrade', () => {
  it('reads the running version from the plugin package.json above the module', () => {
    const { moduleDir } = fakeInstall()
    expect(ownVersion(moduleDir)).toBe('0.1.2')
  })
  it('answers hasUpdate from the semver comparison', async () => {
    const { moduleDir } = fakeInstall()
    const newer = await checkUpgrade({ startDir: moduleDir, fetchImpl: vi.fn().mockResolvedValue(okResponse('0.2.0')) })
    expect(newer).toEqual({ current: '0.1.2', latest: '0.2.0', hasUpdate: true, registry: UPGRADE_REGISTRIES[0] })
    const same = await checkUpgrade({ startDir: moduleDir, fetchImpl: vi.fn().mockResolvedValue(okResponse('0.1.2')) })
    expect(same.hasUpdate).toBe(false)
  })
})

describe('resolveWebProfileDir', () => {
  it('finds the profile as the parent of the node_modules ancestor', () => {
    const { profileDir, moduleDir } = fakeInstall()
    expect(resolveWebProfileDir(moduleDir, {})).toBe(profileDir)
  })
  it('falls back to $DSH_HOME/profiles/web for a linked checkout', () => {
    const root = mkdtempSync(join(tmpdir(), 'soulnet-dshhome-'))
    const profileDir = join(root, 'profiles', 'web')
    mkdirSync(profileDir, { recursive: true })
    writeFileSync(join(profileDir, 'package.json'), JSON.stringify({ name: 'dsh-profile-web', dependencies: { [UPGRADE_PACKAGE]: '0.1.2' } }))
    const checkout = mkdtempSync(join(tmpdir(), 'soulnet-checkout-')) // no node_modules above
    expect(resolveWebProfileDir(checkout, { DSH_HOME: root })).toBe(profileDir)
  })
  it('refuses when no profile depending on the package exists', () => {
    const nowhere = mkdtempSync(join(tmpdir(), 'soulnet-nowhere-'))
    expect(() => resolveWebProfileDir(nowhere, {}, nowhere)).toThrow(/cannot locate/)
  })
})

type SpawnArgs = { command: string; args: string[]; options: Record<string, unknown> }

function fakeSpawn(exitCode: number, captured: SpawnArgs[]): (command: string, args: string[], options: Record<string, unknown>) => unknown {
  return (command, args, options) => {
    captured.push({ command, args, options })
    const listeners = new Map<string, (value?: unknown) => void>()
    const child = {
      stdout: { on: (_: string, cb: (chunk: unknown) => void) => { cb('installed things\n') } },
      stderr: { on: () => {} },
      on: (event: string, cb: (value?: unknown) => void) => { listeners.set(event, cb) },
      kill: () => {},
    }
    setTimeout(() => { listeners.get('close')?.(exitCode) }, 0)
    return child
  }
}

describe('runUpgrade', () => {
  it('rejects an invalid version WITHOUT spawning anything', async () => {
    const captured: SpawnArgs[] = []
    const spawnImpl = fakeSpawn(0, captured) as never
    for (const bad of ['1.2', 'latest', '1.2.3 && evil', 'file:../x']) {
      await expect(runUpgrade(bad, { profileDir: '/tmp', spawnImpl })).rejects.toThrow(/invalid version/)
    }
    expect(captured).toHaveLength(0)
  })
  it('rejects a registry outside the allowed list without spawning', async () => {
    const captured: SpawnArgs[] = []
    await expect(runUpgrade('1.2.3', { profileDir: '/tmp', registry: 'https://evil.example', spawnImpl: fakeSpawn(0, captured) as never }))
      .rejects.toThrow(/not in the allowed list/)
    expect(captured).toHaveLength(0)
  })
  it('always installs THIS package pinned to the validated version, in the profile dir', async () => {
    const { profileDir } = fakeInstall()
    const captured: SpawnArgs[] = []
    const result = await runUpgrade('0.2.0', { profileDir, registry: UPGRADE_REGISTRIES[1], spawnImpl: fakeSpawn(0, captured) as never })
    expect(result.ok).toBe(true)
    expect(result.output).toContain('installed things')
    expect(captured).toHaveLength(1)
    expect(captured[0]?.command).toBe('pnpm')
    expect(captured[0]?.args).toEqual(['add', `${UPGRADE_PACKAGE}@0.2.0`])
    expect(captured[0]?.options['cwd']).toBe(profileDir)
    const env = captured[0]?.options['env'] as Record<string, string>
    expect(env['npm_config_registry']).toBe(UPGRADE_REGISTRIES[1])
  })
  it('reports a pnpm failure as ok:false with the output tail', async () => {
    const { profileDir } = fakeInstall()
    const result = await runUpgrade('0.2.0', { profileDir, spawnImpl: fakeSpawn(1, []) as never })
    expect(result.ok).toBe(false)
    expect(result.exitCode).toBe(1)
  })
})
