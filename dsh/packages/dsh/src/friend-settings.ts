/**
 * The plugin's own per-friend settings (P3) and the diplomacy protocol file.
 *
 *   - `<home>/a2a/dsh-friends.json` — `{ friends: { [fp]: { tier } } }`: the
 *     reply tier per friend (`notify` | `draft` | `auto`). The peer's
 *     friends.yaml has no such field (the soulnet a2a package deliberately
 *     carries no auto_reply switch), so it lives beside the session map.
 *     The per-friend PROTOCOL OVERRIDE is NOT here: the peer stores it in
 *     friends.yaml (`friends.set {protocol}`), shared with SoulMirror.
 *   - `<home>/a2a/protocol.md` — the global diplomacy protocol. The peer
 *     writes the default text when the identity is created; this module
 *     reads it (cached by mtime so the prompt variable can be evaluated on
 *     every assembly) and writes it (Settings / page editor).
 */
import { mkdirSync, readFileSync, statSync, writeFileSync } from 'node:fs'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { dirname, join } from 'node:path'
import { normalizeTier, type ReplyTier } from './policy.ts'

export const FRIEND_SETTINGS_FILE = 'dsh-friends.json'
export const PROTOCOL_FILE = 'protocol.md'

export interface FriendSettings {
  readonly tier?: ReplyTier
  /** Do-not-disturb: mute the unread badge / new-mail toast for this friend. */
  readonly muted?: boolean
}

interface FriendSettingsFile {
  friends: Record<string, FriendSettings>
}

/** In-memory copy of dsh-friends.json with write-through. */
export class FriendSettingsStore {
  private data: FriendSettingsFile = { friends: {} }
  private loaded = false

  constructor(private readonly path: string) {}

  static at(a2aDir: string): FriendSettingsStore {
    return new FriendSettingsStore(join(a2aDir, FRIEND_SETTINGS_FILE))
  }

  async load(): Promise<void> {
    try {
      const raw = JSON.parse(await readFile(this.path, 'utf8')) as Partial<FriendSettingsFile>
      const friends: Record<string, FriendSettings> = {}
      for (const [fp, value] of Object.entries(raw.friends ?? {})) {
        const v = (typeof value === 'object' && value !== null ? value : {}) as Record<string, unknown>
        friends[fp] = {
          ...(v['tier'] === undefined ? {} : { tier: normalizeTier(v['tier']) }),
          ...(v['muted'] === true ? { muted: true } : {}),
        }
      }
      this.data = { friends }
    } catch {
      this.data = { friends: {} }
    }
    this.loaded = true
  }

  get isLoaded(): boolean {
    return this.loaded
  }

  get(fp: string): FriendSettings {
    return this.data.friends[fp] ?? {}
  }

  /** Tier of a friend, or `fallback` (the global default) when none is stored. */
  tier(fp: string, fallback: ReplyTier): ReplyTier {
    return this.data.friends[fp]?.tier ?? fallback
  }

  /** Every stored entry (fp → settings). */
  all(): Readonly<Record<string, FriendSettings>> {
    return this.data.friends
  }

  /** Patch one friend's settings; an explicit `tier: undefined` clears the stored tier (back to the global default). */
  async set(fp: string, patch: { tier?: ReplyTier | undefined; muted?: boolean }): Promise<FriendSettings> {
    const current = this.get(fp)
    const tier = 'tier' in patch ? patch.tier : current.tier
    const muted = 'muted' in patch ? patch.muted : current.muted
    const next: FriendSettings = { ...(tier === undefined ? {} : { tier }), ...(muted === undefined ? {} : { muted }) }
    if (Object.keys(next).length === 0) delete this.data.friends[fp]
    else this.data.friends[fp] = { ...current, ...next }
    await mkdir(dirname(this.path), { recursive: true })
    await writeFile(this.path, JSON.stringify(this.data, null, 2), 'utf8')
    return this.get(fp)
  }

  async remove(fp: string): Promise<void> {
    if (!(fp in this.data.friends)) return
    delete this.data.friends[fp]
    await writeFile(this.path, JSON.stringify(this.data, null, 2), 'utf8')
  }
}

/**
 * The global diplomacy protocol file. `read()` is synchronous and cached by
 * mtime so a prompt-variable provider can call it on every assembly; `write()`
 * replaces the file (and the cache).
 */
export class ProtocolFile {
  private cache: { mtimeMs: number; size: number; text: string } | undefined

  constructor(readonly path: string) {}

  static at(a2aDir: string): ProtocolFile {
    return new ProtocolFile(join(a2aDir, PROTOCOL_FILE))
  }

  /** Current text; `''` when the file does not exist yet (the peer writes it with the identity). */
  read(): string {
    try {
      const stat = statSync(this.path)
      if (this.cache !== undefined && this.cache.mtimeMs === stat.mtimeMs && this.cache.size === stat.size) return this.cache.text
      const text = readFileSync(this.path, 'utf8')
      this.cache = { mtimeMs: stat.mtimeMs, size: stat.size, text }
      return text
    } catch {
      this.cache = undefined
      return ''
    }
  }

  exists(): boolean {
    try {
      statSync(this.path)
      return true
    } catch {
      return false
    }
  }

  write(text: string): void {
    mkdirSync(dirname(this.path), { recursive: true })
    writeFileSync(this.path, text, 'utf8')
    this.cache = undefined
  }
}
