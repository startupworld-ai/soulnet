/**
 * `settings.section` "SoulMirror network": backend status, identity (create on
 * first run; card URI + copy + backup warning), connection settings bound to
 * the host `soulmirror` settings namespace, the alter settings (default reply
 * tier, auto replies per hour, the debug "send as myself" toggle, the global
 * diplomacy protocol editor, open the page / the alter's dsh session),
 * friends (presence / unread / drafts / open on the page), pending requests
 * (accept / reject) and an add-friend form.
 */
import { useCallback, useEffect, useState, useSyncExternalStore } from 'react'
import type { CSSProperties } from 'react'
// Type-only: the 'settings.section' SlotMap row (declared by ui-settings).
import type {} from '@deepseek-ai/dsh-client-ui-settings/client'
import type { InjectFace, PropsLocale, PropsRuntime } from '@deepseek-ai/dsh-client-ui-slots'
import type { SettingsScope } from '@deepseek-ai/dsh-client-runtime/client'
import { api, networkStore, type ApiPending, type ReplyTier } from './api.ts'
import type { NS } from './locales.ts'
import { pageStore } from './page-store.ts'
import { upgradeStore } from './upgrade-store.ts'

type SettingsSectionProps = PropsRuntime<'settings.section'>

export interface SoulmirrorSettingsValues {
  relay?: string
  displayName?: string
  backend?: string
  peerBinary?: string
  home?: string
  defaultTier?: ReplyTier
  autoReplyPerHour?: number
  directSend?: boolean
  paygateBinary?: string
  paygatePort?: number
  paygateProxy?: string
  cdpKeyId?: string
  cdpKeySecret?: string
  cdpWalletSecret?: string
  cdpNetwork?: string
  /** 'comms' = SoulMirror-only preset; 'full' = dsh standard preset (shell/files). */
  alterMode?: 'comms' | 'full'
}

export interface SoulmirrorSettingsInjected {
  /** Bound settings scope of the `soulmirror` namespace. */
  scope: SettingsScope<SoulmirrorSettingsValues>
}

export type SoulmirrorSettingsProps =
  SettingsSectionProps & InjectFace<SoulmirrorSettingsInjected> & PropsLocale<typeof NS>

const card: CSSProperties = { border: '1px solid rgba(127,127,127,.25)', borderRadius: 10, padding: '10px 12px', display: 'grid', gap: 8 }
const h4: CSSProperties = { margin: 0, fontSize: '0.95em' }
const small: CSSProperties = { opacity: 0.7, fontSize: '0.82em', margin: 0 }
const input: CSSProperties = { font: 'inherit', padding: '6px 8px', borderRadius: 6, border: '1px solid rgba(127,127,127,.35)', background: 'transparent', color: 'inherit', width: '100%', boxSizing: 'border-box' }
const mono: CSSProperties = { fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace', fontSize: '0.8em', wordBreak: 'break-all', userSelect: 'all' }
const rowStyle: CSSProperties = { display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }
const warn: CSSProperties = { ...card, borderColor: 'rgba(255,160,0,.55)', background: 'rgba(255,160,0,.08)' }

function useCopy(): [copied: boolean, copy: (text: string) => void] {
  const [copied, setCopied] = useState(false)
  const copy = useCallback((text: string) => {
    void navigator.clipboard?.writeText(text).then(() => {
      setCopied(true)
      setTimeout(() => { setCopied(false) }, 1500)
    }).catch(() => {})
  }, [])
  return [copied, copy]
}

export function CardBlock({ cardUri, home, t }: { cardUri: string; home: string; t: SoulmirrorSettingsProps['t'] }) {
  const [copied, copy] = useCopy()
  const sep = home.includes('\\') ? '\\' : '/'
  return (
    <>
      <div style={card}>
        <h4 style={h4}>{t('settings.card')}</h4>
        <div style={mono} data-soulmirror-card-uri>{cardUri}</div>
        <div style={rowStyle}>
          <button type="button" onClick={() => { copy(cardUri) }}>{copied ? t('settings.card.copied') : t('settings.card.copy')}</button>
          <span style={small}>{t('settings.card.hint')}</span>
        </div>
      </div>
      <div style={warn} role="note">
        <h4 style={h4}>⚠ {t('settings.backup')}</h4>
        <p style={{ ...small, opacity: 0.9 }}>{t('settings.backup.body', { path: `${home}${sep}a2a${sep}identity.json` })}</p>
      </div>
    </>
  )
}


export function WalletBlock({ t }: { t: SoulmirrorSettingsProps['t'] }) {
  const [copied, copy] = useCopy()
  const [wallet, setWallet] = useState<{ address?: string; network?: string; balance_usdc?: string; balance_eth?: string } | null | undefined>(undefined)
  const [err, setErr] = useState<string | undefined>(undefined)
  const refresh = useCallback(() => {
    void api.payWallet().then((r) => {
      setWallet(r.wallet ?? null)
      setErr(r.error)
    }).catch(() => { setWallet(null) })
  }, [])
  useEffect(() => {
    refresh()
    // Keep the balance current after transfers settle on-chain.
    const timer = setInterval(refresh, 15000)
    return () => { clearInterval(timer) }
  }, [refresh])
  return (
    <div style={card} data-soulmirror-wallet-block>
      <h4 style={h4}>{t('settings.wallet')}</h4>
      {wallet === undefined
        ? <p style={small}>{t('settings.wallet.loading')}</p>
        : wallet === null
          ? (
            <>
              <p style={small}>{err !== undefined ? err : t('settings.wallet.none')}</p>
              <div style={rowStyle}>
                <button type="button" onClick={refresh} data-soulmirror-wallet-refresh>{t('settings.wallet.refresh')}</button>
              </div>
            </>
          )
          : (
            <>
              <div style={{ display: 'grid', gap: 4 }}>
                <div style={rowStyle}>
                  <span style={{ opacity: 0.8, fontSize: '0.82em' }}>{t('settings.wallet.address')}</span>
                  {wallet.network !== undefined ? <span style={{ ...small, fontSize: '0.75em' }}>{wallet.network}</span> : null}
                </div>
                <div style={mono} data-soulmirror-wallet-address>{wallet.address}</div>
                <div style={rowStyle}>
                  <button type="button" onClick={() => { void copy(wallet.address ?? '') }}>{copied ? t('settings.card.copied') : t('settings.card.copy')}</button>
                  <button type="button" onClick={refresh} data-soulmirror-wallet-refresh>{t('settings.wallet.refresh')}</button>
                </div>
                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 6, marginTop: 2 }}>
                  <span style={small}>{t('settings.wallet.balanceUsdc')}: <strong>{wallet.balance_usdc ?? '—'}</strong></span>
                  <span style={small}>{t('settings.wallet.balanceEth')}: <strong>{wallet.balance_eth ?? '—'}</strong></span>
                </div>
              </div>
            </>
          )}
    </div>
  )
}

function SettingField({ label, field, scope, values, user, writable, type = 'text', options, optionLabel, min }: {
  label: string
  field: keyof SoulmirrorSettingsValues
  scope: SettingsScope<SoulmirrorSettingsValues>
  values: SoulmirrorSettingsValues | undefined
  user: Record<string, unknown>
  writable: boolean
  type?: 'text' | 'password' | 'select' | 'number'
  options?: readonly string[]
  optionLabel?: (option: string) => string
  min?: number
}) {
  const current = values?.[field] ?? ''
  const [draft, setDraft] = useState(String(current))
  useEffect(() => { setDraft(String(current)) }, [current])
  const commit = (): void => {
    if (draft === String(current)) return
    const value: unknown = type === 'number' ? Math.max(min ?? 0, Math.floor(Number(draft) || 0)) : draft
    void scope.set(field, value as never).catch(() => {})
  }
  const overridden = field in user
  return (
    <label style={{ display: 'grid', gap: 4, fontSize: '0.85em' }}>
      <span style={{ opacity: 0.8 }}>{label}{overridden ? ' *' : ''}</span>
      {type === 'select'
        ? (
          <select value={draft} disabled={!writable} onChange={(e) => { setDraft(e.target.value); void scope.set(field, e.target.value as never).catch(() => {}) }} style={input} data-soulmirror-setting={field}>
            {(options ?? []).map(o => <option key={o} value={o}>{optionLabel === undefined ? o : optionLabel(o)}</option>)}
          </select>
        )
        : <input style={input} type={type === 'password' ? 'password' : (type === 'number' ? 'number' : 'text')} min={min} value={draft} disabled={!writable} onChange={e => { setDraft(e.target.value) }} onBlur={commit} onKeyDown={(e) => { if (e.key === 'Enter') commit() }} data-soulmirror-setting={field} />}
    </label>
  )
}

/**
 * "Version & updates": the plugin self-upgrade (host routes `upgrade.check` /
 * `upgrade.run`, client state in upgrade-store.ts). One click installs the
 * new version and, when the peer supports `host.relaunch`, restarts dsh and
 * reloads this page by itself; otherwise it says what to restart manually.
 */
function UpgradeCard({ t }: { t: SoulmirrorSettingsProps['t'] }) {
  const up = useSyncExternalStore(upgradeStore.subscribe, upgradeStore.getSnapshot)
  const busy = up.phase !== 'idle' && up.phase !== 'done-manual' && up.phase !== 'failed'
  return (
    <div style={card} data-soulmirror-upgrade>
      <div style={rowStyle}>
        <h4 style={h4}>{t('settings.upgrade')}</h4>
        {up.hasUpdate
          ? <span style={{ background: 'rgb(220,80,60)', color: '#fff', borderRadius: 999, padding: '1px 8px', fontSize: '0.72em' }} data-soulmirror-upgrade-badge>{t('settings.upgrade.badge')}</span>
          : null}
      </div>
      <div style={rowStyle}>
        <span style={small}>{t('settings.upgrade.current')}: <strong>{up.current !== undefined ? `v${up.current}` : '—'}</strong></span>
        <button type="button" disabled={busy} onClick={() => { void upgradeStore.check() }}>
          {up.phase === 'checking' ? t('settings.upgrade.checking') : t('settings.upgrade.check')}
        </button>
        {up.hasUpdate && up.latest !== undefined
          ? (
            <button type="button" disabled={busy} onClick={() => { void upgradeStore.run() }} data-soulmirror-upgrade-run>
              {t('settings.upgrade.do', { version: up.latest })}
            </button>
          )
          : null}
      </div>
      {up.phase === 'idle' && up.hasUpdate && up.latest !== undefined && up.current !== undefined
        ? <p style={small}>{t('settings.upgrade.available', { latest: up.latest, current: up.current })}</p>
        : null}
      {up.phase === 'idle' && !up.hasUpdate && up.checked && up.error === undefined && up.current !== undefined
        ? <p style={small}>{t('settings.upgrade.uptodate', { version: up.current })}</p>
        : null}
      {up.phase === 'installing' && up.latest !== undefined ? <p style={small}>{t('settings.upgrade.installing', { version: up.latest })}</p> : null}
      {up.phase === 'restarting' || up.phase === 'reloading' ? <p style={small} role="status">{t('settings.upgrade.restarting')}</p> : null}
      {up.phase === 'done-manual' ? <p style={{ ...small, opacity: 0.95 }} role="status">{t('settings.upgrade.manual', { version: up.current ?? '' })}</p> : null}
      {(up.phase === 'failed' || up.error !== undefined) && up.error !== undefined
        ? <p style={{ ...small, color: 'rgb(220,80,60)' }} role="alert">{t('settings.upgrade.failed', { message: up.error })}</p>
        : null}
      {up.phase === 'failed' && up.output !== undefined && up.output !== ''
        ? <pre style={{ ...mono, whiteSpace: 'pre-wrap', maxHeight: 160, overflow: 'auto', margin: 0, opacity: 0.8, userSelect: 'text' }}>{up.output}</pre>
        : null}
      <p style={small}>{t('settings.upgrade.hint')}</p>
    </div>
  )
}

const BINARY_SOURCE_KEYS = {
  'setting': 'settings.binary.setting',
  'platform-package': 'settings.binary.platform-package',
  'path': 'settings.binary.path',
  'plugin-bin': 'settings.binary.plugin-bin',
} as const
function binarySourceLabel(t: SoulmirrorSettingsProps['t'], source: string): string {
  const key = (BINARY_SOURCE_KEYS as Record<string, (typeof BINARY_SOURCE_KEYS)[keyof typeof BINARY_SOURCE_KEYS] | undefined>)[source]
  return key === undefined ? source : t(key)
}

export function SoulmirrorSettingsSection({ scope, t }: SoulmirrorSettingsProps) {
  const net = useSyncExternalStore(networkStore.subscribe, networkStore.getSnapshot)
  // The scope is a class instance (methods need `this`): wrap instead of passing them unbound.
  const subscribeScope = useCallback((listener: () => void) => scope.subscribe(listener), [scope])
  const readScope = useCallback(() => scope.getSnapshot(), [scope])
  const settings = useSyncExternalStore(subscribeScope, readScope)
  const state = net.state
  const status = net.status ?? state?.status
  const [name, setName] = useState('')
  const [busy, setBusy] = useState<string | undefined>(undefined)
  const [message, setMessage] = useState<string | undefined>(undefined)
  const [error, setError] = useState<string | undefined>(undefined)
  const [addUri, setAddUri] = useState('')
  const [addNote, setAddNote] = useState('')

  const run = async (key: string, action: () => Promise<string | undefined>): Promise<void> => {
    setBusy(key)
    setError(undefined)
    setMessage(undefined)
    try {
      const note = await action()
      if (note !== undefined) setMessage(note)
      await networkStore.refresh()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(undefined)
    }
  }

  const writable = settings.status === 'ready' && settings.writable
  const userLayer = (typeof settings.user === 'object' && settings.user !== null ? settings.user : {}) as Record<string, unknown>
  const displayName = settings.value?.displayName ?? ''
  useEffect(() => { if (name === '' && displayName !== '') setName(displayName) }, [displayName, name])
  const directSend = settings.value?.directSend === true

  return (
    <div style={{ display: 'grid', gap: 12 }} data-soulmirror-settings>
      <h3 style={{ margin: 0 }}>{t('settings.title')}</h3>
      <p style={small}>{t('settings.intro')}</p>

      <div style={card}>
        <div style={rowStyle}>
          <h4 style={h4}>{t('settings.status')}</h4>
          <span>{state?.backend === 'fake' ? t('settings.status.fake') : t('settings.status.soulnet')}</span>
          {status !== undefined ? <span style={{ opacity: 0.8 }}>· {t(`settings.state.${status.state}`)}{status.pid !== undefined ? ` (pid ${status.pid})` : ''}{status.restarts > 0 ? ` · restarts ${status.restarts}` : ''}</span> : null}
          <button type="button" onClick={() => { void networkStore.refresh() }} disabled={net.loading}>{t('settings.refresh')}</button>
        </div>
        {status?.lastError !== undefined ? <p style={{ ...small, color: 'rgb(220,80,60)' }}>{status.lastError}</p> : null}
        {net.error !== undefined ? <p style={{ ...small, color: 'rgb(220,80,60)' }}>{t('settings.error', { message: net.error })}</p> : null}
        {state?.error !== undefined ? <p style={{ ...small, color: 'rgb(220,80,60)' }}>{t('settings.error', { message: state.error })}</p> : null}
        {status?.relay !== undefined || state?.home !== undefined
          ? <p style={small}>{status?.relay ?? ''}{status?.relay !== undefined ? ' · ' : ''}{state?.home ?? ''}</p>
          : null}
        {status?.binary !== undefined
          ? <p style={small} data-soulmirror-binary>{t('settings.binary')}: <code style={{ wordBreak: 'break-all' }}>{status.binary}</code>{status.binarySource !== undefined ? ` (${binarySourceLabel(t, status.binarySource)})` : ''}{status.version !== undefined ? ` · soulnet ${status.version}` : ''}</p>
          : null}
      </div>

      <div style={card}>
        <h4 style={h4}>{t('settings.identity')}</h4>
        {state?.identity
          ? (
            <div style={rowStyle}>
              <strong>{state.identity.name}</strong>
              <span style={mono}>{state.identity.fp}</span>
            </div>
          )
          : (
            <>
              <p style={small}>{t('settings.identity.none')}</p>
              <div style={rowStyle}>
                <input style={{ ...input, maxWidth: 280 }} placeholder={t('settings.identity.name')} value={name} onChange={e => { setName(e.target.value) }} />
                <button
                  type="button"
                  disabled={busy !== undefined || name.trim() === '' || status?.state === 'error'}
                  onClick={() => { void run('create', async () => { await api.createIdentity(name.trim()); return undefined }) }}
                >
                  {busy === 'create' ? t('settings.identity.creating') : t('settings.identity.create')}
                </button>
              </div>
            </>
          )}
      </div>

      {state?.identity ? <CardBlock cardUri={state.identity.cardUri} home={state.home} t={t} /> : null}

      <div style={card}>
        <h4 style={h4}>{t('settings.config')}</h4>
        {settings.status === 'unavailable' || settings.mode === 'memory' ? <p style={small}>{t('settings.config.unavailable')}</p> : null}
        <SettingField label={t('settings.config.relay')} field="relay" scope={scope} values={settings.value} user={userLayer} writable={writable} />
        <SettingField label={t('settings.config.displayName')} field="displayName" scope={scope} values={settings.value} user={userLayer} writable={writable} />
        <SettingField label={t('settings.config.backend')} field="backend" scope={scope} values={settings.value} user={userLayer} writable={writable} type="select" options={['soulnet', 'fake']} />
        <SettingField label={t('settings.config.peerBinary')} field="peerBinary" scope={scope} values={settings.value} user={userLayer} writable={writable} />
        <SettingField label={t('settings.config.home')} field="home" scope={scope} values={settings.value} user={userLayer} writable={writable} />
        <p style={small}>{t('settings.config.hint')}</p>
      </div>

      <UpgradeCard t={t} />

      <div style={card} data-soulmirror-settings-alter>
        <h4 style={h4}>{t('settings.alter')}</h4>
        <p style={small}>{t('settings.alter.intro')}</p>
        <div style={rowStyle}>
          <button type="button" onClick={() => { pageStore.open('alter') }} data-soulmirror-settings-open-page>{t('settings.alter.openPage')}</button>
          {state?.alter?.legacyFriendSessions !== undefined && Object.keys(state.alter.legacyFriendSessions).length > 0
            ? <span style={small}>{t('settings.alter.legacy', { n: Object.keys(state.alter.legacyFriendSessions).length })}</span>
            : null}
        </div>
        <label style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: '0.85em' }}>
          <input type="checkbox" checked={directSend} disabled={!writable} onChange={(e) => { void scope.set('directSend', e.target.checked).catch(() => {}) }} data-soulmirror-setting="directSend" />
          <span>{t('settings.alter.directSend')}</span>
        </label>
      </div>

      <WalletBlock t={t} />

      <div style={card} data-soulmirror-settings-pay>
        <h4 style={h4}>{t('settings.pay')}</h4>
        <p style={small}>{t('settings.pay.intro')}</p>
        <SettingField label={t('settings.pay.cdpKeyId')} field="cdpKeyId" scope={scope} values={settings.value} user={userLayer} writable={writable} />
        <SettingField label={t('settings.pay.cdpKeySecret')} field="cdpKeySecret" scope={scope} values={settings.value} user={userLayer} writable={writable} type="password" />
        <SettingField label={t('settings.pay.cdpWalletSecret')} field="cdpWalletSecret" scope={scope} values={settings.value} user={userLayer} writable={writable} type="password" />
        <SettingField label={t('settings.pay.cdpNetwork')} field="cdpNetwork" scope={scope} values={settings.value} user={userLayer} writable={writable} type="select" options={['base-sepolia', 'base']} />
        <SettingField label={t('settings.pay.paygateProxy')} field="paygateProxy" scope={scope} values={settings.value} user={userLayer} writable={writable} />
        <SettingField label={t('settings.pay.paygateBinary')} field="paygateBinary" scope={scope} values={settings.value} user={userLayer} writable={writable} />
        <SettingField label={t('settings.pay.paygatePort')} field="paygatePort" scope={scope} values={settings.value} user={userLayer} writable={writable} type="number" min={1} />
        <p style={small}>{t('settings.pay.hint')}</p>
      </div>


      <div style={card}>
        <h4 style={h4}>{t('settings.friends')}</h4>
        <p style={small}>{t('settings.friends.hint')}</p>
        {(state?.friends.length ?? 0) === 0
          ? <p style={small}>{t('settings.friends.none')}</p>
          : (
            <ul style={{ listStyle: 'none', padding: 0, margin: 0, display: 'grid', gap: 6 }}>
              {state?.friends.map(f => (
                <li key={f.fp} style={rowStyle}>
                  <span title={f.online === true ? t('settings.friends.online') : t('settings.friends.offline')} style={{ color: f.online === true ? 'rgb(60,180,90)' : 'rgba(127,127,127,.7)' }}>●</span>
                  <span style={{ flex: 1, minWidth: 160 }}>
                    <strong>{f.name}</strong>
                    {f.cardName !== undefined && f.cardName !== f.name ? <span style={{ opacity: 0.6 }}> ({f.cardName})</span> : null}
                    <span style={{ ...mono, opacity: 0.6 }}> {f.fp.slice(0, 16)}…</span>
                    {f.tier !== undefined ? <span style={{ marginLeft: 8, opacity: 0.7 }}>{t(`tier.short.${f.tier}`)}</span> : null}
                    {f.unread > 0 ? <span style={{ marginLeft: 8, color: 'var(--dsw-alias-brand-primary)' }}>{t('settings.friends.unread', { n: f.unread })}</span> : null}
                    {(f.drafts ?? 0) > 0 ? <span style={{ marginLeft: 8, color: 'var(--dsw-alias-state-warn-primary)' }}>{t('page.drafts.count', { n: f.drafts ?? 0 })}</span> : null}
                    {net.typing[f.fp] === true || f.typing === true ? <span style={{ marginLeft: 8, opacity: 0.8 }}>{t('settings.friends.typing')}</span> : null}
                  </span>
                  <button type="button" onClick={() => { pageStore.open(f.fp) }}>{t('settings.friends.open')}</button>
                </li>
              ))}
            </ul>
          )}
      </div>

      <div style={card}>
        <h4 style={h4}>{t('settings.pending')}{(state?.pending.length ?? 0) > 0 ? ` (${state?.pending.length ?? 0})` : ''}</h4>
        {(state?.pending.length ?? 0) === 0
          ? <p style={small}>{t('settings.pending.none')}</p>
          : (
            <ul style={{ listStyle: 'none', padding: 0, margin: 0, display: 'grid', gap: 6 }}>
              {state?.pending.map((p: ApiPending) => (
                <li key={p.id} style={rowStyle}>
                  <span style={{ flex: 1, minWidth: 160 }}>
                    <strong>{p.name}</strong> <span style={{ ...mono, opacity: 0.6 }}>{p.fp.slice(0, 16)}…</span>
                    {p.greeting !== '' ? <span style={{ opacity: 0.75 }}> — “{p.greeting}”</span> : null}
                  </span>
                  <button type="button" disabled={busy !== undefined} onClick={() => { void run(`accept:${p.id}`, async () => { const r = await api.accept(p.id); pageStore.open(r.friend.fp); return undefined }) }}>{t('settings.pending.accept')}</button>
                  <button type="button" disabled={busy !== undefined} onClick={() => { void run(`reject:${p.id}`, async () => { await api.reject(p.id); return undefined }) }}>{t('settings.pending.reject')}</button>
                </li>
              ))}
            </ul>
          )}
      </div>

      <div style={card}>
        <h4 style={h4}>{t('settings.add')}</h4>
        <input style={input} placeholder={t('settings.add.uri')} value={addUri} onChange={e => { setAddUri(e.target.value) }} />
        <input style={input} placeholder={t('settings.add.note')} value={addNote} onChange={e => { setAddNote(e.target.value) }} />
        <div style={rowStyle}>
          <button
            type="button"
            disabled={busy !== undefined || addUri.trim() === '' || !state?.identity}
            onClick={() => {
              void run('add', async () => {
                const { friend } = await api.addFriend(addUri.trim(), addNote.trim() === '' ? undefined : addNote.trim())
                setAddUri('')
                setAddNote('')
                return t('settings.add.sent', { name: friend.name })
              })
            }}
          >
            {t('settings.add.send')}
          </button>
          {message !== undefined ? <span style={small}>{message}</span> : null}
        </div>
      </div>

      {error !== undefined ? <p style={{ ...small, color: 'rgb(220,80,60)' }} role="alert">{t('settings.error', { message: error })}</p> : null}
    </div>
  )
}
