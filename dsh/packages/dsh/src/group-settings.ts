/**
 * Per-group CLIENT settings of this plugin: `<home>/dsh-groups.json`.
 *
 * v3 — seat voices with per-group commanders: `{ [gid]: { voices?: {
 * [name]: { on: true, commanders?: [fp | '*'] } }, duty?: name } }`.
 * `voices` says which of MY voices participate in this group — key `'alter'`
 * is the default alter, every other key names a seat agent
 * (../agent-registry.ts). A voice's `commanders` are the MEMBERS whose group
 * messages may wake that agent here (`'*'` = every member; absent/empty =
 * only the owner commands it, through own posts and its direct chat) — they
 * are configured in the GROUP's agent sheet, where the member list is at
 * hand, never on the agent's global definition. `duty` names the ONE voice
 * that answers unmentioned traffic (still capped by the group profile's
 * `agent_wake`); absent = every enabled voice wakes on mention only.
 *
 * Older shapes fold on read: v1 `{ alter, mode }` → the alter voice (+ duty
 * on `'always'`); v2 voice values without commanders stay commander-less.
 * The legacy accessors (`alterOn` / `modeOf`) and patch keys (`alter` /
 * `mode`) remain views over the new shape.
 *
 * The peer knows nothing of any of it: whether and for whom MY voices engage
 * with a group is my node's business (same principle as ./friend-settings.ts).
 */
import { readFileSync } from 'node:fs'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { dirname, join } from 'node:path'

export const GROUP_SETTINGS_FILE = 'dsh-groups.json'

/** The voice key of the default alter. */
export const VOICE_ALTER = 'alter'

/** Commander entry meaning "any group member may command this voice". */
export const COMMANDER_ANY = '*'

/** My alter's participation strategy inside one group (legacy view of the duty slot). */
export type GroupAlterMode = 'mention' | 'always'

export interface GroupVoiceSetting {
  readonly on: true
  /**
   * Member fingerprints whose group messages may wake this voice (`'*'` =
   * every member; absent/empty = only the owner). Meaningful for seat
   * agents; the alter ignores it (its wake grammar is the owner's name).
   */
  readonly commanders?: readonly string[]
}

export interface GroupSettings {
  /** Voice switches: which of MY voices participate here (`alter` = the default alter; other keys = seat agent names). */
  readonly voices?: Readonly<Record<string, GroupVoiceSetting>>
  /** The duty voice: answers unmentioned traffic (capped by the group's wake policy); absent = mention-only for every voice. */
  readonly duty?: string
  /** Do-not-disturb: mute the unread badge / new-mail toast for this group. */
  readonly muted?: boolean
}

export type GroupSettingsMap = Record<string, GroupSettings>

const str = (value: unknown): string => (typeof value === 'string' ? value : '')

function normalizeCommanders(raw: unknown): string[] {
  if (!Array.isArray(raw)) return []
  const out: string[] = []
  for (const c of raw) {
    if (typeof c !== 'string') continue
    const v = c.trim()
    if (v === '' || out.includes(v)) continue
    out.push(v)
    if (out.length === 64) break
  }
  return out
}

function normalizeEntry(value: unknown): GroupSettings | undefined {
  if (typeof value !== 'object' || value === null) return undefined
  const v = value as Record<string, unknown>
  const voices: Record<string, GroupVoiceSetting> = {}
  const rawVoices = v['voices']
  if (typeof rawVoices === 'object' && rawVoices !== null) {
    for (const [name, entry] of Object.entries(rawVoices as Record<string, unknown>)) {
      if (name.trim() === '') continue
      if (entry === true) {
        voices[name] = { on: true }
        continue
      }
      if (typeof entry === 'object' && entry !== null && (entry as Record<string, unknown>)['on'] === true) {
        const commanders = normalizeCommanders((entry as Record<string, unknown>)['commanders'])
        voices[name] = { on: true, ...(commanders.length === 0 ? {} : { commanders }) }
      }
    }
  }
  // v1 fold: alter toggle + mode.
  if (v['alter'] === true) voices[VOICE_ALTER] = { on: true }
  let duty = str(v['duty'])
  if (duty === '' && v['mode'] === 'always' && voices[VOICE_ALTER] !== undefined) duty = VOICE_ALTER
  if (duty !== '' && voices[duty] === undefined) duty = '' // duty implies participation
  const muted = v['muted'] === true
  if (Object.keys(voices).length === 0 && !muted) return undefined
  return { ...(Object.keys(voices).length === 0 ? {} : { voices }), ...(duty === '' ? {} : { duty }), ...(muted ? { muted: true } : {}) }
}

function normalize(raw: unknown): GroupSettingsMap {
  if (typeof raw !== 'object' || raw === null) return {}
  const map: GroupSettingsMap = {}
  for (const [gid, value] of Object.entries(raw as Record<string, unknown>)) {
    const entry = normalizeEntry(value)
    if (entry !== undefined) map[gid] = entry
  }
  return map
}

/** Path of the settings file under one plugin home. */
export function groupSettingsPath(home: string): string {
  return join(home, GROUP_SETTINGS_FILE)
}

/**
 * Read the whole map synchronously (host-side consumers). Missing or
 * malformed file = empty map.
 */
export function readGroupSettings(home: string): GroupSettingsMap {
  try {
    return normalize(JSON.parse(readFileSync(groupSettingsPath(home), 'utf8')))
  } catch {
    return {}
  }
}

/** One `set` patch: legacy alter keys, a single voice switch (with optional commanders), and/or the duty slot. */
export interface GroupSettingsPatch {
  /** Legacy: the alter toggle (mapped to the `alter` voice). */
  readonly alter?: boolean
  /** Legacy: `'always'` puts the alter on duty, `'mention'` takes it off. */
  readonly mode?: GroupAlterMode
  /** Switch one voice on or off (off also vacates its duty slot). `commanders` replaces the whitelist when given; omitted = kept. */
  readonly voice?: { readonly name: string; readonly on: boolean; readonly commanders?: readonly string[] }
  /** Set the duty voice (implies switching it on) or clear it with `null`. */
  readonly duty?: string | null
  /** Do-not-disturb switch. */
  readonly muted?: boolean
}

/** In-memory copy of dsh-groups.json with write-through (single writer: the sessions plugin's instance). */
export class GroupSettingsStore {
  private data: GroupSettingsMap = {}
  private loaded = false

  constructor(readonly path: string) {}

  static at(home: string): GroupSettingsStore {
    return new GroupSettingsStore(groupSettingsPath(home))
  }

  async load(): Promise<void> {
    try {
      this.data = normalize(JSON.parse(await readFile(this.path, 'utf8')))
    } catch {
      this.data = {}
    }
    this.loaded = true
  }

  get isLoaded(): boolean {
    return this.loaded
  }

  get(gid: string): GroupSettings {
    return this.data[gid] ?? {}
  }

  /** The switch of one voice in one group; default OFF (quiet by default). */
  voiceOn(gid: string, name: string): boolean {
    return this.data[gid]?.voices?.[name]?.on === true
  }

  /** Member fingerprints who may command one voice in one group (may hold '*'; empty = owner only). */
  commandersOf(gid: string, name: string): readonly string[] {
    return this.data[gid]?.voices?.[name]?.commanders ?? []
  }

  /** The duty voice of a group (answers unmentioned traffic), when any. */
  dutyOf(gid: string): string | undefined {
    return this.data[gid]?.duty
  }

  /** Legacy view: the alter toggle of a group. */
  alterOn(gid: string): boolean {
    return this.voiceOn(gid, VOICE_ALTER)
  }

  /** Legacy view: the alter's participation strategy while on. */
  modeOf(gid: string): GroupAlterMode {
    return this.dutyOf(gid) === VOICE_ALTER ? 'always' : 'mention'
  }

  all(): Readonly<GroupSettingsMap> {
    return this.data
  }

  /** Patch one group's settings; an entry with no voice left is dropped from the file. */
  async set(gid: string, patch: GroupSettingsPatch): Promise<GroupSettings> {
    const current = this.get(gid)
    const voices: Record<string, GroupVoiceSetting> = { ...current.voices }
    let duty = current.duty

    const switchVoice = (name: string, on: boolean, commanders?: readonly string[]): void => {
      if (name.trim() === '') return
      if (on) {
        const kept = commanders === undefined ? voices[name]?.commanders : normalizeCommanders([...commanders])
        voices[name] = { on: true, ...(kept === undefined || kept.length === 0 ? {} : { commanders: kept }) }
      } else {
        delete voices[name]
        if (duty === name) duty = undefined
      }
    }

    if (patch.alter !== undefined) switchVoice(VOICE_ALTER, patch.alter)
    if (patch.mode === 'always') {
      switchVoice(VOICE_ALTER, true)
      duty = VOICE_ALTER
    } else if (patch.mode === 'mention' && duty === VOICE_ALTER) {
      duty = undefined
    }
    if (patch.voice !== undefined) switchVoice(patch.voice.name, patch.voice.on, patch.voice.commanders)
    if (patch.duty === null) duty = undefined
    else if (typeof patch.duty === 'string' && patch.duty.trim() !== '') {
      switchVoice(patch.duty, true)
      duty = patch.duty
    }
    if (duty !== undefined && voices[duty] === undefined) duty = undefined

    let muted = current.muted
    if (patch.muted !== undefined) muted = patch.muted

    const entry: GroupSettings = { ...(Object.keys(voices).length === 0 ? {} : { voices }), ...(duty === undefined ? {} : { duty }), ...(muted === true ? { muted: true } : {}) }
    if (Object.keys(entry).length === 0) delete this.data[gid]
    else this.data[gid] = entry
    await mkdir(dirname(this.path), { recursive: true })
    await writeFile(this.path, JSON.stringify(this.data, null, 2), 'utf8')
    return this.get(gid)
  }

  /** Forget a group (left / dissolved). */
  async remove(gid: string): Promise<void> {
    if (!(gid in this.data)) return
    delete this.data[gid]
    await writeFile(this.path, JSON.stringify(this.data, null, 2), 'utf8')
  }
}
