/**
 * The owner's review card for one pending draft of the alter (P4): what the
 * alter wants to send to whom and why it was held, with the four decisions —
 * approve (send as the alter), edit then send, send it back to the alter with
 * feedback ("let the alter revise"), or reject. Rendered in the friend's
 * read-only thread and in the "My alter" chat; the host answers through
 * `drafts.decide` and the card disappears with the SSE `draft` frame.
 */
import { useState, useSyncExternalStore } from 'react'
import { Button, IconCheckOutline14, IconCloseFill14 } from '@deepseek-ai/dsh-client-ui-primitives'
import type { ApiDraft } from './api.ts'
import type { Translate } from './translate.ts'
import { formatAge } from './inbox-state.ts'
import { pageStore } from './page-store.ts'

export function reasonLabel(t: Translate, reason: string): string {
  switch (reason) {
    case 'draft-tier': return t('draft.reason.draft-tier')
    case 'notify-tier': return t('draft.reason.notify-tier')
    case 'rate-limited': return t('draft.reason.rate-limited')
    case 'loop-guard-auto': return t('draft.reason.loop-guard-auto')
    case 'other-friend': return t('draft.reason.other-friend')
    case 'unknown-trigger':
    default: return t('draft.reason.unknown-trigger')
  }
}

export function DraftCard({ draft, t, showTarget, onGoFriend, onGoGroup }: {
  draft: ApiDraft
  t: Translate
  /** Name the target in the head (the alter chat lists drafts to every friend). */
  showTarget?: boolean
  /** Jump to the friend's read-only thread. */
  onGoFriend?: (fp: string) => void
  /** Jump to the group's chat (group drafts). */
  onGoGroup?: (gid: string) => void
}) {
  const page = useSyncExternalStore(pageStore.subscribe, pageStore.getSnapshot)
  const busy = page.deciding.includes(draft.id)
  const [mode, setMode] = useState<'idle' | 'edit' | 'revise'>('idle')
  const [text, setText] = useState(draft.body)
  const [feedback, setFeedback] = useState('')
  const inReplyTo = draft.trigger?.kind === 'inbound' ? (draft.trigger.name ?? draft.name) : undefined
  const openTarget = draft.gid !== undefined && onGoGroup !== undefined ? () => { onGoGroup(draft.gid!) } : onGoFriend !== undefined ? () => { onGoFriend(draft.fp) } : undefined

  return (
    <div className="sm-draft" data-soulmirror-draft={draft.id} data-soulmirror-draft-fp={draft.fp}>
      <div className="sm-draft-head">
        <span className="sm-draft-tag">{t('draft.tag')}</span>
        <span>{showTarget === true ? t('draft.title.to', { name: draft.name }) : t('draft.title')}</span>
        {showTarget === true && openTarget !== undefined
          ? <button type="button" className="sm-linkbtn" onClick={openTarget}>{draft.gid !== undefined ? t('draft.openGroup') : t('draft.openThread')}</button>
          : null}
        <span className="sm-row-time">{formatAge(Date.parse(draft.createdAt))}</span>
      </div>
      {mode === 'edit'
        ? (
          <div className="sm-draft-edit">
            <textarea className="sm-textarea-box" value={text} onChange={(e) => { setText(e.target.value) }} autoFocus data-soulmirror-draft-edit />
            <div className="sm-draft-actions">
              <Button variant="primary" size="sm" disabled={busy || text.trim() === ''} onClick={() => { void pageStore.decideDraft(draft.id, { action: 'approve', body: text }).then((ok) => { if (ok) setMode('idle') }) }} data-soulmirror-draft-edit-send>
                {t('draft.editSend')}
              </Button>
              <button type="button" className="sm-ghostbtn" disabled={busy} onClick={() => { setMode('idle'); setText(draft.body) }}>{t('draft.cancel')}</button>
            </div>
          </div>
        )
        : <div className="sm-draft-body" data-soulmirror-draft-body>{draft.body}</div>}
      <div className="sm-draft-why">
        {reasonLabel(t, draft.reason)}
        {inReplyTo !== undefined ? ` · ${t('draft.inReplyTo', { name: inReplyTo })}` : ''}
      </div>
      {mode === 'revise'
        ? (
          <div className="sm-draft-edit">
            <textarea className="sm-textarea-box" value={feedback} placeholder={t('draft.revise.placeholder')} onChange={(e) => { setFeedback(e.target.value) }} autoFocus data-soulmirror-draft-feedback />
            <div className="sm-draft-actions">
              <Button variant="primary" size="sm" disabled={busy || feedback.trim() === ''} onClick={() => { void pageStore.decideDraft(draft.id, { action: 'revise', feedback }).then((ok) => { if (ok) setMode('idle') }) }} data-soulmirror-draft-revise-send>
                {t('draft.reviseSend')}
              </Button>
              <button type="button" className="sm-ghostbtn" disabled={busy} onClick={() => { setMode('idle'); setFeedback('') }}>{t('draft.cancel')}</button>
            </div>
          </div>
        )
        : null}
      {mode === 'idle'
        ? (
          <div className="sm-draft-actions">
            <Button variant="primary" size="sm" icon={<IconCheckOutline14 size={14} />} disabled={busy} onClick={() => { void pageStore.decideDraft(draft.id, { action: 'approve' }) }} data-soulmirror-draft-approve>
              {t('draft.approve')}
            </Button>
            <button type="button" className="sm-ghostbtn" disabled={busy} onClick={() => { setMode('edit') }} data-soulmirror-draft-edit-open>{t('draft.edit')}</button>
            <button type="button" className="sm-ghostbtn" disabled={busy} onClick={() => { setMode('revise') }} data-soulmirror-draft-revise-open>{t('draft.revise')}</button>
            <button type="button" className="sm-ghostbtn sm-dangerbtn" disabled={busy} onClick={() => { void pageStore.decideDraft(draft.id, { action: 'reject' }) }} data-soulmirror-draft-reject>
              <IconCloseFill14 size={12} /> {t('draft.reject')}
            </button>
          </div>
        )
        : null}
    </div>
  )
}
