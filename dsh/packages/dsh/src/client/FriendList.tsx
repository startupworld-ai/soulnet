/**
 * Middle column of the SoulMirror page (prototype #B): identity header (name,
 * fingerprint, copy card, drafts-to-review count, protocol editor toggle,
 * refresh, close), search box, the PINNED FIRST ITEM "My alter" (never
 * sorted away — the only place the owner talks), "Pending requests" (accept /
 * reject), "Friends" (avatar with presence dot and unread badge, name, tier
 * pill, last preview + age — a `[draft]` marker when the alter's draft to
 * that friend waits for review; unread-first then newest) and "Add friend"
 * (card URI + note) at the bottom. The selected row is highlighted; clicking
 * a friend row selects it (the friend pane marks it read).
 */
import { useCallback, useEffect, useState, useSyncExternalStore, type ReactNode } from 'react'
import { Button, IconCheckOutline14, IconCloseFill14, IconCloseOutline16, IconCopyOutline16, IconRefreshOutline14, IconSearchOutline16, Tooltip, writeClipboard } from '@deepseek-ai/dsh-client-ui-primitives'
import { ProtocolEditor, TierPill } from './alter-ui.tsx'
import { api, networkStore, type ApiGroup, type ApiGroupProfile } from './api.ts'
import { GroupCreateDialog } from './GroupCreateDialog.tsx'
import type { Translate } from './translate.ts'
import { formatAge, previewOf, type InboxFriend } from './inbox-state.ts'
import { agentKey, ALTER_KEY, filterFriends, groupKey, type Col2Tab } from './page-state.ts'
import { AgentSettingsSheet } from './AgentSettingsSheet.tsx'
import { pageStore } from './page-store.ts'
import { SoulMirrorIcon } from './SidebarEntry.tsx'

export interface FriendListProps {
  t: Translate
  /** `ALTER_KEY` or the selected friend's fingerprint. */
  selected: string
  onSelect: (key: string) => void
  /** A contact row was clicked in the address book: select + open the friend's home page. */
  onSelectContact: (fp: string) => void
  /** A pending request was accepted: the new friend's fp. */
  onAccepted?: (fp: string) => void
  /** Close the page (back to dsh). */
  onClose: () => void
}

/** How many seat agents are shown before the rest collapses behind an expander. */
const AGENT_COLLAPSE = 3

function useCopy(): [copied: boolean, copy: (text: string) => void] {
  const [copied, setCopied] = useState(false)
  const copy = useCallback((text: string) => {
    void writeClipboard(text).then((ok) => {
      if (!ok) return
      setCopied(true)
      setTimeout(() => { setCopied(false) }, 1500)
    })
  }, [])
  return [copied, copy]
}

/** Highlight every occurrence of q (case-insensitive) inside text. */
function Highlight({ text, q }: { text: string; q: string }) {
  if (q === '') return <>{text}</>
  const lower = text.toLowerCase()
  const parts: ReactNode[] = []
  let i = 0
  let k = 0
  for (;;) {
    const j = lower.indexOf(q, i)
    if (j === -1) {
      parts.push(text.slice(i))
      break
    }
    if (j > i) parts.push(text.slice(i, j))
    parts.push(<span key={k++} className="sm-hl">{text.slice(j, j + q.length)}</span>)
    i = j + q.length
  }
  return <>{parts}</>
}

/** A short window of body around the first match of q (for the result snippet). */
function snippetAround(body: string, q: string, radius = 28): string {
  const flat = body.replace(/\s+/g, ' ').trim()
  const j = flat.toLowerCase().indexOf(q)
  if (j === -1) return flat.slice(0, radius * 2)
  const start = Math.max(0, j - radius)
  const end = Math.min(flat.length, j + q.length + radius)
  return `${start > 0 ? '…' : ''}${flat.slice(start, end)}${end < flat.length ? '…' : ''}`
}

/** The WeChat-style group marker: a small multi-person glyph on the avatar corner. */
function GroupMarkIcon({ size = 10 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 12 12" fill="currentColor" aria-hidden>
      <circle cx="4" cy="4.2" r="2.1" />
      <path d="M0.8 10.4c0-1.8 1.5-3 3.2-3s3.2 1.2 3.2 3v0.4H0.8Z" />
      <circle cx="8.6" cy="4.6" r="1.7" opacity="0.75" />
      <path d="M7.6 10.8c0.1-1.5 1-2.6 2.2-2.6 1.3 0 2.3 1 2.3 2.3v0.3H7.6Z" opacity="0.75" />
    </svg>
  )
}

/** One row of the unified conversation list (friends and groups mixed, WeChat-style). */
type ChatRowData =
  | { kind: 'friend'; rowKey: string; f: InboxFriend }
  | { kind: 'group'; rowKey: string; g: ApiGroup }

function ChatRow({ row, typing, apps, selected, t, onSelect }: {
  row: ChatRowData
  typing: boolean
  /** Pending join applications (groups I administer). */
  apps: number
  selected: boolean
  t: Translate
  onSelect: () => void
}) {
  const name = row.kind === 'friend' ? row.f.name : row.g.name
  const drafts = row.kind === 'friend' ? row.f.drafts ?? 0 : 0
  const preview = row.kind === 'friend'
    ? (typing ? t('inbox.typing') : previewOf(row.f.lastBody))
    : (row.g.lastBody === undefined || row.g.lastBody === '' ? t('group.members', { n: row.g.members }) : previewOf(row.g.lastBody))
  const lastTs = row.kind === 'friend' ? row.f.lastTs : row.g.lastTs
  return (
    <li>
      <button
        type="button"
        className={`sm-row${selected ? ' sm-selected' : ''}`}
        onClick={onSelect}
        onMouseEnter={row.kind === 'group' ? () => { pageStore.prefetchGroup(row.g.gid) } : undefined}
        aria-pressed={selected}
        data-soulmirror-page-friend={row.kind === 'friend' ? row.f.fp : undefined}
        data-soulmirror-page-group={row.kind === 'group' ? row.g.gid : undefined}
        data-soulmirror-selected={selected ? 'true' : undefined}
        data-soulmirror-row-drafts={drafts > 0 ? drafts : undefined}
      >
        <span className="sm-avawrap">
          <span className="sm-avatar" aria-hidden>{name.slice(0, 1)}</span>
          {row.kind === 'friend'
            ? <span className={`sm-presence${row.f.online === true ? ' sm-online' : ''}`} title={row.f.online === true ? t('inbox.online') : t('inbox.offline')} />
            : <span className="sm-gmark" aria-hidden title={t('group.section')}><GroupMarkIcon /></span>}
          {apps > 0 ? <span className="sm-badge sm-badge-warn" data-soulmirror-group-row-apps={apps}>{apps}</span> : null}
        </span>
        <span className="sm-row-body">
          <span className="sm-row-title">
            <span className="sm-row-name">{name}</span>
            {row.kind === 'friend' && row.f.tier !== undefined && row.f.tier !== 'draft' ? <TierPill tier={row.f.tier} t={t} /> : null}
            <span className="sm-row-time">{formatAge(lastTs)}</span>
          </span>
          <span className="sm-row-preview">
            {drafts > 0 ? <span className="sm-alert">{t('page.row.draftTag')} </span> : null}
            {preview === '' ? t('inbox.noMessages') : preview}
          </span>
        </span>
      </button>
    </li>
  )
}

/** One row of the address book (通讯录): avatar + name + presence, alphabetical, no message preview. */
function ContactRow({ f, selected, t, onSelect }: { f: InboxFriend; selected: boolean; t: Translate; onSelect: () => void }) {
  return (
    <li>
      <button
        type="button"
        className={`sm-row${selected ? ' sm-selected' : ''}`}
        onClick={onSelect}
        aria-pressed={selected}
        data-soulmirror-page-contact={f.fp}
        data-soulmirror-selected={selected ? 'true' : undefined}
      >
        <span className="sm-avawrap">
          <span className="sm-avatar" aria-hidden>{f.name.slice(0, 1)}</span>
          <span className={`sm-presence${f.online === true ? ' sm-online' : ''}`} title={f.online === true ? t('inbox.online') : t('inbox.offline')} />
        </span>
        <span className="sm-row-body">
          <span className="sm-row-title">
            <span className="sm-row-name">{f.name}</span>
            {f.tier !== undefined && f.tier !== 'draft' ? <TierPill tier={f.tier} t={t} /> : null}
          </span>
        </span>
      </button>
    </li>
  )
}

export function FriendList({ t, selected, onSelect, onSelectContact, onAccepted, onClose }: FriendListProps) {
  const net = useSyncExternalStore(networkStore.subscribe, networkStore.getSnapshot)
  const page = useSyncExternalStore(pageStore.subscribe, pageStore.getSnapshot)
  const { inbox } = net
  const identity = net.state?.identity ?? null
  const status = net.status ?? net.state?.status
  const [copied, copy] = useCopy()
  const [query, setQuery] = useState('')
  const [addUri, setAddUri] = useState('')
  const [addNote, setAddNote] = useState('')
  const [busy, setBusy] = useState<string | undefined>(undefined)
  const [note, setNote] = useState<{ kind: 'ok' | 'error'; text: string } | undefined>(undefined)
  const [protocolOpen, setProtocolOpen] = useState(false)
  const [createOpen, setCreateOpen] = useState(false)
  const [joinUri, setJoinUri] = useState('')
  /** The WeChat-style "+" menu and the small dialog it opens. */
  const [plusOpen, setPlusOpen] = useState(false)
  const [agentSheetOpen, setAgentSheetOpen] = useState(false)
  const [agentsExpanded, setAgentsExpanded] = useState(false)
  const [dialog, setDialog] = useState<'add' | 'join' | undefined>(undefined)

  // Typing a query warms every archive (sequentially — never floods the
  // connection pool) so the content search has data to look through.
  useEffect(() => {
    if (query.trim() === '') return
    void pageStore.warm([
      ...inbox.friends.map(f => ({ kind: 'friend' as const, fp: f.fp })),
      ...inbox.groups.map(g => ({ kind: 'group' as const, gid: g.gid })),
    ])
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [query])

  // Escape closes the open small dialog (and the + menu) before the page would close.
  useEffect(() => {
    if (dialog === undefined && !plusOpen) return
    const onKey = (e: KeyboardEvent): void => {
      if (e.key !== 'Escape') return
      setDialog(undefined)
      setPlusOpen(false)
    }
    document.addEventListener('keydown', onKey)
    return () => { document.removeEventListener('keydown', onKey) }
  }, [dialog, plusOpen])
  // One unified conversation list (WeChat-style): friends and groups mixed,
  // newest first; a corner glyph marks groups. The search does not filter the
  // list — it opens a RESULTS PANEL over it (names + message content).
  const q = query.trim().toLowerCase()
  const rows: ChatRowData[] = [
    ...inbox.friends.map(f => ({ kind: 'friend' as const, rowKey: f.fp, f })),
    ...inbox.groups.map(g => ({ kind: 'group' as const, rowKey: groupKey(g.gid), g })),
  ].sort((a, b) => {
    const ta = (a.kind === 'friend' ? a.f.lastTs : a.g.lastTs) ?? 0
    const tb = (b.kind === 'friend' ? b.f.lastTs : b.g.lastTs) ?? 0
    if (ta !== tb) return tb - ta
    const na = a.kind === 'friend' ? a.f.name : a.g.name
    const nb = b.kind === 'friend' ? b.f.name : b.g.name
    return na.localeCompare(nb)
  })
  // Split the combined rows by the second-column tab.
  const friendRows = rows.filter((r): r is Extract<ChatRowData, { kind: 'friend' }> => r.kind === 'friend')
  const groupRows = rows.filter((r): r is Extract<ChatRowData, { kind: 'group' }> => r.kind === 'group')
  // Address book (通讯录): all friends, alphabetical, filtered by the search query.
  const contactRows = filterFriends(inbox.friends, query).sort((a, b) => a.name.localeCompare(b.name))
  // Seat agents collapse to AGENT_COLLAPSE until the owner expands them.
  const agents = net.state?.agents ?? []
  const agentsExpandedNow = agentsExpanded || agents.length <= AGENT_COLLAPSE
  const visibleAgents = agentsExpandedNow ? agents : agents.slice(0, AGENT_COLLAPSE)
  // Search hits: names first, then message content out of the cached archives
  // (all threads are prefetched while the page is open).
  const nameHits: ChatRowData[] = q === ''
    ? []
    : rows.filter(r => r.kind === 'friend'
      ? r.f.name.toLowerCase().includes(q) || (r.f.remark ?? '').toLowerCase().includes(q) || r.f.fp.toLowerCase().startsWith(q)
      : r.g.name.toLowerCase().includes(q))
  const contentHits = q === ''
    ? []
    : ((): { rowKey: string; kind: 'friend' | 'group'; name: string; body: string; ts: number; id: string }[] => {
        const out: { rowKey: string; kind: 'friend' | 'group'; name: string; body: string; ts: number; id: string }[] = []
        for (const f of inbox.friends) {
          for (const e of pageStore.thread(f.fp).entries) {
            if (e.body.toLowerCase().includes(q)) out.push({ rowKey: f.fp, kind: 'friend', name: f.name, body: e.body, ts: e.ts, id: e.id })
          }
        }
        for (const g of inbox.groups) {
          for (const e of pageStore.groupThread(g.gid).entries) {
            if (e.body.toLowerCase().includes(q)) out.push({ rowKey: groupKey(g.gid), kind: 'group', name: g.name, body: e.body, ts: e.ts, id: e.id })
          }
        }
        return out.sort((a, b) => b.ts - a.ts).slice(0, 30)
      })()
  const drafts = inbox.drafts.length
  const alterRunning = page.alter.status === 'running' || page.alter.chat.running || page.alter.instructing
  const alterSelected = selected === ALTER_KEY
  const col2Tab: Col2Tab = page.col2Tab

  const run = async (key: string, action: () => Promise<string | undefined>): Promise<void> => {
    setBusy(key)
    setNote(undefined)
    try {
      const ok = await action()
      if (ok !== undefined) setNote({ kind: 'ok', text: ok })
      await networkStore.refresh()
    } catch (e: unknown) {
      setNote({ kind: 'error', text: e instanceof Error ? e.message : String(e) })
    } finally {
      setBusy(undefined)
    }
  }

  const submitAdd = (): void => {
    if (busy !== undefined || addUri.trim() === '' || identity === null) return
    void run('add', async () => {
      const { friend } = await api.addFriend(addUri.trim(), addNote.trim() === '' ? undefined : addNote.trim())
      setAddUri('')
      setAddNote('')
      setDialog(undefined)
      return t('settings.add.sent', { name: friend.name })
    })
  }

  const createGroup = (name: string, members: string[], profile: ApiGroupProfile): void => {
    if (busy !== undefined) return
    void run('group.create', async () => {
      const { group } = await api.groupCreate(name, members, profile)
      setCreateOpen(false)
      onSelect(groupKey(group.gid))
      return t('group.create.created', { name: group.name })
    })
  }

  const submitJoin = (): void => {
    if (busy !== undefined || joinUri.trim() === '' || identity === null) return
    void run('group.apply', async () => {
      const { gid } = await api.groupApply(joinUri.trim())
      setJoinUri('')
      setDialog(undefined)
      if (gid !== '') { pageStore.setCol2Tab('messages'); onSelect(groupKey(gid)) }
      return t('group.join.applied')
    })
  }

  return (
    <aside className="sm-list-col" data-soulmirror-page-list>
      <div className="sm-list-head">
        <div style={{ flex: 1, minWidth: 0, display: 'grid', gap: 1 }}>
          <div className="sm-list-head-title">
            <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{identity?.name ?? t('page.title')}</span>
            {drafts > 0
              ? (
                <Tooltip label={t('page.drafts.count', { n: drafts })} side="bottom">
                  <span className="sm-badge sm-badge-warn" data-soulmirror-page-drafts-total={drafts}>{t('page.row.draftTag')} {drafts}</span>
                </Tooltip>
              )
              : null}
          </div>
          {identity !== null
            ? (
              <div className="sm-muted" style={{ display: 'flex', alignItems: 'center', gap: 6, minWidth: 0, fontSize: 12 }} data-soulmirror-page-identity>
                <span style={{ fontFamily: 'var(--ds-font-family-code, ui-monospace, monospace)', opacity: 0.75 }}>{identity.fp.slice(0, 12)}…</span>
                <Tooltip label={copied ? t('inbox.card.copied') : t('inbox.card.copy')} side="bottom">
                  <button type="button" className="sm-iconbtn" aria-label={t('inbox.card.copy')} onClick={() => { copy(identity.cardUri) }}>
                    {copied ? <IconCheckOutline14 size={14} /> : <IconCopyOutline16 size={14} />}
                  </button>
                </Tooltip>
                {inbox.pending.length > 0 ? <span style={{ color: 'var(--dsw-alias-state-warn-primary)' }}>{t('page.identity.pending', { n: inbox.pending.length })}</span> : null}
              </div>
            )
            : <span className="sm-muted" style={{ fontSize: 12 }}>{status?.state === 'ready' || status === undefined ? t('inbox.noIdentity') : t('onboard.waiting', { state: t(`settings.state.${status.state}`) })}</span>}
        </div>
        <Tooltip label={t('protocol.title')} side="bottom">
          <button type="button" className="sm-ghostbtn" aria-label={t('protocol.title')} aria-expanded={protocolOpen} onClick={() => { setProtocolOpen(v => !v) }} data-soulmirror-page-protocol-toggle>
            {t('protocol.open')}
          </button>
        </Tooltip>
        <Tooltip label={t('settings.refresh')} side="bottom">
          <button type="button" className="sm-iconbtn" aria-label={t('settings.refresh')} onClick={() => { void networkStore.refresh() }}>
            <IconRefreshOutline14 size={14} />
          </button>
        </Tooltip>
        <Tooltip label={t('page.close')} side="bottom">
          <button type="button" className="sm-iconbtn" aria-label={t('page.close')} onClick={onClose} data-soulmirror-page-close>
            <IconCloseOutline16 size={14} />
          </button>
        </Tooltip>
      </div>
      <div className="sm-list-search" style={{ position: 'relative' }}>
        <span style={{ display: 'flex', alignItems: 'center', flex: 1, gap: 6, minWidth: 0 }}>
          <IconSearchOutline16 size={14} />
          <input
            className="sm-input"
            placeholder={t('page.search')}
            value={query}
            onChange={(e) => { setQuery(e.target.value) }}
            onKeyDown={(e) => { if (e.key === 'Escape' && query !== '') { e.stopPropagation(); setQuery('') } }}
            aria-label={t('page.search')}
            data-soulmirror-page-search
          />
          {query !== ''
            ? (
              <button type="button" className="sm-iconbtn" aria-label={t('inbox.close')} onClick={() => { setQuery('') }}>
                <IconCloseOutline16 size={12} />
              </button>
            )
            : null}
        </span>
        {q !== ''
          ? (
            <div className="sm-search-pop" data-soulmirror-search-results>
              {nameHits.length === 0 && contentHits.length === 0
                ? <span className="sm-muted" style={{ padding: '10px 12px', fontSize: 12 }}>{t('page.empty.noMatch', { query })}</span>
                : (
                  <>
                    {nameHits.length > 0 ? <div className="sm-section" style={{ padding: '6px 12px 2px' }}>{t('search.chats')}</div> : null}
                    {nameHits.map(r => (
                      <button
                        key={`n-${r.rowKey}`}
                        type="button"
                        className="sm-search-hit"
                        onClick={() => { onSelect(r.rowKey); setQuery('') }}
                        data-soulmirror-search-name={r.rowKey}
                      >
                        <span className="sm-avawrap">
                          <span className="sm-avatar sm-avatar-sm" aria-hidden>{(r.kind === 'friend' ? r.f.name : r.g.name).slice(0, 1)}</span>
                          {r.kind === 'group' ? <span className="sm-gmark" aria-hidden><GroupMarkIcon size={8} /></span> : null}
                        </span>
                        <span className="sm-search-hit-body">
                          <span className="sm-row-name"><Highlight text={r.kind === 'friend' ? r.f.name : r.g.name} q={q} /></span>
                        </span>
                      </button>
                    ))}
                    {contentHits.length > 0 ? <div className="sm-section" style={{ padding: '6px 12px 2px' }}>{t('search.messages')}</div> : null}
                    {contentHits.map(h => (
                      <button
                        key={`c-${h.id}`}
                        type="button"
                        className="sm-search-hit"
                        onClick={() => { onSelect(h.rowKey); setQuery('') }}
                        data-soulmirror-search-message={h.id}
                      >
                        <span className="sm-avawrap">
                          <span className="sm-avatar sm-avatar-sm" aria-hidden>{h.name.slice(0, 1)}</span>
                          {h.kind === 'group' ? <span className="sm-gmark" aria-hidden><GroupMarkIcon size={8} /></span> : null}
                        </span>
                        <span className="sm-search-hit-body">
                          <span className="sm-row-title">
                            <span className="sm-row-name">{h.name}</span>
                            <span className="sm-row-time">{formatAge(h.ts)}</span>
                          </span>
                          <span className="sm-row-preview"><Highlight text={snippetAround(h.body, q)} q={q} /></span>
                        </span>
                      </button>
                    ))}
                  </>
                )}
            </div>
          )
          : null}
        <span className="sm-plusmenu-wrap">
          <button
            type="button"
            className="sm-ghostbtn"
            style={{ width: 28, padding: 0, justifyContent: 'center', fontSize: 16, lineHeight: 1 }}
            aria-label={t('plus.menu')}
            aria-haspopup="menu"
            aria-expanded={plusOpen}
            disabled={identity === null}
            onClick={() => { setPlusOpen(v => !v) }}
            data-soulmirror-plus
          >
            ＋
          </button>
          {plusOpen
            ? (
              <>
                <div className="sm-plus-backdrop" role="presentation" onClick={() => { setPlusOpen(false) }} />
                <div className="sm-plusmenu" role="menu" data-soulmirror-plus-menu>
                  <button type="button" role="menuitem" onClick={() => { setPlusOpen(false); pageStore.setCol2Tab('messages'); setCreateOpen(true) }} data-soulmirror-plus-new-group>
                    {t('group.create')}
                  </button>
                  <button type="button" role="menuitem" onClick={() => { setPlusOpen(false); setDialog('add') }} data-soulmirror-plus-add-friend>
                    {t('foot.addFriend')}
                  </button>
                  <button type="button" role="menuitem" onClick={() => { setPlusOpen(false); setDialog('join') }} data-soulmirror-plus-join-group>
                    {t('foot.joinGroup')}
                  </button>
                </div>
              </>
            )
            : null}
        </span>
      </div>
      <div className="sm-col2-tabs" data-soulmirror-col2-tabs>
        {(['messages', 'contacts'] as const).map(tab => (
          <button
            key={tab}
            type="button"
            className={`sm-col2-tab${col2Tab === tab ? ' sm-active' : ''}`}
            onClick={() => { pageStore.setCol2Tab(tab) }}
            aria-pressed={col2Tab === tab}
            data-soulmirror-col2-tab={tab}
          >
            {t(`col2.${tab}`)}
          </button>
        ))}
      </div>
      <div className="sm-list-body">
        {protocolOpen ? <ProtocolEditor t={t} /> : null}

        {/* ——— 消息: My alter + 智能体 + 群聊 + 好友会话 ——— */}
        {col2Tab === 'messages' && (
          <>
            <div className="sm-section">{t('col2.contacts.section')}</div>
            <ul style={{ listStyle: 'none', margin: 0, padding: 0 }}>
              <li>
                <button
                  type="button"
                  className={`sm-row sm-row-alter${alterSelected ? ' sm-selected' : ''}`}
                  onClick={() => { onSelect(ALTER_KEY) }}
                  aria-pressed={alterSelected}
                  data-soulmirror-page-alter
                  data-soulmirror-selected={alterSelected ? 'true' : undefined}
                >
                  <span className="sm-avawrap">
                    <span className="sm-avatar sm-avatar-alter" aria-hidden><SoulMirrorIcon size={16} /></span>
                    {drafts > 0 ? <span className="sm-badge" data-soulmirror-alter-drafts={drafts}>{drafts}</span> : null}
                  </span>
                  <span className="sm-row-body">
                    <span className="sm-row-title">
                      <span className="sm-row-name">{t('alter.me')} <span className={`sm-livedot${alterRunning ? ' sm-busy' : ''}`} aria-hidden /></span>
                    </span>
                    <span className="sm-row-preview">{alterRunning ? t('alter.status.running') : drafts > 0 ? t('alter.row.drafts', { n: drafts }) : t('alter.row.hint')}</span>
                  </span>
                </button>
              </li>
            </ul>

            <div className="sm-section" style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
              <span style={{ flex: 1 }}>{t('agents.section')}</span>
              <button type="button" className="sm-linkbtn" onClick={() => { setAgentSheetOpen(true) }} data-soulmirror-agents-add>＋ {t('settings.agents.add')}</button>
            </div>
            {agents.length === 0
              ? <p className="sm-muted" style={{ margin: 0, padding: '4px 10px 10px', fontSize: 12 }} data-soulmirror-page-empty-list>{t('col2.agents.empty')}</p>
              : (
                <>
                  <ul style={{ listStyle: 'none', margin: 0, padding: 0 }}>
                    {visibleAgents.map((a) => {
                      const key = agentKey(a.name)
                      const agentSelected = selected === key
                      return (
                        <li key={a.name}>
                          <button
                            type="button"
                            className={`sm-row${agentSelected ? ' sm-selected' : ''}`}
                            onClick={() => { onSelect(key) }}
                            aria-pressed={agentSelected}
                            data-soulmirror-page-agent={a.name}
                            data-soulmirror-selected={agentSelected ? 'true' : undefined}
                          >
                            <span className="sm-avawrap">
                              <span className="sm-avatar" aria-hidden>🤖</span>
                            </span>
                            <span className="sm-row-body">
                              <span className="sm-row-title">
                                <span className="sm-row-name">{a.name} <span className={`sm-livedot${a.status === 'running' ? ' sm-busy' : ''}`} aria-hidden /></span>
                              </span>
                              <span className="sm-row-preview">{a.status === 'running' ? t('settings.agents.status.running') : a.cwd ?? t('settings.agents.status.idle')}</span>
                            </span>
                          </button>
                        </li>
                      )
                    })}
                  </ul>
                  {agents.length > AGENT_COLLAPSE
                    ? (
                      <button type="button" className="sm-linkbtn" style={{ margin: '2px 10px 6px' }} onClick={() => { setAgentsExpanded(v => !v) }} data-soulmirror-agents-expand>
                        {agentsExpandedNow ? t('agents.collapse') : t('agents.expand', { n: agents.length - AGENT_COLLAPSE })}
                      </button>
                    )
                    : null}
                </>
              )}
            {agentSheetOpen
              ? <AgentSettingsSheet t={t} onClose={() => { setAgentSheetOpen(false) }} onSaved={(name) => { onSelect(agentKey(name)) }} />
              : null}

            <div className="sm-section">{t('groups.section')}</div>
            {groupRows.length === 0
              ? <p className="sm-muted" style={{ margin: 0, padding: '4px 10px 10px', fontSize: 12 }} data-soulmirror-page-empty-list>{q !== '' ? t('page.empty.noMatch', { query }) : t('col2.groups.empty')}</p>
              : (
                <ul style={{ listStyle: 'none', margin: 0, padding: 0 }} data-soulmirror-page-groups>
                  {groupRows.map(r => (
                    <ChatRow
                      key={r.rowKey}
                      row={r}
                      typing={false}
                      apps={inbox.groupApps[r.g.gid] ?? 0}
                      selected={selected === r.rowKey}
                      t={t}
                      onSelect={() => { onSelect(r.rowKey) }}
                    />
                  ))}
                </ul>
              )}
            {createOpen
              ? (
                <GroupCreateDialog
                  t={t}
                  friends={inbox.friends}
                  busy={busy !== undefined}
                  onCreate={createGroup}
                  onClose={() => { setCreateOpen(false) }}
                />
              )
              : null}

            <div className="sm-section">{t('messages.friends')}</div>
            {friendRows.length === 0
              ? (
                <p className="sm-muted" style={{ margin: 0, padding: '4px 10px 10px', fontSize: 12 }} data-soulmirror-page-empty-list>
                  {identity === null ? t('page.empty.noFriends.noIdentity') : q !== '' ? t('page.empty.noMatch', { query }) : t('chats.empty')}
                </p>
              )
              : (
                <ul style={{ listStyle: 'none', margin: 0, padding: 0 }} data-soulmirror-page-friends data-soulmirror-page-chats>
                  {friendRows.map(r => (
                    <ChatRow
                      key={r.rowKey}
                      row={r}
                      typing={inbox.typing[r.f.fp] === true || r.f.typing === true}
                      apps={0}
                      selected={selected === r.rowKey}
                      t={t}
                      onSelect={() => { onSelect(r.rowKey) }}
                    />
                  ))}
                </ul>
              )}
          </>
        )}

        {/* ——— 通讯录: 待处理 + 全部好友地址簿 ——— */}
        {col2Tab === 'contacts' && (
          <>
            {inbox.pending.length > 0
              ? (
                <>
                  <div className="sm-section">{t('page.pending')} · {inbox.pending.length}</div>
                  <ul style={{ listStyle: 'none', margin: 0, padding: 0 }}>
                    {inbox.pending.map(p => (
                      <li key={p.id} className="sm-req" data-soulmirror-page-pending={p.id}>
                        <span className="sm-avatar sm-avatar-sm" aria-hidden>{p.name.slice(0, 1)}</span>
                        <span className="sm-row-body">
                          <span className="sm-row-title"><span className="sm-row-name">{p.name}</span><span className="sm-row-time">{p.createdAt === undefined ? '' : formatAge(Date.parse(p.createdAt))}</span></span>
                          <span className="sm-row-preview">{p.greeting === '' ? t('inbox.pending.noGreeting') : previewOf(p.greeting)}</span>
                        </span>
                        <span className="sm-pending-actions">
                          <Tooltip label={t('settings.pending.accept')} side="bottom">
                            <button
                              type="button"
                              className="sm-iconbtn"
                              aria-label={t('settings.pending.accept')}
                              disabled={busy !== undefined}
                              onClick={() => {
                                void run(`accept:${p.id}`, async () => {
                                  const r = await api.accept(p.id)
                                  onAccepted?.(r.friend.fp)
                                  return t('inbox.pending.accepted', { name: r.friend.name })
                                })
                              }}
                            >
                              <IconCheckOutline14 size={14} />
                            </button>
                          </Tooltip>
                          <Tooltip label={t('settings.pending.reject')} side="bottom">
                            <button type="button" className="sm-iconbtn" aria-label={t('settings.pending.reject')} disabled={busy !== undefined} onClick={() => { void run(`reject:${p.id}`, async () => { await api.reject(p.id); return undefined }) }}>
                              <IconCloseFill14 size={14} />
                            </button>
                          </Tooltip>
                        </span>
                      </li>
                    ))}
                  </ul>
                </>
              )
              : null}
            <div className="sm-section">{t('inbox.friends')}</div>
            {contactRows.length === 0
              ? (
                <p className="sm-muted" style={{ margin: 0, padding: '4px 10px 10px', fontSize: 12 }} data-soulmirror-page-empty-list>
                  {identity === null ? t('page.empty.noFriends.noIdentity') : q !== '' ? t('page.empty.noMatch', { query }) : t('inbox.empty')}
                </p>
              )
              : (
                <ul style={{ listStyle: 'none', margin: 0, padding: 0 }} data-soulmirror-page-contacts>
                  {contactRows.map(f => (
                    <ContactRow
                      key={f.fp}
                      f={f}
                      selected={selected === f.fp}
                      t={t}
                      onSelect={() => { onSelectContact(f.fp) }}
                    />
                  ))}
                </ul>
              )}
          </>
        )}
      </div>
      {note !== undefined || net.error !== undefined
        ? (
          <div className="sm-list-foot">
            {note !== undefined
              ? <span style={{ fontSize: 12, color: note.kind === 'error' ? 'var(--dsw-alias-state-error-primary)' : 'var(--dsw-alias-label-secondary)' }} role={note.kind === 'error' ? 'alert' : undefined}>{note.kind === 'error' ? t('settings.error', { message: note.text }) : note.text}</span>
              : <span style={{ fontSize: 12, color: 'var(--dsw-alias-state-error-primary)' }}>{t('settings.error', { message: net.error ?? '' })}</span>}
          </div>
        )
        : null}
      {dialog === 'add'
        ? (
          <div className="sm-modal-backdrop" role="presentation" onClick={(e) => { if (e.target === e.currentTarget) setDialog(undefined) }}>
            <div className="sm-modal" role="dialog" aria-label={t('page.add')} data-soulmirror-modal data-soulmirror-dialog-add>
              <div className="sm-modal-head">
                <span className="sm-modal-title">{t('page.add')}</span>
                <button type="button" className="sm-iconbtn" aria-label={t('inbox.close')} onClick={() => { setDialog(undefined) }}>
                  <IconCloseOutline16 size={14} />
                </button>
              </div>
              <label className="sm-field">
                <span>{t('page.add.uri')}</span>
                <input
                  className="sm-input sm-input-lg"
                  placeholder={t('page.add.uri')}
                  value={addUri}
                  autoFocus
                  onChange={(e) => { setAddUri(e.target.value) }}
                  onKeyDown={(e) => { if (e.key === 'Enter' && !e.nativeEvent.isComposing) submitAdd() }}
                  data-soulmirror-page-add
                />
              </label>
              <label className="sm-field">
                <span>{t('page.add.note')}</span>
                <input
                  className="sm-input sm-input-lg"
                  placeholder={t('page.add.note')}
                  value={addNote}
                  onChange={(e) => { setAddNote(e.target.value) }}
                  onKeyDown={(e) => { if (e.key === 'Enter' && !e.nativeEvent.isComposing) submitAdd() }}
                />
              </label>
              <div className="sm-modal-foot">
                <Button variant="outline" size="sm" onClick={() => { setDialog(undefined) }}>{t('inbox.close')}</Button>
                <Button variant="primary" size="sm" disabled={busy !== undefined || addUri.trim() === ''} onClick={submitAdd}>
                  {t('page.add.send')}
                </Button>
              </div>
            </div>
          </div>
        )
        : null}
      {dialog === 'join'
        ? (
          <div className="sm-modal-backdrop" role="presentation" onClick={(e) => { if (e.target === e.currentTarget) setDialog(undefined) }}>
            <div className="sm-modal" role="dialog" aria-label={t('group.join.section')} data-soulmirror-modal data-soulmirror-dialog-join>
              <div className="sm-modal-head">
                <span className="sm-modal-title">{t('group.join.section')}</span>
                <button type="button" className="sm-iconbtn" aria-label={t('inbox.close')} onClick={() => { setDialog(undefined) }}>
                  <IconCloseOutline16 size={14} />
                </button>
              </div>
              <label className="sm-field">
                <span>{t('group.join.uri')}</span>
                <input
                  className="sm-input sm-input-lg"
                  placeholder={t('group.join.uri')}
                  value={joinUri}
                  autoFocus
                  onChange={(e) => { setJoinUri(e.target.value) }}
                  onKeyDown={(e) => { if (e.key === 'Enter' && !e.nativeEvent.isComposing) submitJoin() }}
                  data-soulmirror-group-join-uri
                />
              </label>
              <div className="sm-modal-foot">
                <Button variant="outline" size="sm" onClick={() => { setDialog(undefined) }}>{t('inbox.close')}</Button>
                <Button variant="outline" size="sm" disabled={busy !== undefined || joinUri.trim() === ''} onClick={submitJoin} data-soulmirror-group-join-apply>
                  {t('group.join.apply')}
                </Button>
              </div>
            </div>
          </div>
        )
        : null}
    </aside>
  )
}
