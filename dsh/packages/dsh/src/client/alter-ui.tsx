/**
 * Building blocks shared by the SoulMirror page and the settings section:
 *
 *   - `TierPill` / `TierSelect`: the reply tier (notify / draft / auto, with
 *     an optional "default" entry).
 *   - `FriendSettingsPanel`: per-friend tier + protocol override (friend pane,
 *     above the action bar).
 *   - `ProtocolEditor`: the global diplomacy protocol textarea (friend list
 *     head + settings section), saved through `protocol.set`.
 */
import { useEffect, useState, useSyncExternalStore } from 'react'
import { api, networkStore, type ReplyTier } from './api.ts'
import type { Translate } from './translate.ts'

export const TIERS: readonly ReplyTier[] = ['notify', 'draft', 'auto']

export function tierLabel(t: Translate, tier: ReplyTier): string {
  return tier === 'notify' ? t('tier.notify') : tier === 'auto' ? t('tier.auto') : t('tier.draft')
}

export function tierShort(t: Translate, tier: ReplyTier): string {
  return tier === 'notify' ? t('tier.short.notify') : tier === 'auto' ? t('tier.short.auto') : t('tier.short.draft')
}

export function TierPill({ tier, t }: { tier: ReplyTier; t: Translate }) {
  return <span className={`sm-tier-pill sm-tier-${tier}`} data-soulmirror-tier={tier}>{tierShort(t, tier)}</span>
}

export function TierSelect({ value, defaultTier, onChange, t, disabled, id }: {
  /** Stored tier; undefined = the global default applies. */
  value: ReplyTier | undefined
  defaultTier: ReplyTier
  onChange: (tier: ReplyTier | undefined) => void
  t: Translate
  disabled?: boolean
  id?: string
}) {
  return (
    <select
      id={id}
      className="sm-select"
      value={value ?? ''}
      disabled={disabled}
      onChange={(e) => { onChange(e.target.value === '' ? undefined : e.target.value as ReplyTier) }}
      data-soulmirror-tier-select
    >
      <option value="">{t('tier.default', { tier: tierShort(t, defaultTier) })}</option>
      {TIERS.map(tier => <option key={tier} value={tier}>{tierLabel(t, tier)}</option>)}
    </select>
  )
}

/** Per-friend alter settings: reply tier + protocol override (friend pane, above the action bar). */
export function FriendSettingsPanel({ fp, name, tier, tierExplicit, protocol, muted, defaultTier, perHour, t, onClose }: {
  fp: string
  name: string
  tier: ReplyTier | undefined
  tierExplicit: boolean
  protocol: string | undefined
  muted: boolean
  defaultTier: ReplyTier
  perHour: number
  t: Translate
  onClose?: () => void
}) {
  const [draft, setDraft] = useState(protocol ?? '')
  const [busy, setBusy] = useState(false)
  const [note, setNote] = useState<{ kind: 'ok' | 'error'; text: string } | undefined>(undefined)
  useEffect(() => { setDraft(protocol ?? '') }, [fp, protocol])

  const save = async (patch: { tier?: ReplyTier | ''; protocol?: string; muted?: boolean }): Promise<void> => {
    setBusy(true)
    setNote(undefined)
    try {
      await api.friendSet(fp, patch)
      await networkStore.refresh()
      setNote({ kind: 'ok', text: t('friend.protocol.saved') })
    } catch (e: unknown) {
      setNote({ kind: 'error', text: e instanceof Error ? e.message : String(e) })
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="sm-friend-settings" data-soulmirror-friend-settings={fp}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
        <div className="sm-section" style={{ padding: 0, flex: 1 }}>{t('friend.settings.title', { name })}</div>
        {onClose !== undefined ? <button type="button" className="sm-linkbtn" onClick={onClose}>{t('inbox.close')}</button> : null}
      </div>
      <label>
        <span>{t('friend.tier')}</span>
        <TierSelect value={tierExplicit ? tier : undefined} defaultTier={defaultTier} disabled={busy} t={t} onChange={(next) => { void save({ tier: next ?? '' }) }} />
        <span>{t('friend.tier.hint', { n: perHour })}</span>
      </label>
      <label style={{ display: 'flex', alignItems: 'center', gap: 8, flexDirection: 'row' }}>
        <input type="checkbox" checked={muted} disabled={busy} onChange={(e) => { void save({ muted: e.target.checked }) }} data-soulmirror-friend-mute />
        <span>{t('friend.mute')}</span>
      </label>
      <label>
        <span>{t('friend.protocol')}</span>
        <textarea className="sm-textarea-box" value={draft} placeholder={t('friend.protocol.placeholder')} onChange={(e) => { setDraft(e.target.value) }} data-soulmirror-friend-protocol />
      </label>
      <div className="sm-alter-actions">
        {note !== undefined ? <span style={{ marginRight: 'auto', fontSize: 11.5 }} className={note.kind === 'error' ? 'sm-alter-error' : 'sm-muted'}>{note.kind === 'error' ? t('settings.error', { message: note.text }) : note.text}</span> : null}
        <button type="button" className="sm-ghostbtn" disabled={busy || draft === (protocol ?? '')} onClick={() => { void save({ protocol: draft }) }} data-soulmirror-friend-protocol-save>{t('friend.protocol.save')}</button>
      </div>
    </div>
  )
}

/** The global diplomacy protocol editor (protocol.md), used by the friend list and the settings section. */
export function ProtocolEditor({ t, compact }: { t: Translate; compact?: boolean }) {
  const net = useSyncExternalStore(networkStore.subscribe, networkStore.getSnapshot)
  const [text, setText] = useState<string | undefined>(undefined)
  const [saved, setSaved] = useState<string | undefined>(undefined)
  const [path, setPath] = useState<string>(net.state?.alter?.protocolPath ?? '')
  const [exists, setExists] = useState<boolean>(net.state?.alter?.protocolExists ?? true)
  const [busy, setBusy] = useState(false)
  const [note, setNote] = useState<{ kind: 'ok' | 'error'; text: string } | undefined>(undefined)

  const load = async (): Promise<void> => {
    setBusy(true)
    try {
      const r = await api.protocolGet()
      setText(r.text)
      setSaved(r.text)
      setPath(r.path)
      setExists(r.exists)
      setNote(undefined)
    } catch (e: unknown) {
      setNote({ kind: 'error', text: e instanceof Error ? e.message : String(e) })
    } finally {
      setBusy(false)
    }
  }
  useEffect(() => { void load() }, [])

  const save = async (): Promise<void> => {
    if (text === undefined) return
    setBusy(true)
    try {
      const r = await api.protocolSet(text)
      setSaved(r.text)
      setText(r.text)
      setExists(true)
      setNote({ kind: 'ok', text: t('protocol.saved') })
    } catch (e: unknown) {
      setNote({ kind: 'error', text: e instanceof Error ? e.message : String(e) })
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="sm-protocol" data-soulmirror-protocol>
      {compact ? null : <strong>{t('protocol.title')}</strong>}
      <span className="sm-muted" style={{ fontSize: 11.5 }}>{t('protocol.hint', { path })}</span>
      {!exists ? <span className="sm-alter-warn" style={{ fontSize: 11.5 }}>{t('protocol.missing')}</span> : null}
      <textarea
        className="sm-textarea-box"
        value={text ?? ''}
        disabled={text === undefined}
        onChange={(e) => { setText(e.target.value) }}
        spellCheck={false}
        data-soulmirror-protocol-text
      />
      <div className="sm-alter-actions">
        {note !== undefined ? <span style={{ marginRight: 'auto', fontSize: 11.5 }} className={note.kind === 'error' ? 'sm-alter-error' : 'sm-muted'}>{note.kind === 'error' ? t('settings.error', { message: note.text }) : note.text}</span> : null}
        <button type="button" className="sm-ghostbtn" disabled={busy} onClick={() => { void load() }}>{t('protocol.reload')}</button>
        <button type="button" className="sm-ghostbtn" disabled={busy || text === undefined || text === saved} onClick={() => { void save() }} data-soulmirror-protocol-save>{t('protocol.save')}</button>
      </div>
    </div>
  )
}
