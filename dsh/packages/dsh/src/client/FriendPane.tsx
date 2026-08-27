/**
 * Right pane for a FRIEND item (P4): the read-only A2A thread between the
 * owner's alter and that friend's alter — header (name, presence, Card, Mark
 * read), a banner saying the owner cannot speak here, the bubbles (in = the
 * friend, out = my alter; delivery state; day separators; load older on
 * scroll-up), the pending DRAFT CARDS for this friend (approve / edit /
 * revise / reject), the typing indicator (SSE), an optional friend-settings
 * panel (reply tier + protocol override) and the bottom ACTION BAR instead of
 * a composer: drafts waiting · Card · Protocol & settings · "Talk to My
 * alter". With the debug setting "send as myself" on, a small direct-send row
 * appears above the action bar (bypasses the alter; P2b behaviour).
 */
import { useEffect, useLayoutEffect, useRef, useState, useSyncExternalStore } from 'react'
import { Button, IconCheckOutline14, IconCopyOutline16, IconSendOutline16, IconUserOutline16, Tooltip, writeClipboard } from '@deepseek-ai/dsh-client-ui-primitives'
import { FriendSettingsPanel } from './alter-ui.tsx'
import { api, networkStore } from './api.ts'
import { ContentTabs } from './ContentTabs.tsx'
import { DraftCard } from './DraftCard.tsx'
import { MemoryPane } from './MemoryPane.tsx'
import type { Translate } from './translate.ts'
import { draftsFor, formatAge, type InboxFriend } from './inbox-state.ts'
import { formatClock, formatDay, tabsFor, type PaneTab, withDaySeparators, type ThreadEntry, type ThreadState } from './page-state.ts'
import { pageStore } from './page-store.ts'

export interface FriendPaneProps {
  t: Translate
  friend: InboxFriend
  /** Whether the page is visible (mark-read only then). */
  visible: boolean
  /** Jump to "My alter". */
  onGoAlter: () => void
  /** Debug setting: offer the direct-send row (bypasses the alter). */
  directSend: boolean
}

/** How close to the bottom (px) counts as "following" — new bubbles auto-scroll only then. */
const FOLLOW_SLACK = 48

function statusLabel(t: Translate, entry: ThreadEntry): { text: string; failed: boolean } {
  switch (entry.status) {
    case 'sending': return { text: t('page.status.sending'), failed: false }
    case 'queued': return { text: t('page.status.queued'), failed: false }
    case 'error': return { text: t('page.status.error'), failed: true }
    case 'failed': return { text: t('page.status.failed', { message: entry.error ?? '' }), failed: true }
    case 'sent':
    default: return { text: t('page.status.sent'), failed: false }
  }
}

function Bubble({ entry, t, fp, onRetry }: { entry: ThreadEntry; t: Translate; fp: string; onRetry: (body: string) => void }) {
  const out = entry.dir === 'out'
  const status = out ? statusLabel(t, entry) : undefined
  const pending = entry.status === 'sending'
  const failed = entry.status === 'failed' || entry.status === 'error'
  return (
    <div className={`sm-msg ${out ? 'sm-out' : 'sm-in'}`} data-soulmirror-bubble={out ? 'out' : 'in'} data-soulmirror-seq={entry.seq > 0 ? entry.seq : undefined}>
      <div className={`sm-bubble${pending ? ' sm-pending' : ''}${failed ? ' sm-failed' : ''}`}>
        {entry.body}
        {entry.artifactName !== undefined ? <div style={{ fontSize: 11, opacity: 0.8, marginTop: 4 }}>📎 {t('page.thread.attachment', { name: entry.artifactName })}</div> : null}
      </div>
      <div className="sm-msg-meta">
        {out ? <span>{t('bubble.mine')}</span> : null}
        {entry.auto === true ? <span>{t('bubble.auto')}</span> : null}
        <span>{formatClock(entry.ts)}</span>
        {status !== undefined ? <span className={status.failed ? 'sm-status-failed' : undefined} data-soulmirror-status={entry.status ?? 'sent'}>{status.text}</span> : null}
        {entry.status === 'failed' && entry.clientId !== undefined
          ? (
            <>
              <button type="button" className="sm-linkbtn" onClick={() => { const body = pageStore.discard(fp, entry.clientId!); if (body !== undefined) onRetry(body) }}>{t('page.status.retry')}</button>
              <button type="button" className="sm-linkbtn" onClick={() => { pageStore.discard(fp, entry.clientId!) }}>{t('page.status.discard')}</button>
            </>
          )
          : null}
      </div>
    </div>
  )
}

export function FriendPane({ t, friend, visible, onGoAlter, directSend }: FriendPaneProps) {
  const fp = friend.fp
  const page = useSyncExternalStore(pageStore.subscribe, pageStore.getSnapshot)
  const net = useSyncExternalStore(networkStore.subscribe, networkStore.getSnapshot)
  const thread: ThreadState = page.threads[fp] ?? { entries: [], window: 0, complete: false, loading: false, loaded: false }
  const drafts = draftsFor(net.inbox, fp)
  const alterConfig = net.state?.alter
  const typing = net.inbox.typing[fp] === true
  const [cardOpen, setCardOpen] = useState(false)
  const [settingsOpen, setSettingsOpen] = useState(false)
  const [copied, setCopied] = useState(false)
  const [debugText, setDebugText] = useState('')
  const scroller = useRef<HTMLDivElement>(null)
  const draftsAnchor = useRef<HTMLDivElement>(null)
  const following = useRef(true)
  /** Last scroll offset, kept across tab switches (the scroller unmounts off "chat"). */
  const savedTop = useRef(0)
  const prevFirstSeq = useRef<number | undefined>(undefined)
  const prevHeight = useRef(0)

  // Switching friends: reset, fetch the archive.
  useEffect(() => {
    setCardOpen(false)
    setSettingsOpen(false)
    setDebugText('')
    following.current = true
    if (!(pageStore.thread(fp).loaded)) void pageStore.load(fp)
  }, [fp])

  // Escape closes the card popover (the page listens too and yields while it is open).
  useEffect(() => {
    if (!cardOpen) return
    const onKey = (e: KeyboardEvent): void => { if (e.key === 'Escape') setCardOpen(false) }
    document.addEventListener('keydown', onKey)
    return () => { document.removeEventListener('keydown', onKey) }
  }, [cardOpen])

  // Refresh presence for the open friend (cached 10 s by the peer).
  useEffect(() => {
    let cancelled = false
    api.presence([fp]).then(({ online }) => { if (!cancelled) networkStore.applyPresence(online) }).catch(() => {})
    return () => { cancelled = true }
  }, [fp])

  // Open + visible = read: on selection and as mail lands.
  const unread = friend.unread
  useEffect(() => {
    if (!visible) return
    if (typeof document !== 'undefined' && document.visibilityState !== 'visible') return
    if (unread === 0) return
    void networkStore.markRead(fp)
  }, [fp, unread, visible, thread.entries.length])
  useEffect(() => {
    if (!visible || typeof document === 'undefined') return
    const onVisibility = (): void => { if (document.visibilityState === 'visible') void networkStore.markRead(fp) }
    document.addEventListener('visibilitychange', onVisibility)
    return () => { document.removeEventListener('visibilitychange', onVisibility) }
  }, [fp, visible])

  // Auto-scroll: stick to the bottom while following; keep the viewport
  // anchored when older entries are prepended.
  const entries = thread.entries
  const firstSeq = entries.find(e => e.seq > 0)?.seq
  useLayoutEffect(() => {
    const el = scroller.current
    if (el === null) return
    if (prevFirstSeq.current !== undefined && firstSeq !== undefined && firstSeq < prevFirstSeq.current) {
      el.scrollTop += el.scrollHeight - prevHeight.current
    } else if (following.current) {
      el.scrollTop = el.scrollHeight
    }
    prevFirstSeq.current = firstSeq
    prevHeight.current = el.scrollHeight
  }, [entries, typing, firstSeq, fp, drafts.length])

  const onScroll = (): void => {
    const el = scroller.current
    if (el === null) return
    following.current = el.scrollHeight - el.scrollTop - el.clientHeight < FOLLOW_SLACK
    savedTop.current = el.scrollTop
    if (el.scrollTop < 24 && !thread.complete && !thread.loading && thread.loaded) void pageStore.loadOlder(fp)
  }

  const age = friend.lastTs === undefined ? '' : formatAge(friend.lastTs)
  const presence = friend.online === true
    ? t('page.header.online')
    : age === '' ? t('page.header.offline') : `${t('page.header.offline')} · ${age === 'now' ? t('page.header.lastSeen.now') : t('page.header.lastSeen', { age })}`
  const rows = withDaySeparators(entries)
  const note = friend.remark !== undefined && friend.remark !== friend.name ? friend.remark : undefined

  const sendDebug = (): void => {
    const text = debugText.trim()
    if (text === '') return
    setDebugText('')
    following.current = true
    void pageStore.send(fp, text)
  }

  /**
   * The scroller UNMOUNTS while the "home" tab is up, so returning to "chat"
   * would otherwise paint a fresh element at scrollTop 0 (the oldest message):
   * the data-keyed effect above does not re-run on a tab switch. Put the reader
   * back where they were — or at the bottom if they were following the tail.
   */
  useLayoutEffect(() => {
    if (page.paneTab !== 'chat') return
    const el = scroller.current
    if (el === null) return
    el.scrollTop = following.current ? el.scrollHeight : savedTop.current
  }, [page.paneTab])

  const paneTab: PaneTab = page.paneTab
  const tabs = tabsFor('friend', false)

  // Private home (a friend's own page): identity card + quick actions.
  const friendHome = (
    <div className="sm-home" data-soulmirror-friend-home>
      <div className="sm-home-inner">
        <div className="sm-home-id">
          <span className="sm-avatar sm-avatar-lg" aria-hidden>{friend.name.slice(0, 1)}</span>
          <div style={{ display: 'grid', gap: 2, minWidth: 0 }}>
            <span className="sm-home-id-name">{friend.name}</span>
            <span className="sm-home-id-sub">
              <span className={`sm-presence${friend.online === true ? ' sm-online' : ''}`} />
              <span>{friend.online === true ? t('page.header.online') : t('page.header.offline')}</span>
              {friend.tier !== undefined && friend.tier !== 'draft' ? <span> · {t(`tier.short.${friend.tier}`)}</span> : null}
            </span>
          </div>
        </div>
        <div className="sm-home-card">
          <div className="sm-home-title"><span>{t('friend.home.profile')}</span></div>
          <div className="sm-home-line"><span className="sm-home-line-key">{t('friend.home.fp')}</span><span className="sm-home-line-val" data-soulmirror-friend-fp>{fp}</span></div>
          <div className="sm-home-line"><span className="sm-home-line-key">{t('friend.home.name')}</span><span className="sm-home-line-val">{friend.cardName ?? friend.name}</span></div>
          {note !== undefined ? <div className="sm-home-line"><span className="sm-home-line-key">{t('friend.home.remark')}</span><span className="sm-home-line-val">{note}</span></div> : null}
          {friend.protocol !== undefined && friend.protocol !== '' ? <div className="sm-home-line"><span className="sm-home-line-key">{t('friend.home.protocol')}</span><span className="sm-home-line-val">{friend.protocol}</span></div> : null}
          {age !== '' ? <div className="sm-home-line"><span className="sm-home-line-key">{t('friend.home.lastActive')}</span><span className="sm-home-line-val">{age}</span></div> : null}
          {drafts.length > 0 ? <div className="sm-home-line"><span className="sm-home-line-key">{t('friend.home.drafts')}</span><span className="sm-home-line-val sm-alert">{drafts.length}</span></div> : null}
        </div>
        <div className="sm-home-card">
          <div className="sm-home-title"><span>{t('friend.home.actions')}</span></div>
          <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
            <button type="button" className="sm-ghostbtn" onClick={() => { setCardOpen(v => !v) }}><IconUserOutline16 size={14} /> {t('page.header.card')}</button>
            <button type="button" className="sm-ghostbtn" onClick={() => { pageStore.setPaneTab('chat'); setCardOpen(true) }}><IconCopyOutline16 size={14} /> {t('page.header.card.copy')}</button>
            <button type="button" className="sm-ghostbtn" aria-expanded={settingsOpen} onClick={() => { setSettingsOpen(v => !v) }}>{t('friend.actbar.settings')}</button>
            <Button variant="primary" size="sm" onClick={onGoAlter}>{t('friend.home.sendMessage')}</Button>
          </div>
        </div>
        {settingsOpen
          ? (
            <FriendSettingsPanel
              fp={fp}
              name={friend.name}
              tier={friend.tier}
              tierExplicit={friend.tierExplicit === true}
              protocol={friend.protocol}
              muted={friend.muted === true}
              defaultTier={alterConfig?.defaultTier ?? 'draft'}
              perHour={alterConfig?.autoReplyPerHour ?? 20}
              t={t}
              onClose={() => { setSettingsOpen(false) }}
            />
          )
          : null}
      </div>
    </div>
  )

  return (
    <section className="sm-chat-col" data-soulmirror-page-chat={fp} data-soulmirror-readonly style={{ position: 'relative' }}>
      <header className="sm-chat-head">
        <span className="sm-avawrap">
          <span className="sm-avatar sm-avatar-lg" aria-hidden>{friend.name.slice(0, 1)}</span>
          <span className={`sm-presence${friend.online === true ? ' sm-online' : ''}`} />
        </span>
        <div style={{ flex: 1, minWidth: 0, display: 'grid' }}>
          <div className="sm-chat-head-name">{friend.name}</div>
          <div className="sm-chat-head-sub" data-soulmirror-presence={friend.online === true ? 'online' : 'offline'}>
            {typing ? <span style={{ color: 'var(--dsw-alias-brand-primary)' }}>{t('page.thread.typing', { name: friend.name })}</span> : presence}
            {note !== undefined ? <span>· {note}</span> : null}
            {friend.tier !== undefined && friend.tier !== 'draft' ? <span>· {t(`tier.short.${friend.tier}`)}</span> : null}
          </div>
        </div>
        <div className="sm-chat-head-actions">
          <button type="button" className="sm-ghostbtn" onClick={() => { setCardOpen(v => !v) }} aria-expanded={cardOpen} data-soulmirror-page-card>
            <IconUserOutline16 size={14} /> {t('page.header.card')}
          </button>
        </div>
      </header>
      <ContentTabs tabs={tabs} active={paneTab} onChange={pageStore.setPaneTab} t={t} />
      {paneTab === 'home' ? friendHome : paneTab === 'memory' ? <MemoryPane t={t} allow={{ global: true, friend: fp }} scope={{ kind: 'shared-friend', fp }} /> : <>
      <div className="sm-banner" role="note" data-soulmirror-readonly-banner>
        <span>{t('friend.banner.prefix')}<b>{t('friend.banner.mine')}</b>{t('friend.banner.middle')}<b>{t('friend.banner.theirs', { name: friend.name })}</b>{t('friend.banner.suffix')}</span>
        <button type="button" className="sm-ghostbtn" onClick={onGoAlter} data-soulmirror-banner-go>{t('friend.banner.go')}</button>
      </div>
      {cardOpen
        ? (
          <div className="sm-card-pop" role="dialog" aria-label={t('page.header.card.title', { name: friend.name })} data-soulmirror-page-card-pop>
            <strong>{t('page.header.card.title', { name: friend.name })}</strong>
            <span className="sm-muted">{friend.cardName ?? friend.name}{note !== undefined ? ` · ${note}` : ''}</span>
            <span className="sm-card-uri" data-soulmirror-page-card-fp>{fp}</span>
            <div style={{ display: 'flex', gap: 6, justifyContent: 'flex-end' }}>
              <button
                type="button"
                className="sm-ghostbtn"
                onClick={() => {
                  void writeClipboard(fp).then((ok) => {
                    if (!ok) return
                    setCopied(true)
                    setTimeout(() => { setCopied(false) }, 1500)
                  })
                }}
              >
                {copied ? <IconCheckOutline14 size={14} /> : <IconCopyOutline16 size={14} />} {copied ? t('page.header.card.copied') : t('page.header.card.copy')}
              </button>
              <button type="button" className="sm-ghostbtn" onClick={() => { setCardOpen(false) }}>{t('inbox.close')}</button>
            </div>
          </div>
        )
        : null}
      <div ref={scroller} className="sm-thread" onScroll={onScroll} data-soulmirror-page-thread>
        <div className="sm-thread-inner">
          {thread.loaded && !thread.complete
            ? (
              <button type="button" className="sm-linkbtn sm-muted" style={{ alignSelf: 'center', fontSize: 11 }} disabled={thread.loading} onClick={() => { void pageStore.loadOlder(fp) }}>
                {thread.loading ? t('page.thread.loading') : t('page.thread.loadOlder')}
              </button>
            )
            : thread.loaded && entries.length > 0
              ? <span className="sm-muted" style={{ alignSelf: 'center', fontSize: 11 }}>{t('page.thread.start')}</span>
              : null}
          {!thread.loaded && thread.loading ? <span className="sm-muted" style={{ alignSelf: 'center', fontSize: 12 }}>{t('page.thread.loading')}</span> : null}
          {thread.error !== undefined ? <span style={{ alignSelf: 'center', fontSize: 12, color: 'var(--dsw-alias-state-error-primary)' }}>{t('settings.error', { message: thread.error })}</span> : null}
          {thread.loaded && entries.length === 0 && thread.error === undefined && drafts.length === 0
            ? <span className="sm-muted" style={{ alignSelf: 'center', fontSize: 12, padding: '24px 0', textAlign: 'center' }} data-soulmirror-page-thread-empty>{t('friend.empty', { name: friend.name })}</span>
            : null}
          {rows.map(row => row.kind === 'day'
            ? <div key={row.key} className="sm-day">{formatDay(row.ts, Date.now(), { today: t('page.thread.today'), yesterday: t('page.thread.yesterday') })}</div>
            : <Bubble key={row.key} entry={row.entry} t={t} fp={fp} onRetry={(body) => { setDebugText(body) }} />)}
          {drafts.length > 0
            ? (
              <div ref={draftsAnchor} data-soulmirror-page-drafts={drafts.length} style={{ display: 'contents' }}>
                <div className="sm-citem sm-center"><span className="sm-statepill sm-warn">{t('friend.drafts.pill', { n: drafts.length })}</span></div>
                {drafts.map(d => <DraftCard key={d.id} draft={d} t={t} />)}
              </div>
            )
            : null}
        </div>
      </div>
      <div className="sm-typing" aria-live="polite" data-soulmirror-page-typing={typing ? 'on' : 'off'}>
        {typing ? <><span className="sm-typing-dots"><span /><span /><span /></span>{t('page.thread.typing', { name: friend.name })}</> : null}
      </div>
      {settingsOpen
        ? (
          <FriendSettingsPanel
            fp={fp}
            name={friend.name}
            tier={friend.tier}
            tierExplicit={friend.tierExplicit === true}
            protocol={friend.protocol}
            muted={friend.muted === true}
            defaultTier={alterConfig?.defaultTier ?? 'draft'}
            perHour={alterConfig?.autoReplyPerHour ?? 20}
            t={t}
            onClose={() => { setSettingsOpen(false) }}
          />
        )
        : null}
      {directSend
        ? (
          <div className="sm-composer" data-soulmirror-debug-send style={{ paddingBottom: 6 }}>
            <div className="sm-composer-box">
              <textarea
                className="sm-textarea"
                rows={1}
                value={debugText}
                placeholder={t('friend.debug.placeholder', { name: friend.name })}
                onChange={(e) => { setDebugText(e.target.value); if (e.target.value.trim() !== '') pageStore.noteTyping(fp) }}
                onBlur={() => { if (debugText.trim() === '') pageStore.stopTyping(fp) }}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                    e.preventDefault()
                    sendDebug()
                  }
                }}
                data-soulmirror-debug-composer
              />
              <Button variant="outline" size="sm" icon={<IconSendOutline16 size={14} />} disabled={debugText.trim() === ''} onClick={sendDebug} data-soulmirror-debug-send-button>
                {t('friend.debug.send')}
              </Button>
            </div>
            <span className="sm-composer-hint">{t('page.composer.direct', { name: friend.name })}</span>
          </div>
        )
        : null}
      <div className="sm-actbar" data-soulmirror-actbar>
        <span className="sm-actbar-eye">{t('friend.actbar.eye')}</span>
        {drafts.length > 0
          ? (
            <button type="button" className="sm-ghostbtn sm-warnbtn" onClick={() => { draftsAnchor.current?.scrollIntoView({ block: 'end', behavior: 'smooth' }) }} data-soulmirror-actbar-drafts={drafts.length}>
              {t('friend.actbar.pending')} <b>{drafts.length}</b>
            </button>
          )
          : null}
        <button type="button" className="sm-ghostbtn" onClick={() => { setCardOpen(v => !v) }}>
          <IconUserOutline16 size={14} /> {t('page.header.card')}
        </button>
        <Tooltip label={t('friend.settings.title', { name: friend.name })} side="top">
          <button type="button" className="sm-ghostbtn" aria-expanded={settingsOpen} onClick={() => { setSettingsOpen(v => !v) }} data-soulmirror-friend-settings-toggle>
            {t('friend.actbar.settings')}
          </button>
        </Tooltip>
        <Button variant="primary" size="sm" onClick={onGoAlter} data-soulmirror-actbar-go-alter>{t('friend.actbar.goAlter')}</Button>
      </div>
      </>}
    </section>
  )
}
