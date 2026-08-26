/**
 * Right pane for the pinned first item "My alter" (P4): the ONLY place the
 * owner talks. Header (alter avatar, name, live status, "Open in dsh"), a
 * pending bar when drafts wait, the transcript of the alter session rendered
 * by us from `session.history` (owner messages right, the alter's notes left,
 * relayed friend mail as compact cards, what the alter sent / queued as send
 * lines, the plugin's draft-decision notes, failed turns), the pending draft
 * cards, and the composer → `alter.instruct` (an owner user/message + a woken
 * turn). The native dsh session holds the same log ("Open in dsh").
 */
import { useCallback, useEffect, useLayoutEffect, useRef, useState, useSyncExternalStore } from 'react'
import { Button, IconSendOutline16 } from '@deepseek-ai/dsh-client-ui-primitives'
import type { PropsRenderSlots } from '@deepseek-ai/dsh-client-ui-slots'
import type { SettingsScope } from '@deepseek-ai/dsh-client-runtime/client'
import { networkStore, type ApiChatItem } from './api.ts'
import type { AlterCardOwnerProps } from './alter-card.ts'
import { ContentTabs } from './ContentTabs.tsx'
import { DraftCard } from './DraftCard.tsx'
import type { SoulmirrorSettingsValues } from './SettingsSection.tsx'
import type { Translate } from './translate.ts'
import { formatClock, formatDay, tabsFor, type PaneTab } from './page-state.ts'
import { pageStore } from './page-store.ts'
import { SoulMirrorIcon } from './SidebarEntry.tsx'

export interface AlterPaneProps {
  t: Translate
  /** Open the alter's dsh session (closes the page). */
  onOpenSession: (sessionId: string) => void
  /** Jump to a friend's read-only thread. */
  onGoFriend: (fp: string) => void
  /** The page's `alter.card` render authorization, handed down as plain props. */
  renderCards: PropsRenderSlots<'alter.card'>['renderSlot']
  /** Live `soulmirror` settings scope for the cards. */
  scope: SettingsScope<SoulmirrorSettingsValues>
}

/** How close to the bottom (px) counts as "following" — new items auto-scroll only then. */
const FOLLOW_SLACK = 48

function nameOf(friends: readonly { fp: string; name: string }[], fp: string, fallback?: string): string {
  return friends.find(f => f.fp === fp)?.name ?? fallback ?? (fp.length > 10 ? `${fp.slice(0, 10)}…` : fp)
}

function dayOf(ts: number): string {
  const d = new Date(ts)
  return `${d.getFullYear()}-${d.getMonth() + 1}-${d.getDate()}`
}

export function AlterPane({ t, onOpenSession, onGoFriend, renderCards, scope }: AlterPaneProps) {
  const page = useSyncExternalStore(pageStore.subscribe, pageStore.getSnapshot)
  const net = useSyncExternalStore(networkStore.subscribe, networkStore.getSnapshot)
  const alter = page.alter
  const friends = net.inbox.friends
  const drafts = net.inbox.drafts
  const running = alter.status === 'running' || alter.chat.running || alter.instructing
  const [draft, setDraft] = useState('')
  const scroller = useRef<HTMLDivElement>(null)
  const textarea = useRef<HTMLTextAreaElement>(null)
  const following = useRef(true)

  useEffect(() => {
    if (!alter.loaded && !alter.loading) void pageStore.loadAlter()
    textarea.current?.focus()
  }, [])

  const items = alter.chat.items
  const firstPaint = useRef(true)
  useLayoutEffect(() => {
    const el = scroller.current
    if (el === null) return
    if (firstPaint.current && items.length > 0) {
      // Open the transcript from the TOP the first time there is content (read
      // the conversation from its start), then only follow new messages once the
      // user is at the bottom — the same shape as a dsh thread. Waiting for the
      // first non-empty items matters: on mount the transcript is still loading,
      // and scrolling then would be a no-op (the later load would snap down).
      firstPaint.current = false
      el.scrollTop = 0
      return
    }
    if (following.current) el.scrollTop = el.scrollHeight
  }, [items, drafts.length, running])

  const onScroll = (): void => {
    const el = scroller.current
    if (el === null) return
    following.current = el.scrollHeight - el.scrollTop - el.clientHeight < FOLLOW_SLACK
  }

  const submit = useCallback((text: string): void => {
    if (text.trim() === '') return
    following.current = true
    setDraft('')
    const el = textarea.current
    if (el !== null) el.style.height = 'auto'
    void pageStore.instruct(text).finally(() => { textarea.current?.focus() })
  }, [])

  const autosize = (): void => {
    const el = textarea.current
    if (el === null) return
    el.style.height = 'auto'
    el.style.height = `${Math.min(160, el.scrollHeight)}px`
  }

  // Rows with day separators; "proactive" = the alter spoke after friend mail, not after the owner.
  const rows: { key: string; node: JSX.Element }[] = []
  let lastDay: string | undefined
  let lastTrigger: 'owner' | 'inbound' | undefined
  for (const item of items) {
    const day = dayOf(item.ts)
    if (day !== lastDay) {
      rows.push({ key: `day:${day}`, node: <div className="sm-day">{formatDay(item.ts, Date.now(), { today: t('page.thread.today'), yesterday: t('page.thread.yesterday') })}</div> })
      lastDay = day
    }
    rows.push({ key: item.key, node: renderItem(item, lastTrigger) })
    if (item.kind === 'owner') lastTrigger = 'owner'
    else if (item.kind === 'inbound') lastTrigger = 'inbound'
  }

  function renderItem(item: ApiChatItem, trigger: 'owner' | 'inbound' | undefined): JSX.Element {
    switch (item.kind) {
      case 'owner':
        return (
          <div className="sm-citem sm-owner" data-soulmirror-alter-item="owner" data-soulmirror-alter-revise={item.revise?.fp}>
            <div className="sm-cmeta">
              {item.revise !== undefined ? <span className="sm-proactive">{t('alter.item.revise', { name: nameOf(friends, item.revise.fp, item.revise.name) })}</span> : null}
              <span>{formatClock(item.ts)}</span>
            </div>
            <div className="sm-obubble">{item.text}</div>
          </div>
        )
      case 'alter':
        return (
          <div className="sm-citem sm-alter" data-soulmirror-alter-item="alter">
            <div className="sm-cmeta">
              <span className="sm-avatar sm-avatar-alter sm-avatar-sm" aria-hidden><SoulMirrorIcon size={13} /></span>
              <span>{formatClock(item.ts)}</span>
              {trigger === 'inbound' ? <span className="sm-proactive">{t('alter.proactive')}</span> : null}
            </div>
            <div className="sm-abubble">{item.text}</div>
          </div>
        )
      case 'inbound': {
        const name = nameOf(friends, item.fp, item.name)
        return (
          <div className="sm-citem sm-wide" data-soulmirror-alter-item="inbound">
            <div className="sm-inmail" role="button" tabIndex={0} onClick={() => { onGoFriend(item.fp) }} onKeyDown={(e) => { if (e.key === 'Enter') onGoFriend(item.fp) }}>
              <div className="sm-inmail-head">
                <span>{t('alter.item.inbound', { name: '' })}</span><b>{name}</b>
                <span>· {formatClock(item.ts)}</span>
                {item.auto ? <span>· {t('bubble.auto')}</span> : null}
                <span style={{ marginLeft: 'auto' }} className="sm-linkbtn">{t('alter.item.view')}</span>
              </div>
              <div className="sm-inmail-body">{item.body}</div>
            </div>
          </div>
        )
      }
      case 'send': {
        const name = nameOf(friends, item.fp)
        const state = item.outcome === undefined
          ? <span className="sm-statepill">{t('alter.item.send.pending')}</span>
          : item.outcome === 'sent'
            ? <span className="sm-statepill sm-ok">{item.auto ? t('alter.item.send.auto') : t('alter.item.send.sent')}</span>
            : item.outcome === 'draft-queued'
              ? <span className="sm-statepill sm-warn">{t('alter.item.send.draft')}</span>
              : <span className="sm-statepill sm-err">{item.outcome === 'failed' ? t('alter.item.send.failed') : t('alter.item.send.refused')}{item.detail === undefined ? '' : ` · ${item.detail}`}</span>
        return (
          <div className="sm-citem sm-wide" data-soulmirror-alter-item="send" data-soulmirror-alter-send={item.outcome ?? 'pending'}>
            <div className="sm-sendline">
              <div className="sm-sendline-head">
                <span>{t('alter.item.send', { name: '' })}</span><b>{name}</b>
                <span>· {formatClock(item.ts)}</span>
                {state}
                <button type="button" className="sm-linkbtn" style={{ marginLeft: 'auto' }} onClick={() => { onGoFriend(item.fp) }}>{t('alter.item.view')}</button>
              </div>
              <div className="sm-sendline-body">{item.body}</div>
            </div>
          </div>
        )
      }
      case 'note': {
        const name = nameOf(friends, item.fp)
        const text = item.note === 'draft-approved' ? t('alter.item.note.approved', { name }) : item.note === 'draft-rejected' ? t('alter.item.note.rejected', { name }) : t('alter.item.note.revise', { name })
        return <div className="sm-citem sm-center" data-soulmirror-alter-item="note"><div className="sm-noteline">{text} · {formatClock(item.ts)}</div></div>
      }
      case 'turn-failed':
        return (
          <div className="sm-citem sm-center" data-soulmirror-alter-item="turn-failed">
            <span className="sm-statepill sm-err">{t('alter.item.turnFailed', { reason: item.message === undefined ? item.reason : `${item.reason} — ${item.message}` })}</span>
            <span className="sm-noteline">{t('alter.failed.hint')}</span>
          </div>
        )
      case 'thinking':
        return (
          <div className="sm-citem sm-wide" data-soulmirror-alter-item="thinking" data-soulmirror-thinking={item.streaming ? 'streaming' : 'done'}>
            <div style={{ fontSize: 12, opacity: 0.6, fontStyle: 'italic', whiteSpace: 'pre-wrap', borderLeft: '2px solid rgba(127,127,127,.4)', padding: '2px 10px', margin: '2px 0 2px 12px', maxWidth: '100%' }}>
              💭 {item.text}
            </div>
          </div>
        )
      case 'tool':
        return (
          <div className="sm-citem sm-wide" data-soulmirror-alter-item="tool">
            <div style={{ fontSize: 12, opacity: 0.7, fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace', padding: '1px 4px', wordBreak: 'break-all' }}>
              🔧 {item.name} <span style={{ opacity: 0.7 }}>{item.args}</span>
            </div>
          </div>
        )
      default:
        return <></>
    }
  }

  const firstDraft = drafts[0]

  const paneTab: PaneTab = page.paneTab
  const tabs = tabsFor('alter', false)
  const alterHome = (
    <div className="sm-home" data-soulmirror-alter-home>
      <div className="sm-home-inner">
        <div className="sm-home-id">
          <span className="sm-avatar sm-avatar-alter sm-avatar-lg" aria-hidden><SoulMirrorIcon size={18} /></span>
          <div style={{ display: 'grid', gap: 2, minWidth: 0 }}>
            <span className="sm-home-id-name">{t('alter.me')}</span>
            <span className="sm-home-id-sub">
              <span className={`sm-livedot${running ? ' sm-busy' : ''}`} aria-hidden />
              <span>{alter.sessionId === undefined ? t('alter.status.noSession') : running ? t('alter.status.running') : t('alter.status.idle')}</span>
            </span>
          </div>
        </div>
        <div className="sm-home-card">
          <div className="sm-home-title"><span>{t('alter.home.profile')}</span></div>
          <div className="sm-home-line"><span className="sm-home-line-key">{t('alter.home.session')}</span><span className="sm-home-line-val">{alter.sessionId ?? t('alter.home.none')}</span></div>
          <div className="sm-home-line"><span className="sm-home-line-key">{t('alter.home.friends')}</span><span className="sm-home-line-val">{friends.length}</span></div>
          <div className="sm-home-line"><span className="sm-home-line-key">{t('alter.home.drafts')}</span><span className="sm-home-line-val">{drafts.length}</span></div>
          <div className="sm-home-line"><span className="sm-home-line-key">{t('alter.home.defaultTier')}</span><span className="sm-home-line-val">{t(`tier.short.${net.state?.alter?.defaultTier ?? 'draft'}`)}</span></div>
          <div className="sm-home-line"><span className="sm-home-line-key">{t('alter.home.perHour')}</span><span className="sm-home-line-val">{net.state?.alter?.autoReplyPerHour ?? 20}</span></div>
        </div>
      </div>
    </div>
  )

  // The "settings" tab is the card surface: every registered `alter.card` renders here.
  const cardProps: AlterCardOwnerProps = {
    alter: { sessionId: alter.sessionId, status: alter.status },
    scope,
    openSession: () => { if (alter.sessionId !== undefined) onOpenSession(alter.sessionId) },
  }
  const alterSettings = (
    <div className="sm-home" data-soulmirror-alter-settings>
      <div className="sm-home-inner">
        {renderCards('alter.card', cardProps)}
      </div>
    </div>
  )

  return (
    <section className="sm-chat-col" data-soulmirror-page-chat="alter" data-soulmirror-alter-status={running ? 'running' : 'idle'} style={{ position: 'relative' }}>
      <header className="sm-chat-head">
        <span className="sm-avatar sm-avatar-alter sm-avatar-lg" aria-hidden><SoulMirrorIcon size={18} /></span>
        <div style={{ flex: 1, minWidth: 0, display: 'grid' }}>
          <div className="sm-chat-head-name">{t('alter.me')}</div>
          <div className="sm-chat-head-sub">
            <span className={`sm-livedot${running ? ' sm-busy' : ''}`} aria-hidden />
            {alter.sessionId === undefined ? t('alter.status.noSession') : running ? t('alter.status.running') : t('alter.status.idle')}
          </div>
        </div>
      </header>
      <ContentTabs tabs={tabs} active={paneTab} onChange={pageStore.setPaneTab} t={t} />
      {paneTab === 'settings' ? alterSettings : paneTab === 'home' ? alterHome : <>
      {drafts.length > 0 && firstDraft !== undefined
        ? (
          <div className="sm-pendbar" data-soulmirror-alter-pendbar={drafts.length}>
            <span>{t('alter.pendbar', { n: drafts.length })}</span>
            <button type="button" className="sm-linkbtn" onClick={() => { onGoFriend(firstDraft.fp) }}>{t('alter.pendbar.go', { name: firstDraft.name })}</button>
          </div>
        )
        : null}
      <div ref={scroller} className="sm-thread" onScroll={onScroll} data-soulmirror-alter-thread>
        <div className="sm-thread-inner">
          {!alter.loaded && alter.loading ? <span className="sm-muted" style={{ alignSelf: 'center', fontSize: 12 }}>{t('page.thread.loading')}</span> : null}
          {alter.error !== undefined ? <span style={{ alignSelf: 'center', fontSize: 12, color: 'var(--dsw-alias-state-error-primary)' }}>{t('settings.error', { message: alter.error })}</span> : null}
          {alter.loaded && items.length === 0 && drafts.length === 0
            ? (
              <div className="sm-empty" data-soulmirror-alter-empty>
                <SoulMirrorIcon size={32} />
                <div className="sm-empty-title">{t('alter.empty.title')}</div>
                <p>{t('alter.empty.hint')}</p>
              </div>
            )
            : null}
          {rows.map(row => <div key={row.key} style={{ display: 'contents' }}>{row.node}</div>)}
          {drafts.map(d => <DraftCard key={d.id} draft={d} t={t} showTarget onGoFriend={onGoFriend} />)}
          {running
            ? (
              <div className="sm-citem sm-alter" data-soulmirror-alter-running>
                <div className="sm-typing" style={{ padding: '2px 0' }}><span className="sm-typing-dots"><span /><span /><span /></span>{t('alter.thinking')}</div>
              </div>
            )
            : null}
        </div>
      </div>
      <div className="sm-composer">
        <div className="sm-composer-box">
          <textarea
            ref={textarea}
            className="sm-textarea"
            rows={1}
            value={draft}
            placeholder={t('alter.composer.placeholder')}
            onChange={(e) => { setDraft(e.target.value); autosize() }}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                e.preventDefault()
                submit(draft)
              }
            }}
            data-soulmirror-page-composer
          />
          <Button variant="primary" size="sm" icon={<IconSendOutline16 size={14} />} disabled={draft.trim() === '' || alter.instructing} onClick={() => { submit(draft) }} data-soulmirror-page-send>
            {alter.instructing ? t('page.composer.instructing') : t('page.composer.send')}
          </Button>
        </div>
        <span className="sm-composer-hint">{t('alter.composer.hint')}</span>
      </div>
      </>}
    </section>
  )
}
