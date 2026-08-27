/**
 * Client store of the plugin self-upgrade (Settings → SoulMirror network →
 * "Version & updates"; host routes `upgrade.check` / `upgrade.run`).
 *
 * The client half does a SILENT check once per page load (client/index.ts);
 * `hasUpdate` then puts the badge on the Settings entry. `run()` drives the
 * whole one-click path: install → the host answers `restarting: true` and
 * exits → this store polls `/state` until the RESTARTED server answers, then
 * reloads the page (phase `reloading`). A host that could not arm the
 * relaunch helper answers `restarting: false` and the UI shows the
 * manual-restart hint instead (phase `done-manual`).
 */
import { API_BASE, api, type ApiUpgradeRun } from './api.ts'

export type UpgradePhase =
  | 'idle'
  | 'checking'
  | 'installing'
  /** Installed, host restarting itself; waiting for the server to come back. */
  | 'restarting'
  /** Server is back — reloading the page right now. */
  | 'reloading'
  /** Installed, but the host cannot restart itself: refresh for the UI, restart dsh for the host half. */
  | 'done-manual'
  | 'failed'

export interface UpgradeSnapshot {
  readonly phase: UpgradePhase
  readonly current: string | undefined
  readonly latest: string | undefined
  readonly hasUpdate: boolean
  /** Registry that answered the check (reused for the install). */
  readonly registry: string | undefined
  /** Last check/run error (undefined while things go well). */
  readonly error: string | undefined
  /** pnpm output tail of a failed install. */
  readonly output: string | undefined
  /** True once a check has answered (silent or manual). */
  readonly checked: boolean
}

const EMPTY: UpgradeSnapshot = { phase: 'idle', current: undefined, latest: undefined, hasUpdate: false, registry: undefined, error: undefined, output: undefined, checked: false }

/** How long the restart may take before the store gives up and shows the manual hint. */
const RESTART_LIMIT_MS = 3 * 60_000
const RESTART_POLL_MS = 2_000

export class UpgradeStore {
  private snapshot: UpgradeSnapshot = EMPTY
  private readonly listeners = new Set<() => void>()
  private checkedOnce = false

  getSnapshot = (): UpgradeSnapshot => this.snapshot

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener)
    return () => { this.listeners.delete(listener) }
  }

  private set(patch: Partial<UpgradeSnapshot>): void {
    this.snapshot = { ...this.snapshot, ...patch }
    for (const listener of this.listeners) listener()
  }

  /**
   * Query the host for the latest published version. Silent mode (page load)
   * swallows errors — an offline machine must not grow error banners on its
   * own; the manual button reports them.
   */
  check = async (options: { silent?: boolean; refresh?: boolean } = {}): Promise<void> => {
    if (this.snapshot.phase === 'checking' || this.snapshot.phase === 'installing' || this.snapshot.phase === 'restarting') return
    if (options.silent === true && this.checkedOnce && options.refresh !== true) return
    this.checkedOnce = true
    this.set({ phase: 'checking', ...(options.silent === true ? {} : { error: undefined }) })
    try {
      const result = await api.upgradeCheck()
      this.set({
        phase: 'idle',
        current: result.current,
        latest: result.latest,
        hasUpdate: result.hasUpdate,
        registry: result.registry,
        checked: true,
        error: undefined,
      })
    } catch (error: unknown) {
      this.set({
        phase: 'idle',
        checked: true,
        ...(options.silent === true ? {} : { error: error instanceof Error ? error.message : String(error) }),
      })
    }
  }

  /** One-click upgrade to the checked `latest`. */
  run = async (): Promise<void> => {
    const { latest, registry } = this.snapshot
    if (latest === undefined || this.snapshot.phase === 'installing' || this.snapshot.phase === 'restarting') return
    this.set({ phase: 'installing', error: undefined, output: undefined })
    let result: ApiUpgradeRun
    try {
      result = await api.upgradeRun(latest, registry)
    } catch (error: unknown) {
      this.set({ phase: 'failed', error: error instanceof Error ? error.message : String(error) })
      return
    }
    if (!result.ok) {
      this.set({ phase: 'failed', error: `pnpm exit ${result.exitCode}`, output: result.output })
      return
    }
    if (!result.restarting) {
      this.set({ phase: 'done-manual', hasUpdate: false, current: result.version })
      return
    }
    this.set({ phase: 'restarting', hasUpdate: false, current: result.version })
    await this.waitForRestart()
  }

  /**
   * The old server exits ~500 ms after answering; poll `/state` until it
   * FIRST disappears and THEN answers again, so an answer from the old
   * process does not reload us into the old bundle.
   */
  private async waitForRestart(): Promise<void> {
    const deadline = Date.now() + RESTART_LIMIT_MS
    let sawDown = false
    while (Date.now() < deadline) {
      await new Promise(resolve => setTimeout(resolve, RESTART_POLL_MS))
      try {
        const response = await fetch(`${API_BASE}state`, { method: 'GET', signal: AbortSignal.timeout(1_500) })
        if (!response.ok) {
          sawDown = true
          continue
        }
        if (sawDown) {
          this.set({ phase: 'reloading' })
          location.reload()
          return
        }
      } catch {
        sawDown = true
      }
    }
    // It never came back (or never went down) in time — the manual hint still applies.
    this.set({ phase: 'done-manual' })
  }
}

export const upgradeStore = new UpgradeStore()
