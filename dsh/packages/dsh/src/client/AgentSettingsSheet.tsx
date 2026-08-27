/**
 * Modal sheet creating / editing ONE named seat agent — its DEFINITION only:
 * name (the @-mention token), working directory, capability preset, approval
 * switch, remove. WHERE it works and WHO may command it there is per group —
 * the group chat's agents sheet (./GroupAgentsSheet.tsx), where the member
 * list is at hand. Opened from the list's "add agent" button and from the
 * agent pane's settings button. Self-contained overlay
 * (data-soulmirror-modal keeps the page's Escape handler from closing the
 * whole page underneath it).
 */
import { useState, type CSSProperties } from 'react'
import { api, networkStore, type ApiSeatAgent } from './api.ts'
import { pickDirectory } from './dir-picker.ts'
import type { Translate } from './translate.ts'

const field: CSSProperties = { font: 'inherit', padding: '6px 8px', borderRadius: 6, border: '1px solid rgba(127,127,127,.35)', background: 'transparent', color: 'inherit', width: '100%', boxSizing: 'border-box' }
const small: CSSProperties = { opacity: 0.75, fontSize: '0.82em', margin: 0 }
const check: CSSProperties = { display: 'flex', alignItems: 'center', gap: 8, fontSize: '0.85em' }

export function AgentSettingsSheet({ t, agent, onClose, onSaved, onRemoved }: {
  t: Translate
  /** undefined = create a new agent. */
  agent?: ApiSeatAgent
  onClose: () => void
  /** Called with the stored name after a successful save. */
  onSaved?: (name: string) => void
  onRemoved?: () => void
}) {
  const [name, setName] = useState(agent?.name ?? '')
  const [cwd, setCwd] = useState(agent?.cwd ?? '')
  const [preset, setPreset] = useState(agent?.preset ?? '')
  const [prompt, setPrompt] = useState(agent?.prompt ?? '')
  const [approval, setApproval] = useState(agent?.approval === true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | undefined>(undefined)

  const save = (): void => {
    setBusy(true)
    setError(undefined)
    void api.agentsSet({ name: name.trim(), ...(preset === '' ? {} : { preset }), ...(cwd.trim() === '' ? {} : { cwd: cwd.trim() }), ...(prompt.trim() === '' ? {} : { prompt: prompt.trim() }), ...(approval ? { approval: true } : {}) })
      .then(({ agent: stored }) => {
        void networkStore.refresh()
        onSaved?.(stored.name)
        onClose()
      })
      .catch((e: unknown) => { setError(e instanceof Error ? e.message : String(e)) })
      .finally(() => { setBusy(false) })
  }
  const remove = (): void => {
    if (agent === undefined) return
    setBusy(true)
    void api.agentsRemove(agent.name)
      .then(() => {
        void networkStore.refresh()
        onRemoved?.()
        onClose()
      })
      .catch((e: unknown) => { setError(e instanceof Error ? e.message : String(e)) })
      .finally(() => { setBusy(false) })
  }

  return (
    <div
      style={{ position: 'absolute', inset: 0, zIndex: 40, display: 'grid', placeItems: 'center', background: 'rgba(0,0,0,.35)' }}
      role="dialog"
      aria-label={t('settings.agents')}
      data-soulmirror-modal
      data-soulmirror-agent-sheet
      onMouseDown={(e) => { if (e.target === e.currentTarget) onClose() }}
    >
      <div style={{ width: 'min(420px, calc(100% - 48px))', maxHeight: 'calc(100% - 64px)', overflowY: 'auto', borderRadius: 12, border: '1px solid rgba(127,127,127,.3)', background: 'var(--dsw-alias-bg-layer-2, var(--dsw-alias-bg-base))', boxShadow: '0 12px 40px rgba(0,0,0,.28)', padding: 16, display: 'grid', gap: 10 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <strong style={{ flex: 1, fontSize: '1em' }}>{agent === undefined ? t('settings.agents.add') : t('agent.settings.title', { name: agent.name })}</strong>
          <button type="button" className="sm-ghostbtn" onClick={onClose}>{t('inbox.close')}</button>
        </div>
        <label style={{ display: 'grid', gap: 4, fontSize: '0.85em' }}>
          <span style={{ opacity: 0.8 }}>{t('settings.agents.name')}</span>
          <input style={field} value={name} disabled={agent !== undefined} onChange={e => { setName(e.target.value) }} data-soulmirror-agent-name />
        </label>
        <label style={{ display: 'grid', gap: 4, fontSize: '0.85em' }}>
          <span style={{ opacity: 0.8 }}>{t('settings.agents.cwd')}</span>
          <div style={{ display: 'flex', gap: 6 }}>
            <input style={{ ...field, flex: 1, minWidth: 0 }} value={cwd} onChange={e => { setCwd(e.target.value) }} data-soulmirror-agent-cwd />
            <button
              type="button"
              className="sm-ghostbtn"
              style={{ whiteSpace: 'nowrap' }}
              disabled={busy}
              onClick={() => { void pickDirectory().then(p => { if (p !== null) setCwd(p) }) }}
              data-soulmirror-agent-cwd-browse
            >
              {t('settings.agents.cwd.browse')}
            </button>
          </div>
        </label>
        <label style={{ display: 'grid', gap: 4, fontSize: '0.85em' }}>
          <span style={{ opacity: 0.8 }}>{t('settings.agents.preset')}</span>
          <select style={field} value={preset} onChange={e => { setPreset(e.target.value) }}>
            <option value="">{t('settings.agents.preset.default')}</option>
            {['standard', 'minimal', 'code'].map(p => <option key={p} value={p}>{p}</option>)}
          </select>
        </label>
        <label style={{ display: 'grid', gap: 4, fontSize: '0.85em' }}>
          <span style={{ opacity: 0.8 }}>{t('settings.agents.prompt')}</span>
          <textarea
            style={{ ...field, minHeight: 88, resize: 'vertical', fontFamily: 'inherit' }}
            value={prompt}
            placeholder={t('settings.agents.prompt.placeholder')}
            onChange={e => { setPrompt(e.target.value) }}
            data-soulmirror-agent-prompt
          />
        </label>
        <label style={check}>
          <input type="checkbox" checked={approval} onChange={e => { setApproval(e.target.checked) }} data-soulmirror-agent-approval />
          <span>{t('settings.agents.approval')}</span>
        </label>
        <p style={small}>{t('settings.agents.groupHint')}</p>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
          <button type="button" disabled={busy || name.trim() === ''} onClick={save} data-soulmirror-agent-save>{t('settings.agents.save')}</button>
          {agent !== undefined ? <button type="button" disabled={busy} onClick={remove} data-soulmirror-agent-remove>{t('settings.agents.remove')}</button> : null}
          {error !== undefined ? <span style={{ ...small, color: 'rgb(220,80,60)' }} role="alert">{error}</span> : null}
        </div>
      </div>
    </div>
  )
}
