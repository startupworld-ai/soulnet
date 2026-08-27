/**
 * Self-upgrade of the soulnet-dsh plugin, host side (routes `upgrade.check` /
 * `upgrade.run` in ./index.ts).
 *
 *   - check: ask the npm registry for the `latest` dist-tag of THIS package
 *     and compare it with the running version (read from the plugin's own
 *     package.json). Registries are tried in order — registry.npmjs.org with
 *     a 5 s timeout, then registry.npmmirror.com (npmjs is often unreachable
 *     from mainland China).
 *   - run: `pnpm add soulnet-dsh@<version>` in the dsh web profile directory
 *     (the same install pnpm layout `dsh plugin` manages). pnpm is spawned
 *     exactly the way dsh's own `plugin` command does: by name from PATH,
 *     `shell: true` on Windows (pnpm.cmd), ENOENT = "install pnpm".
 *
 * Security boundary: the package name is a constant — only `soulnet-dsh` can
 * ever be installed through this module — and the version must match strict
 * semver (no ranges, no URLs, no shell metacharacters), so nothing
 * attacker-controlled reaches the command line.
 *
 * The profile directory is not handed to plugins by dsh, so it is derived:
 * the installed module lives at `<profile>/node_modules/soulnet-dsh/lib/…`,
 * i.e. the parent of the outermost `node_modules` ancestor; a linked dev
 * checkout has no such ancestor and falls back to `$DSH_HOME/profiles/web`
 * (default home `~/.dsh`, mirroring dsh-home-paths). Either way the directory
 * must hold a package.json that depends on this package, or run refuses.
 */
import { spawn as nodeSpawn } from 'node:child_process'
import { existsSync, readFileSync } from 'node:fs'
import { homedir } from 'node:os'
import { basename, dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

/** The only package this module is allowed to install. */
export const UPGRADE_PACKAGE = 'soulnet-dsh'
/** Registries in trial order (npmjs first, the China mirror as fallback). */
export const UPGRADE_REGISTRIES = ['https://registry.npmjs.org', 'https://registry.npmmirror.com'] as const
/** Per-registry metadata timeout. */
export const REGISTRY_TIMEOUT_MS = 5_000
/** Hard cap on one pnpm run. */
export const RUN_TIMEOUT_MS = 10 * 60_000
/** How much combined pnpm output the answer keeps (the tail). */
export const OUTPUT_TAIL_LIMIT = 8_000

/**
 * Strict semver: `major.minor.patch` with an optional prerelease, nothing
 * else (no `v` prefix, no ranges, no build metadata, no whitespace). This is
 * the injection gate for `upgrade.run` — everything that reaches the pnpm
 * command line must have passed it.
 */
const SEMVER_RE = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/

export function isValidVersion(version: string): boolean {
  return SEMVER_RE.test(version)
}

/** One prerelease identifier per semver §11: numerics compare numerically and rank below alphanumerics. */
function compareIdentifier(a: string, b: string): number {
  const an = /^\d+$/.test(a)
  const bn = /^\d+$/.test(b)
  if (an && bn) return Number(a) < Number(b) ? -1 : Number(a) > Number(b) ? 1 : 0
  if (an) return -1
  if (bn) return 1
  return a < b ? -1 : a > b ? 1 : 0
}

/** Semver precedence: negative = a < b, 0 = equal, positive = a > b. */
export function compareVersions(a: string, b: string): number {
  const [coreA = '', preA] = a.split('-', 2) as [string?, string?]
  const [coreB = '', preB] = b.split('-', 2) as [string?, string?]
  const numsA = coreA.split('.').map(Number)
  const numsB = coreB.split('.').map(Number)
  for (let i = 0; i < 3; i++) {
    const d = (numsA[i] ?? 0) - (numsB[i] ?? 0)
    if (d !== 0) return d
  }
  if (preA === undefined && preB === undefined) return 0
  if (preA === undefined) return 1 // release > prerelease
  if (preB === undefined) return -1
  const idsA = preA.split('.')
  const idsB = preB.split('.')
  for (let i = 0; i < Math.max(idsA.length, idsB.length); i++) {
    const ia = idsA[i]
    const ib = idsB[i]
    if (ia === undefined) return -1 // shorter prerelease ranks lower
    if (ib === undefined) return 1
    const d = compareIdentifier(ia, ib)
    if (d !== 0) return d
  }
  return 0
}

const moduleDir = (): string => dirname(fileURLToPath(import.meta.url))

function readManifest(dir: string): { name?: string; version?: string; deps: Set<string> } | undefined {
  const path = join(dir, 'package.json')
  if (!existsSync(path)) return undefined
  try {
    const parsed = JSON.parse(readFileSync(path, 'utf8')) as {
      name?: string
      version?: string
      dependencies?: Record<string, string>
      devDependencies?: Record<string, string>
    }
    return {
      ...(typeof parsed.name === 'string' ? { name: parsed.name } : {}),
      ...(typeof parsed.version === 'string' ? { version: parsed.version } : {}),
      deps: new Set([...Object.keys(parsed.dependencies ?? {}), ...Object.keys(parsed.devDependencies ?? {})]),
    }
  } catch {
    return undefined
  }
}

/**
 * The running plugin's own version: walk up from this module (lib/index.js in
 * an install, src/api/ in tests) to the nearest package.json named
 * {@link UPGRADE_PACKAGE}.
 */
export function ownVersion(startDir: string = moduleDir()): string | undefined {
  let dir = resolve(startDir)
  for (let i = 0; i < 12; i++) {
    const manifest = readManifest(dir)
    if (manifest?.name === UPGRADE_PACKAGE && manifest.version !== undefined) return manifest.version
    const parent = dirname(dir)
    if (parent === dir) return undefined
    dir = parent
  }
  return undefined
}

export interface UpgradeCheck {
  current: string
  latest: string
  hasUpdate: boolean
  /** The registry that answered — `upgrade.run` reuses it so check and install agree. */
  registry: string
}

type FetchLike = (url: string, init: { signal: AbortSignal; headers: Record<string, string> }) => Promise<{
  ok: boolean
  status: number
  json(): Promise<unknown>
}>

/** `latest` dist-tag of {@link UPGRADE_PACKAGE}, from the first registry that answers sanely. */
export async function fetchLatest(options: {
  fetchImpl?: FetchLike
  registries?: readonly string[]
  timeoutMs?: number
} = {}): Promise<{ latest: string; registry: string }> {
  const fetchImpl = options.fetchImpl ?? (fetch as unknown as FetchLike)
  const registries = options.registries ?? UPGRADE_REGISTRIES
  const timeoutMs = options.timeoutMs ?? REGISTRY_TIMEOUT_MS
  const failures: string[] = []
  for (const registry of registries) {
    try {
      const response = await fetchImpl(`${registry}/${UPGRADE_PACKAGE}`, {
        signal: AbortSignal.timeout(timeoutMs),
        // Abbreviated metadata (npm install format) still carries dist-tags and is far smaller.
        headers: { accept: 'application/vnd.npm.install-v1+json, application/json' },
      })
      if (!response.ok) {
        failures.push(`${registry}: HTTP ${response.status}`)
        continue
      }
      const data = await response.json() as { 'dist-tags'?: { latest?: string } }
      const latest = data['dist-tags']?.latest
      if (latest === undefined || !isValidVersion(latest)) {
        failures.push(`${registry}: no valid dist-tags.latest`)
        continue
      }
      return { latest, registry }
    } catch (error: unknown) {
      failures.push(`${registry}: ${error instanceof Error ? error.message : String(error)}`)
    }
  }
  throw new Error(`could not reach any npm registry (${failures.join('; ')})`)
}

/** `upgrade.check`: the running version vs. the registry's latest. */
export async function checkUpgrade(options: {
  fetchImpl?: FetchLike
  registries?: readonly string[]
  timeoutMs?: number
  startDir?: string
} = {}): Promise<UpgradeCheck> {
  const current = ownVersion(options.startDir ?? moduleDir())
  if (current === undefined) throw new Error(`cannot determine the running ${UPGRADE_PACKAGE} version`)
  const { latest, registry } = await fetchLatest(options)
  return { current, latest, hasUpdate: compareVersions(latest, current) > 0, registry }
}

/**
 * The dsh web profile directory (`pnpm add` target). Preferred: the parent of
 * the outermost `node_modules` ancestor of this module (that is where dsh's
 * profile pnpm installed us). Fallback (linked dev checkout): the standard
 * `$DSH_HOME/profiles/web`. Whichever wins must be a profile that actually
 * depends on {@link UPGRADE_PACKAGE}.
 */
export function resolveWebProfileDir(
  startDir: string = moduleDir(),
  env: Record<string, string | undefined> = process.env,
  home: string = homedir(),
): string {
  const isProfile = (dir: string): boolean => readManifest(dir)?.deps.has(UPGRADE_PACKAGE) === true
  let dir = resolve(startDir)
  let found: string | undefined
  for (let i = 0; i < 24; i++) {
    if (basename(dir) === 'node_modules' && isProfile(dirname(dir))) found = dirname(dir)
    const parent = dirname(dir)
    if (parent === dir) break
    dir = parent
  }
  if (found !== undefined) return found
  const dshHome = env['DSH_HOME'] !== undefined && env['DSH_HOME'].trim() !== '' ? env['DSH_HOME'] : join(home, '.dsh')
  const fallback = join(resolve(dshHome), 'profiles', 'web')
  if (isProfile(fallback)) return fallback
  throw new Error(`cannot locate the dsh profile directory that installed ${UPGRADE_PACKAGE} (looked above ${startDir} and at ${fallback})`)
}

export interface UpgradeRunResult {
  ok: boolean
  exitCode: number
  /** Tail of the combined pnpm stdout+stderr. */
  output: string
  version: string
  profileDir: string
}

/** One upgrade at a time, process-wide (two browser tabs must not race pnpm). */
let upgradeInflight = false

export function upgradeRunning(): boolean {
  return upgradeInflight
}

/**
 * `upgrade.run`: `pnpm add soulnet-dsh@<version>` in the profile directory,
 * spawned like dsh's own `plugin` command (PATH lookup, `shell` on Windows).
 * The registry (when given) travels as `npm_config_registry` so the install
 * hits the same mirror the check succeeded against.
 */
export function runUpgrade(version: string, options: {
  profileDir?: string
  registry?: string
  timeoutMs?: number
  spawnImpl?: typeof nodeSpawn
} = {}): Promise<UpgradeRunResult> {
  if (!isValidVersion(version)) return Promise.reject(new Error(`invalid version ${JSON.stringify(version)} (strict X.Y.Z semver required)`))
  if (options.registry !== undefined && !(UPGRADE_REGISTRIES as readonly string[]).includes(options.registry)) {
    return Promise.reject(new Error('registry not in the allowed list'))
  }
  let profileDir: string
  try {
    profileDir = options.profileDir ?? resolveWebProfileDir()
  } catch (error: unknown) {
    return Promise.reject(error instanceof Error ? error : new Error(String(error)))
  }
  if (upgradeInflight) return Promise.reject(new Error('an upgrade is already running'))
  upgradeInflight = true
  const spawnImpl = options.spawnImpl ?? nodeSpawn
  const timeoutMs = options.timeoutMs ?? RUN_TIMEOUT_MS
  return new Promise<UpgradeRunResult>((resolvePromise) => {
    let output = ''
    const append = (chunk: unknown): void => {
      output = (output + String(chunk)).slice(-OUTPUT_TAIL_LIMIT)
    }
    const finish = (exitCode: number): void => {
      if (!upgradeInflight) return
      upgradeInflight = false
      resolvePromise({ ok: exitCode === 0, exitCode, output: output.trim(), version, profileDir })
    }
    let child: ReturnType<typeof nodeSpawn>
    try {
      child = spawnImpl('pnpm', ['add', `${UPGRADE_PACKAGE}@${version}`], {
        cwd: profileDir,
        // Same pnpm location strategy as dsh's `plugin` command: by name from
        // PATH, through the shell on Windows (pnpm is pnpm.cmd there).
        shell: process.platform === 'win32',
        windowsHide: true,
        env: {
          ...process.env,
          ...(options.registry === undefined ? {} : { npm_config_registry: options.registry }),
        },
        stdio: ['ignore', 'pipe', 'pipe'],
      })
    } catch (error: unknown) {
      append(error instanceof Error ? error.message : String(error))
      finish(1)
      return
    }
    const timer = setTimeout(() => {
      append(`\n[timed out after ${Math.round(timeoutMs / 1000)}s — killing pnpm]`)
      try {
        child.kill()
      } catch {
        // already gone
      }
      finish(124)
    }, timeoutMs)
    timer.unref?.()
    child.stdout?.on('data', append)
    child.stderr?.on('data', append)
    child.on('error', (error: NodeJS.ErrnoException) => {
      clearTimeout(timer)
      append(error.code === 'ENOENT' ? 'pnpm not found on PATH — install pnpm to manage profile plugins' : error.message)
      finish(error.code === 'ENOENT' ? 127 : 1)
    })
    child.on('close', (code) => {
      clearTimeout(timer)
      finish(code ?? 1)
    })
  })
}
