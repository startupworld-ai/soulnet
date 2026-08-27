/**
 * Paygate client: spawns and drives the local payment gateway process
 * (`payment/cmd/paygate`, Go). The gateway listens on 127.0.0.1 only and
 * authenticates every request with the A2A request signature — the plugin asks
 * the soulnet peer to sign (`identity.signRequest`, the private key never
 * leaves the peer), reads the public key from identity.json, and calls the
 * gateway over plain HTTP on the loopback.
 *
 * CDP secrets are passed to the gateway via its environment (from the
 * `soulmirror` settings, which live in $DSH_HOME profiles — never in the repo).
 */
import { spawn, type ChildProcess } from 'node:child_process'
import { existsSync, readFileSync, realpathSync } from 'node:fs'
import { createRequire } from 'node:module'
import { dirname, join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import type { NetworkClient } from './types.ts'

/** The paygate-relevant slice of the `soulmirror` settings. */
export interface PaygateSettings {
  readonly paygateBinary: string
  readonly paygatePort: number
  readonly paygateProxy: string
  readonly cdpKeyId: string
  readonly cdpKeySecret: string
  readonly cdpWalletSecret: string
  readonly cdpNetwork: string
}

export interface PaygateOptions {
  readonly home: string
  readonly net: NetworkClient
  readonly settings: () => PaygateSettings
  readonly logger?: (message: string) => void
  /** Test seam: replace `child_process.spawn`. */
  readonly spawn?: (request: { binary: string; args: string[]; env: Record<string, string> }) => ChildProcess
}

export interface PaygateStatus {
  readonly state: 'stopped' | 'starting' | 'running' | 'error'
  readonly port: number
  readonly cdpConfigured: boolean
  readonly network: string
  readonly lastError?: string
}

/** A gateway error: HTTP response `{error, code}` (code = peer-style -320xx). */
export class PaygateError extends Error {
  override readonly name = 'PaygateError'
  constructor(message: string, readonly code: number) {
    super(message)
  }
}

export interface PaygateClient {
  start(): void
  status(): PaygateStatus
  /** Restart the gateway process (e.g. after CDP settings changed). No-op when never started or disposed. */
  restart(): void
  /** Call one /v2/pay/* endpoint with an A2A-signed request. */
  call(method: 'GET' | 'POST', path: string, body?: unknown): Promise<unknown>
  /** Stop the gateway process. Idempotent. */
  dispose(): Promise<void>
}

const A2A_TIMEOUT_MS = 30_000

const PAYGATE_PLATFORM_PACKAGE_PREFIX = 'soulnet-paygate-'

/** `soulnet-paygate-<os>-<arch>` for a supported pair, else `undefined`. */
export function paygatePlatformPackageName(platform = process.platform, arch = process.arch): string | undefined {
  const target = `${platform}-${arch}`
  if (!['darwin-arm64', 'darwin-x64', 'linux-arm64', 'linux-x64', 'win32-x64'].includes(target)) return undefined
  return `${PAYGATE_PLATFORM_PACKAGE_PREFIX}${platform === 'win32' ? 'windows' : platform}-${arch}`
}

/**
 * Resolve the installed paygate binary, in order:
 *  1. the explicit `paygateBinary` setting (handled by the caller);
 *  2. the platform package `soulnet-paygate-<os>-<arch>` (optional dependency,
 *     resolved from this module and its realpath so pnpm layouts work);
 *  3. a hand-dropped `<plugin>/bin/paygate`;
 *  4. `paygate` on PATH (the caller's fallback).
 */
/** Resolve an installed package's directory from this module (mirrors peer resolution). */
function resolvePaygatePackageDir(name: string): string | undefined {
  const bases: string[] = [import.meta.url]
  try {
    const real = realpathSync(fileURLToPath(import.meta.url))
    const realUrl = pathToFileURL(real).href
    if (realUrl !== import.meta.url) bases.push(realUrl)
  } catch {
    // not a file URL (bundled in memory) or unreadable; the first base still works
  }
  for (const base of bases) {
    try {
      return dirname(createRequire(base).resolve(`${name}/package.json`))
    } catch {
      // not installed from this base
    }
  }
  return undefined
}

function resolvePaygateBinary(): string | undefined {
  const pkg = paygatePlatformPackageName()
  if (pkg !== undefined) {
    const pkgDir = resolvePaygatePackageDir(pkg)
    if (pkgDir !== undefined) {
      const bin = join(pkgDir, 'bin', process.platform === 'win32' ? 'paygate.exe' : 'paygate')
      if (existsSync(bin)) return bin
    }
  }
  // hand-dropped binary next to the plugin
  try {
    for (const depth of ['../bin/paygate', '../../bin/paygate', '../../../bin/paygate']) {
      const candidate = new URL(depth, import.meta.url)
      if (existsSync(candidate)) return candidate.pathname
    }
  } catch { /* import.meta.url unavailable */ }
  return undefined
}

/** Read the identity's Ed25519 public key (base64) from <home>/a2a/identity.json. */
function readIdentityPub(home: string): string | undefined {
  const file = join(home, 'a2a', 'identity.json')
  if (!existsSync(file)) return undefined
  try {
    const id = JSON.parse(readFileSync(file, 'utf8')) as { ed_pub?: string }
    return id.ed_pub === undefined || id.ed_pub === '' ? undefined : id.ed_pub
  } catch {
    return undefined
  }
}

export function createPaygateClient(options: PaygateOptions): PaygateClient {
  const log = options.logger ?? (() => {})
  const net = options.net
  let child: ChildProcess | undefined
  let disposed = false
  let started = false
  let status: PaygateStatus = { state: 'stopped', port: options.settings().paygatePort, cdpConfigured: false, network: options.settings().cdpNetwork }
  let pubCache: string | undefined

  const envFor = (): Record<string, string> => {
    const s = options.settings()
    const env: Record<string, string> = {
      PAYGATE_LISTEN: `127.0.0.1:${s.paygatePort}`,
      PAYGATE_HOME: join(options.home, 'a2a', 'pay'),
      PAYGATE_IDENTITY_FILE: join(options.home, 'a2a', 'identity.json'),
      CDP_NETWORK: s.cdpNetwork,
    }
    if (s.paygateProxy !== '') {
      env.HTTP_PROXY = s.paygateProxy
      env.HTTPS_PROXY = s.paygateProxy
    }
    if (s.cdpKeyId !== '' && s.cdpKeySecret !== '' && s.cdpWalletSecret !== '') {
      env.CDP_API_KEY_ID = s.cdpKeyId
      env.CDP_API_KEY_SECRET = s.cdpKeySecret
      env.CDP_WALLET_SECRET = s.cdpWalletSecret
    }
    return env
  }

  const cdpConfigured = (): boolean => {
    const s = options.settings()
    return s.cdpKeyId !== '' && s.cdpKeySecret !== '' && s.cdpWalletSecret !== ''
  }

  const spawnGateway = (): void => {
    if (disposed || child !== undefined) return
    const s = options.settings()
    const binary = s.paygateBinary !== '' ? s.paygateBinary : (resolvePaygateBinary() ?? 'paygate')
    const env = envFor()
    status = { state: 'starting', port: s.paygatePort, cdpConfigured: cdpConfigured(), network: s.cdpNetwork }
    log(`paygate: spawning ${binary} on 127.0.0.1:${s.paygatePort} (cdp=${cdpConfigured()})`)
    try {
      if (options.spawn !== undefined) {
        child = options.spawn({ binary, args: [], env })
      } else {
        child = spawn(binary, [], {
          env: { ...process.env, ...env },
          stdio: ['ignore', 'pipe', 'pipe'],
        })
        child.stdout?.on('data', (d: Buffer) => log(`paygate: ${String(d).trimEnd()}`))
        child.stderr?.on('data', (d: Buffer) => log(`paygate: ${String(d).trimEnd()}`))
      }
    } catch (error: unknown) {
      status = { ...status, state: 'error', lastError: String(error) }
      child = undefined
      return
    }
    child.on('exit', (code, signal) => {
      log(`paygate: exited (code=${code} signal=${String(signal)})`)
      child = undefined
      if (!disposed) status = { ...status, state: 'stopped', lastError: `paygate exited (code=${code})` }
    })
    child.on('error', (error: Error) => {
      child = undefined
      if (!disposed) status = { ...status, state: 'error', lastError: error.message }
    })
  }

  const ready = async (timeoutMs = 10_000): Promise<void> => {
    const deadline = Date.now() + timeoutMs
    // eslint-disable-next-line no-constant-condition
    while (true) {
      if (disposed) throw new PaygateError('paygate disposed', -32099)
      if (status.state === 'error') throw new PaygateError(status.lastError ?? 'paygate failed to start', -32099)
      try {
        const res = await fetch(`http://127.0.0.1:${status.port}/v2/pay/health`, { signal: AbortSignal.timeout(1500) })
        if (res.ok) {
          const data = (await res.json()) as { network?: string; cdp_configured?: boolean }
          status = { state: 'running', port: status.port, cdpConfigured: data.cdp_configured ?? cdpConfigured(), network: data.network ?? status.network }
          return
        }
      } catch {
        // not up yet
      }
      if (Date.now() >= deadline) throw new PaygateError('paygate not ready', -32099)
      await new Promise(resolve => setTimeout(resolve, 200))
    }
  }

  const start = (): void => {
    if (started || disposed) return
    started = true
    spawnGateway()
  }

  const restart = (): void => {
    if (disposed) return
    const proc = child
    child = undefined
    if (proc !== undefined && proc.exitCode === null) proc.kill('SIGTERM')
    status = { state: 'starting', port: options.settings().paygatePort, cdpConfigured: cdpConfigured(), network: options.settings().cdpNetwork }
    spawnGateway()
  }

  const call = async (method: 'GET' | 'POST', path: string, body?: unknown): Promise<unknown> => {
    await ready()
    const ts = new Date().toISOString()
    pubCache = pubCache ?? readIdentityPub(options.home)
    if (pubCache === undefined) throw new PaygateError('no identity public key (identity.json missing)', -32001)
    const signature = await net.signRequest(method, path, ts)
    const init: RequestInit = {
      method,
      headers: {
        'Content-Type': 'application/json',
        'X-A2A-Pub': pubCache,
        'X-A2A-Timestamp': ts,
        'X-A2A-Signature': signature,
      },
      signal: AbortSignal.timeout(A2A_TIMEOUT_MS),
    }
    if (body !== undefined) init.body = JSON.stringify(body)
    const res = await fetch(`http://127.0.0.1:${status.port}${path}`, init)
    const data = (await res.json()) as { error?: string; code?: number } & Record<string, unknown>
    if (data.error !== undefined) throw new PaygateError(data.error, data.code ?? res.status)
    return data
  }

  const dispose = async (): Promise<void> => {
    disposed = true
    const proc = child
    child = undefined
    if (proc === undefined || proc.exitCode !== null) return
    await new Promise<void>(resolve => {
      const timer = setTimeout(() => { proc.kill('SIGKILL'); resolve() }, 2000)
      proc.once('exit', () => { clearTimeout(timer); resolve() })
      proc.kill('SIGTERM')
    })
  }

  return { start, restart, status: () => status, call, dispose }
}
