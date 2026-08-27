/**
 * Right pane for one NAMED seat agent: the owner talks to it directly here
 * (composer → `agent.instruct`), watches its transcript (`agent.history`,
 * same fold as the alter's — owner bubbles right, the agent's notes left,
 * the group messages that woke it as compact cards, its group posts as send
 * lines) and reaches its settings (the sheet) and its native dsh session
 * from the header. Live: SSE `agent` frames for this name trigger a
 * debounced refetch.
 */
import { useCallback, useEffect, useLayoutEffect, useRef, useState, useSyncExternalStore } from 'react'
import { Button, IconRightUpOutline14, IconSendOutline16, IconSettingsOutline16, Tooltip } from '@deepseek-ai/dsh-client-ui-primitives'
import { api, networkStore, type ApiChatItem, type ApiHistory, type ApiSeatAgent } from './api.ts'
import { AgentSettingsSheet } from './AgentSettingsSheet.tsx'
import { ContentTabs } from './ContentTabs.tsx'
import { MemoryPane } from './MemoryPane.tsx'
import { ProcessItemView } from './process-ui.tsx'
import { formatClock, formatDay, tabsFor, type PaneTab } from './page-state.ts'
import { pageStore } from './page-store.ts'
import type { Translate } from './translate.ts'

const FOLLOW_SLACK = 48
const HISTORY_LIMIT = 200
const REFETCH_DEBOUNCE_MS = 150

export interface AgentPaneProps {
  t: Translate
  agent: ApiSeatAgent
  /** Open the agent's dsh session (closes the page). */
  onOpenSession: (sessionId: string) => void
  /** The agent was removed in the settings sheet (the page falls back to the alter). */
  onRemoved: () => void
}

function dayOf(ts: number): string {
  const d = new Date(ts)
  return `${d.getFullYear()}-${d.getMonth() + 1}-${d.getDate()}`
}

export function AgentPane({ t, agent, onOpenSession, onRemoved }: AgentPaneProps) {
  const name = agent.name
  const page = useSyncExternalStore(pageStore.subscribe, pageStore.getSnapshot)
  const [history, setHistory] = useState<ApiHistory | undefined>(undefined)
  const [error, setError] = useState<string | undefined>(undefined)
  const [draft, setDraft] = useState('')
  const [instructing, setInstructing] = useState(false)
  const [sheetOpen, setSheetOpen] = useState(false)
  const [openThinking, setOpenThinking] = useState<ReadonlySet<string>>(new Set())
  const scroller = useRef<HTMLDivElement>(null)
  const textarea = useRef<HTMLTextAreaElement>(null)
  const following = useRef(true)
  /** Last scroll offset, kept across tab switches (the scroller unmounts off "chat"). */
  const savedTop = useRef(0)
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)

  const load = useCallback((): void => {
    api.agentHistory(name, HISTORY_LIMIT).then((h) => {
      setHistory(h)
      setError(undefined)
    }).catch((e: unknown) => { setError(e instanceof Error ? e.message : String(e)) })
  }, [name])

  // Load on open / on switching agents; live refetch on this agent's SSE frames.
  useEffect(() => {
    setHistory(undefined)
    setDraft('')
    setOpenThinking(new Set())
    following.current = true
    load()
    textarea.current?.focus()
    const off = networkStore.onFrame((frame) => {
      if (frame.kind !== 'agent' || frame.name !== name) return
      if (timer.current !== undefined) clearTimeout(timer.current)
      timer.current = setTimeout(() => { load() }, REFETCH_DEBOUNCE_MS)
    })
    return () => {
      off()
      if (timer.current !== undefined) clearTimeout(timer.current)
    }
  }, [name, load])

  const items = history?.chat.items ?? []
  const running = agent.status === 'running' || history?.status === 'running' || history?.chat.running === true || instructing

  useLayoutEffect(() => {
    const el = scroller.current
    if (el === null) return
    if (following.current) el.scrollTop = el.scrollHeight
  }, [items.length, running])

  const onScroll = (): void => {
    const el = scroller.current
    if (el === null) return
    following.current = el.scrollHeight - el.scrollTop - el.clientHeight < FOLLOW_SLACK
    savedTop.current = el.scrollTop
  }

  /**
   * The scroller UNMOUNTS while a non-chat tab is up, so returning to "chat"
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

  const submit = (text: string): void => {
    if (text.trim() === '' || instructing) return
    following.current = true
    setDraft('')
    setInstructing(true)
    void api.agentInstruct(name, text)
      .then(() => { load() })
      .catch((e: unknown) => { setError(e instanceof Error ? e.message : String(e)) })
      .finally(() => {
        setInstructing(false)
        textarea.current?.focus()
      })
  }

  const groupsById = new Map((networkStore.getSnapshot().state?.groups ?? []).map(g => [g.gid, g.name]))
  const targetName = (fp: string): string => groupsById.get(fp) ?? (fp.length > 10 ? `${fp.slice(0, 10)}…` : fp)

  function renderItem(item: ApiChatItem): JSX.Element {
    switch (item.kind) {
      case 'owner':
        return (
          <div className="sm-citem sm-owner" data-soulmirror-agent-item="owner">
            <div className="sm-cmeta"><span>{formatClock(item.ts)}</span></div>
            <div className="sm-obubble">{item.text}</div>
          </div>
        )
      case 'alter': // the agent's own words to the owner (same event shape as the alter's)
        return (
          <div className="sm-citem sm-alter" data-soulmirror-agent-item="agent">
            <div className="sm-cmeta">
              <span className="sm-avatar sm-avatar-sm" aria-hidden>{name.slice(0, 1)}</span>
              <span>{formatClock(item.ts)}</span>
            </div>
            <div className="sm-abubble">{item.text}</div>
          </div>
        )
      case 'inbound':
        return (
          <div className="sm-citem sm-wide" data-soulmirror-agent-item="inbound">
            <div className="sm-inmail">
              <div className="sm-inmail-head">
                <span>{t('agent.woke')}</span><b>{item.name}</b>
                <span>· {formatClock(item.ts)}</span>
              </div>
              <div className="sm-inmail-body">{item.body}</div>
            </div>
          </div>
        )
      case 'send': {
        const state = item.outcome === undefined
          ? <span className="sm-statepill">{t('alter.item.send.pending')}</span>
          : item.outcome === 'sent'
            ? <span className="sm-statepill sm-ok">{item.auto ? t('alter.item.send.auto') : t('alter.item.send.sent')}</span>
            : item.outcome === 'draft-queued'
              ? <span className="sm-statepill sm-warn">{t('alter.item.send.draft')}</span>
              : <span className="sm-statepill sm-err">{item.outcome === 'failed' ? t('alter.item.send.failed') : t('alter.item.send.refused')}{item.detail === undefined ? '' : ` · ${item.detail}`}</span>
        return (
          <div className="sm-citem sm-wide" data-soulmirror-agent-item="send">
            <div className="sm-sendline">
              <div className="sm-sendline-head">
                <span>{t('alter.item.send', { name: '' })}</span><b>{targetName(item.fp)}</b>
                <span>· {formatClock(item.ts)}</span>
                {state}
              </div>
              <div className="sm-sendline-body">{item.body}</div>
            </div>
          </div>
        )
      }
      case 'turn-failed':
        return (
          <div className="sm-citem sm-center" data-soulmirror-agent-item="turn-failed">
            <span className="sm-statepill sm-err">{t('alter.item.turnFailed', { reason: item.message === undefined ? item.reason : `${item.reason} — ${item.message}` })}</span>
          </div>
        )
      case 'thinking':
      case 'tool':
        return (
          <ProcessItemView
            item={item}
            t={t}
            open={openThinking.has(item.key)}
            onToggle={(key) => {
              setOpenThinking((prev) => {
                const next = new Set(prev)
                if (next.has(key)) next.delete(key)
                else next.add(key)
                return next
              })
            }}
          />
        )
      default:
        return <></>
    }
  }

  const rows: { key: string; node: JSX.Element }[] = []
  let lastDay: string | undefined
  for (const item of items) {
    const day = dayOf(item.ts)
    if (day !== lastDay) {
      rows.push({ key: `day:${day}`, node: <div className="sm-day">{formatDay(item.ts, Date.now(), { today: t('page.thread.today'), yesterday: t('page.thread.yesterday') })}</div> })
      lastDay = day
    }
    rows.push({ key: item.key, node: renderItem(item) })
  }

  const sessionId = history?.sessionId ?? agent.sessionId ?? null

  const paneTab: PaneTab = page.paneTab
  const tabs = tabsFor('agent', false)
  const agentInfo = (
    <div className="sm-home" data-soulmirror-agent-home>
      <div className="sm-home-inner">
        <div className="sm-home-id">
          <span className="sm-avatar sm-avatar-lg" aria-hidden>🤖</span>
          <div style={{ display: 'grid', gap: 2, minWidth: 0 }}>
            <span className="sm-home-id-name">{name}</span>
            <span className="sm-home-id-sub">
              <span className={`sm-livedot${running ? ' sm-busy' : ''}`} aria-hidden />
              <span>{running ? t('settings.agents.status.running') : t('settings.agents.status.idle')}</span>
            </span>
          </div>
        </div>
        <div className="sm-home-card">
          <div className="sm-home-title"><span>{t('agent.home.profile')}</span></div>
          {agent.preset !== undefined ? <div className="sm-home-line"><span className="sm-home-line-key">{t('agent.home.preset')}</span><span className="sm-home-line-val">{agent.preset}</span></div> : null}
          {agent.cwd !== undefined ? <div className="sm-home-line"><span className="sm-home-line-key">{t('agent.home.cwd')}</span><span className="sm-home-line-val">{agent.cwd}</span></div> : null}
          {sessionId !== null ? <div className="sm-home-line"><span className="sm-home-line-key">{t('agent.home.session')}</span><span className="sm-home-line-val">{sessionId}</span></div> : null}
        </div>
        {agent.prompt !== undefined && agent.prompt !== ''
          ? (
            <div className="sm-home-card">
              <div className="sm-home-title"><span>{t('agent.home.prompt')}</span></div>
              <div className="sm-home-rules">{agent.prompt}</div>
            </div>
          )
          : null}
        <div className="sm-home-card">
          <div className="sm-home-title"><span>{t('agent.home.actions')}</span></div>
          <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
            <button type="button" className="sm-ghostbtn" onClick={() => { setSheetOpen(true) }}><IconSettingsOutline16 size={14} /> {t('agent.settings')}</button>
            {sessionId !== null ? <button type="button" className="sm-ghostbtn" onClick={() => { onOpenSession(sessionId) }}><IconRightUpOutline14 size={14} /> {t('alter.openDsh')}</button> : null}
          </div>
        </div>
      </div>
    </div>
  )

  return (
    <section className="sm-chat-col" data-soulmirror-page-chat="agent" data-soulmirror-agent-pane={name} style={{ position: 'relative' }}>
      <header className="sm-chat-head">
        <span className="sm-avatar sm-avatar-lg" aria-hidden>🤖</span>
        <div style={{ flex: 1, minWidth: 0, display: 'grid' }}>
          <div className="sm-chat-head-name">{name}</div>
          <div className="sm-chat-head-sub">
            <span className={`sm-livedot${running ? ' sm-busy' : ''}`} aria-hidden />
            {running ? t('settings.agents.status.running') : t('settings.agents.status.idle')}
            {agent.cwd !== undefined ? <span style={{ opacity: 0.65 }}> · {agent.cwd}</span> : null}
          </div>
        </div>
        <div className="sm-chat-head-actions">
          <Tooltip label={t('agent.settings')} side="bottom">
            <button type="button" className="sm-ghostbtn" onClick={() => { setSheetOpen(true) }} data-soulmirror-agent-settings>
              <IconSettingsOutline16 size={14} /> {t('agent.settings')}
            </button>
          </Tooltip>
          {sessionId !== null
            ? (
              <Tooltip label={t('alter.openDsh.hint')} side="bottom">
                <button type="button" className="sm-ghostbtn" onClick={() => { onOpenSession(sessionId) }} data-soulmirror-agent-open-dsh>
                  <IconRightUpOutline14 size={14} /> {t('alter.openDsh')}
                </button>
              </Tooltip>
            )
            : null}
        </div>
      </header>
      <ContentTabs tabs={tabs} active={paneTab} onChange={pageStore.setPaneTab} t={t} />
      {paneTab === 'info' ? agentInfo : paneTab === 'memory' ? <MemoryPane t={t} allow={{ global: true, agent: name }} scope={{ kind: 'agent', name }} /> : <>
      <div ref={scroller} className="sm-thread" onScroll={onScroll} data-soulmirror-agent-thread>
        <div className="sm-thread-inner">
          {history === undefined && error === undefined ? <span className="sm-muted" style={{ alignSelf: 'center', fontSize: 12 }}>{t('page.thread.loading')}</span> : null}
          {error !== undefined ? <span style={{ alignSelf: 'center', fontSize: 12, color: 'var(--dsw-alias-state-error-primary)' }}>{t('settings.error', { message: error })}</span> : null}
          {history !== undefined && items.length === 0
            ? (
              <div className="sm-empty" data-soulmirror-agent-empty>
                <span className="sm-avatar sm-avatar-lg" aria-hidden>{name.slice(0, 1)}</span>
                <div className="sm-empty-title">{name}</div>
                <p>{t('agent.empty.hint', { name })}</p>
              </div>
            )
            : null}
          {rows.map(row => <div key={row.key} style={{ display: 'contents' }}>{row.node}</div>)}
          {running
            ? <div className="sm-citem sm-alter" data-soulmirror-agent-running><div className="sm-typing" style={{ padding: '2px 0' }}><span className="sm-typing-dots"><span /><span /><span /></span>{t('settings.agents.status.running')}</div></div>
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
            placeholder={t('agent.composer.placeholder', { name })}
            onChange={(e) => { setDraft(e.target.value) }}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                e.preventDefault()
                submit(draft)
              }
            }}
            data-soulmirror-agent-composer
          />
          <Button variant="primary" size="sm" icon={<IconSendOutline16 size={14} />} disabled={draft.trim() === '' || instructing} onClick={() => { submit(draft) }} data-soulmirror-agent-send>
            {instructing ? t('page.composer.instructing') : t('page.composer.send')}
          </Button>
        </div>
      </div>
      {sheetOpen
        ? <AgentSettingsSheet t={t} agent={agent} onClose={() => { setSheetOpen(false) }} onRemoved={onRemoved} />
        : null}
      </>}
    </section>
  )
}
