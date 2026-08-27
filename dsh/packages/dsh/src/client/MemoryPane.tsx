/**
 * The memory page (added as a pane tab for the alter and every seat agent):
 * list, add-by-hand, edit and delete memories, each tagged with its origin
 * (auto-extracted vs manual) and its scope (global vs this agent vs group).
 * The add and edit forms let the owner pick the scope (global vs only this
 * agent / only this group) instead of forcing the pane's fixed scope.
 */
import { useCallback, useEffect, useState } from 'react'
import { api, type ApiMemory } from './api.ts'
import type { Translate } from './translate.ts'

export interface MemoryPaneProps {
  t: Translate
  /** Which memories to list. */
  allow: { global?: boolean; agent?: string; group?: string; friend?: string }
  /** The scope new hand-added memories land in (this pane's identity). */
  scope: { kind: 'global' } | { kind: 'agent'; name: string } | { kind: 'shared-group'; gid: string } | { kind: 'shared-friend'; fp: string }
}

/** The scopes this pane can write to (global always, plus its own when it has one). */
type ScopeKind = 'global' | 'agent' | 'shared-group' | 'shared-friend'

const pill: React.CSSProperties = { fontSize: 10.5, padding: '1px 6px', borderRadius: 8, border: '1px solid rgba(127,127,127,.4)', color: 'var(--dsw-alias-label-secondary)', whiteSpace: 'nowrap' }
const autoPill: React.CSSProperties = { ...pill, borderColor: 'rgba(90,150,255,.5)', color: 'var(--dsw-alias-accent, #4c8dff)' }
const manualPill: React.CSSProperties = { ...pill, borderColor: 'rgba(140,200,120,.5)', color: 'var(--dsw-alias-state-success-primary, #4ade80)' }

function scopeLabel(t: Translate, scope: ApiMemory['scope']): string {
  switch (scope.kind) {
    case 'global': return t('memory.scope.global')
    case 'agent': return t('memory.scope.agent', { name: scope.name ?? '' })
    case 'shared-group': return t('memory.scope.group')
    case 'shared-friend': return t('memory.scope.friend')
    default: return scope.kind
  }
}

export function MemoryPane({ t, allow, scope }: MemoryPaneProps) {
  const [items, setItems] = useState<ApiMemory[]>([])
  const [loading, setLoading] = useState(true)
  const [draft, setDraft] = useState('')
  const [draftKind, setDraftKind] = useState<ScopeKind>(scope.kind === 'agent' ? 'agent' : scope.kind === 'shared-group' ? 'shared-group' : scope.kind === 'shared-friend' ? 'shared-friend' : 'global')
  const [editing, setEditing] = useState<string | undefined>(undefined)
  const [editText, setEditText] = useState('')
  const [editKind, setEditKind] = useState<ScopeKind>('global')
  const [busy, setBusy] = useState(false)
  /** Which scope the list is filtered to (`all` = show every allowed memory). */
  const [filter, setFilter] = useState<'all' | ScopeKind>('all')

  const agentName = scope.kind === 'agent' ? scope.name : undefined
  const groupGid = scope.kind === 'shared-group' ? scope.gid : undefined
  const friendFp = scope.kind === 'shared-friend' ? scope.fp : undefined

  /** Build the memory scope object for a selectable kind, using this pane's identity. */
  const scopeFor = (kind: ScopeKind): MemoryPaneProps['scope'] =>
    kind === 'agent' ? { kind: 'agent', name: agentName ?? '' }
      : kind === 'shared-group' ? { kind: 'shared-group', gid: groupGid ?? '' }
        : kind === 'shared-friend' ? { kind: 'shared-friend', fp: friendFp ?? '' }
          : { kind: 'global' }

  /** The scope choices this pane offers (global always; its own only when it has one). */
  const scopeOptions: Array<{ kind: ScopeKind; label: string }> = scope.kind === 'agent'
    ? [
        { kind: 'global', label: t('memory.scope.global') },
        { kind: 'agent', label: t('memory.scope.onlyAgent') },
      ]
    : scope.kind === 'shared-group'
      ? [
          { kind: 'global', label: t('memory.scope.global') },
          { kind: 'shared-group', label: t('memory.scope.onlyGroup') },
        ]
      : scope.kind === 'shared-friend'
        ? [
            { kind: 'global', label: t('memory.scope.global') },
            { kind: 'shared-friend', label: t('memory.scope.onlyFriend') },
          ]
        : [{ kind: 'global', label: t('memory.scope.global') }]

  /** The list filtered by the active filter (`all` = everything allowed). */
  const filteredItems = filter === 'all' ? items : items.filter(m => m.scope.kind === filter)

  const reload = useCallback(() => {
    setLoading(true)
    api.memoryList(allow)
      .then(({ memories }) => { setItems(memories) })
      .catch(() => {})
      .finally(() => { setLoading(false) })
  }, [allow])

  useEffect(() => { reload() }, [reload])

  const add = (): void => {
    if (draft.trim() === '' || busy) return
    setBusy(true)
    api.memoryAdd({ kind: 'fact', content: draft.trim(), scope: scopeFor(draftKind) })
      .then(() => { setDraft(''); reload() })
      .catch(() => {})
      .finally(() => { setBusy(false) })
  }

  const saveEdit = (uid: string): void => {
    if (editText.trim() === '') return
    api.memoryUpdate(uid, editText.trim(), scopeFor(editKind))
      .then(() => { setEditing(undefined); reload() })
      .catch(() => {})
  }

  const remove = (uid: string): void => {
    api.memoryRemove(uid).then(() => { reload() }).catch(() => {})
  }

  const scopePicker = (value: ScopeKind, onChange: (k: ScopeKind) => void, dataAttr: string): JSX.Element | null => {
    if (scopeOptions.length <= 1) return null
    return (
      <label style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 12, opacity: 0.85, whiteSpace: 'nowrap' }}>
        <span>{t('memory.scope.label')}</span>
        <select className="sm-select" style={{ fontSize: 12, padding: '3px 6px' }} value={value} onChange={(e) => { onChange(e.target.value as ScopeKind) }} data-soulmirror-memory-scope-pick={dataAttr}>
          {scopeOptions.map(o => <option key={o.kind} value={o.kind}>{o.label}</option>)}
        </select>
      </label>
    )
  }

  return (
    <div className="sm-home" data-soulmirror-memory-pane>
      <div className="sm-home-inner">
        <div style={{ display: 'flex', gap: 8, alignItems: 'flex-end' }}>
          <textarea
            className="sm-textarea-box"
            style={{ flex: 1, minHeight: 40, resize: 'vertical' }}
            value={draft}
            placeholder={t('memory.add.placeholder')}
            onChange={(e) => { setDraft(e.target.value) }}
            data-soulmirror-memory-draft
          />
          <button type="button" className="sm-ghostbtn" disabled={busy || draft.trim() === ''} onClick={add} data-soulmirror-memory-add>{t('memory.add')}</button>
        </div>
        {scopePicker(draftKind, setDraftKind, 'add')}

        {scopeOptions.length > 1
          ? (
            <div style={{ display: 'flex', alignItems: 'center', gap: 6, flexWrap: 'wrap', marginTop: 8 }}>
              <span style={{ fontSize: 12, opacity: 0.85 }}>{t('memory.filter.label')}</span>
              {[{ kind: 'all' as const, label: t('memory.filter.all') }, ...scopeOptions].map(o => (
                <button
                  key={o.kind}
                  type="button"
                  className="sm-ghostbtn"
                  style={{
                    fontSize: 12,
                    padding: '2px 10px',
                    borderRadius: 12,
                    ...(filter === o.kind
                      ? { borderColor: 'var(--dsw-alias-accent, #4c8dff)', color: 'var(--dsw-alias-accent, #4c8dff)' }
                      : {}),
                  }}
                  aria-pressed={filter === o.kind}
                  onClick={() => { setFilter(o.kind) }}
                  data-soulmirror-memory-filter={o.kind}
                >
                  {o.label}
                </button>
              ))}
            </div>
          )
          : null}

        {loading
          ? <span className="sm-muted" style={{ fontSize: 12, padding: '8px 0', display: 'block' }}>{t('page.thread.loading')}</span>
          : items.length === 0
            ? <span className="sm-muted" style={{ fontSize: 12, padding: '8px 0', display: 'block' }}>{t('memory.empty.page')}</span>
            : filteredItems.length === 0
              ? <span className="sm-muted" style={{ fontSize: 12, padding: '8px 0', display: 'block' }}>{t('memory.empty.filter')}</span>
              : null}

        <div style={{ display: 'grid', gap: 8, marginTop: 8 }}>
          {filteredItems.map(m => (
            <div key={m.uid} className="sm-home-card" style={{ padding: '10px 12px' }} data-soulmirror-memory={m.uid}>
              <div style={{ display: 'flex', gap: 6, alignItems: 'center', flexWrap: 'wrap' }}>
                <span style={m.origin === 'manual' ? manualPill : autoPill} data-soulmirror-memory-origin={m.origin}>{m.origin === 'manual' ? t('memory.origin.manual') : t('memory.origin.auto')}</span>
                <span style={pill} data-soulmirror-memory-scope={m.scope.kind}>{scopeLabel(t, m.scope)}</span>
              </div>
              {editing === m.uid
                ? (
                  <div style={{ display: 'grid', gap: 8, marginTop: 8 }}>
                    <textarea className="sm-textarea-box" style={{ flex: 1, minHeight: 40 }} value={editText} onChange={(e) => { setEditText(e.target.value) }} />
                    <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                      {scopePicker(editKind, setEditKind, 'edit')}
                      <span style={{ flex: 1 }} />
                      <button type="button" className="sm-ghostbtn" onClick={() => { saveEdit(m.uid) }} data-soulmirror-memory-save>{t('memory.save')}</button>
                      <button type="button" className="sm-ghostbtn" onClick={() => { setEditing(undefined) }}>{t('inbox.close')}</button>
                    </div>
                  </div>
                )
                : (
                  <>
                    <div style={{ marginTop: 6, fontSize: 13, lineHeight: 1.5, whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{m.content}</div>
                    <div style={{ display: 'flex', gap: 8, marginTop: 6 }}>
                      <button type="button" className="sm-linkbtn sm-muted" style={{ fontSize: 11.5 }} onClick={() => { setEditing(m.uid); setEditText(m.content); setEditKind(m.scope.kind === 'agent' ? 'agent' : m.scope.kind === 'shared-group' ? 'shared-group' : m.scope.kind === 'shared-friend' ? 'shared-friend' : 'global') }} data-soulmirror-memory-edit>{t('memory.edit')}</button>
                      <button type="button" className="sm-linkbtn sm-muted" style={{ fontSize: 11.5 }} onClick={() => { remove(m.uid) }} data-soulmirror-memory-delete>{t('memory.delete')}</button>
                    </div>
                  </>
                )}
            </div>
          ))}
        </div>
      </div>
    </div>
  )
}
