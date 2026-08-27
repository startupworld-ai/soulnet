/**
 * The built-in CHAT room — the `group.room` occupant under the key `chat`
 * (registered in ../index.ts through the standard slot API, exactly like a
 * third-party room would be). It renders the group's message stream — inbound
 * runs carry a small per-sender avatar and a name label in a stable
 * per-fingerprint colour; consecutive messages from the same sender within
 * five minutes fold into one run — plus day separators and load-older, and it
 * owns the composer: manual sends go out as `by: 'owner'`; the governance
 * profile drives it — humans-off (or a speak-who gate) replaces the composer
 * with one quiet bar, and when agents may speak a per-group "my alter
 * participates" switch reads/writes the `group.settings` alter flag.
 */
import { useEffect, useLayoutEffect, useRef, useState, useSyncExternalStore } from 'react'
import { Button, IconSendOutline16 } from '@deepseek-ai/dsh-client-ui-primitives'
import type { PropsLocale } from '@deepseek-ai/dsh-client-ui-slots'
import { api, networkStore, type ApiChatItem } from '../api.ts'
import { GroupAgentsSheet } from '../GroupAgentsSheet.tsx'
import { ProcessItemView, type ProcessItem } from '../process-ui.tsx'
import type { RoomOwnerProps } from '../group-room.ts'
import type { NS } from '../locales.ts'
import { ALTER_KEY, formatClock, formatDay, groupKey, type ThreadEntry } from '../page-state.ts'
import { pageStore } from '../page-store.ts'
import { buildTimeline, type TimelineRow } from './timeline.ts'
import type { Translate } from '../translate.ts'

export type ChatRoomProps = RoomOwnerProps & PropsLocale<typeof NS>

const FOLLOW_SLACK = 48

/** Stable display hue for one fingerprint (name label + avatar tint). */
function hueOf(fp: string): number {
  let h = 0
  for (let i = 0; i < fp.length; i++) h = ((h * 31) + fp.charCodeAt(i)) >>> 0
  return h % 360
}

function ChatBubble({ entry, t, nameOf, showHeader, myName, onDiscard }: {
  entry: ThreadEntry
  t: Translate
  nameOf: (fp: string | undefined) => string
  /** First message of a sender run: show the avatar and the name label. */
  showHeader: boolean
  /** My display name (voice posts always name their owner: "GroupBob · DevBot"). */
  myName: string
  onDiscard: (clientId: string) => void
}) {
  const out = entry.dir === 'out'
  // Only what the OWNER typed sits on the right; my own voices' posts (alter /
  // named agents) read like participants on the left, labelled with the voice.
  const mine = out && entry.by !== 'alter'
  const pending = entry.status === 'sending'
  const failed = entry.status === 'failed' || entry.status === 'error'
  const voiceLabel = entry.by === 'alter' ? (entry.agent !== undefined && entry.agent !== '' ? entry.agent : t('group.by.alter')) : undefined
  // Every voice post names its OWNER — mine and other members' alike.
  const name = voiceLabel !== undefined
    ? `${out ? myName : nameOf(entry.from)} · ${voiceLabel}`
    : nameOf(entry.from)
  const hue = mine ? 0 : hueOf(voiceLabel !== undefined ? `voice:${name}` : (entry.from ?? ''))
  const bubble = <div className={`sm-bubble${pending ? ' sm-pending' : ''}${failed ? ' sm-failed' : ''}`}>{entry.body}</div>
  const meta = (
    <div className="sm-msg-meta">
      {mine ? <span>{t('group.sender.me')}</span> : null}
      <span>{formatClock(entry.ts)}</span>
      {out && pending ? <span>{t('page.status.sending')}</span> : null}
      {out && entry.status === 'error' ? <span className="sm-status-failed">{t('page.status.error')}</span> : null}
      {out && entry.status === 'failed' ? <span className="sm-status-failed">{t('page.status.failed', { message: entry.error ?? '' })}</span> : null}
      {failed && entry.clientId !== undefined
        ? <button type="button" className="sm-linkbtn" onClick={() => { onDiscard(entry.clientId!) }}>{t('page.status.discard')}</button>
        : null}
    </div>
  )
  if (mine) {
    return (
      <div className="sm-msg sm-out" data-soulmirror-group-bubble="out" data-soulmirror-seq={entry.seq > 0 ? entry.seq : undefined}>
        {bubble}
        {meta}
      </div>
    )
  }
  return (
    <div className="sm-msg sm-in" data-soulmirror-group-bubble={out ? 'voice' : 'in'} data-soulmirror-seq={entry.seq > 0 ? entry.seq : undefined}>
      <div className="sm-gline">
        {showHeader
          ? (
            <span
              className="sm-gavatar"
              aria-hidden
              style={{ background: `hsl(${hue}deg 45% 52% / var(--sm-hue-avatar-a, 0.18))`, color: `hsl(${hue}deg 50% var(--sm-hue-avatar-l, 42%))` }}
            >
              {voiceLabel !== undefined ? '🤖' : name.slice(0, 1)}
            </span>
          )
          : <span className="sm-gspacer" aria-hidden />}
        <div style={{ display: 'grid', gap: 2, minWidth: 0, justifyItems: 'start' }}>
          {showHeader
            ? (
              <div className="sm-msg-meta" style={{ margin: 0 }}>
                {voiceLabel !== undefined ? <span aria-hidden style={{ fontSize: '0.9em' }}>🤖</span> : null}
                <span className="sm-gname" style={{ color: `hsl(${hue}deg 50% var(--sm-hue-name-l, 45%))` }}>{name}</span>
                {out ? <span className="sm-statepill">{t('group.voices.mine')}</span> : null}
              </div>
            )
            : null}
          {bubble}
          {meta}
        </div>
      </div>
    </div>
  )
}

export function ChatRoom({ gid, group, me, members, thread, actions, canSpeakHuman, t }: ChatRoomProps) {
  const [text, setText] = useState('')
  const [voices, setVoices] = useState<Record<string, boolean> | undefined>(undefined)
  const [voiceCommanders, setVoiceCommanders] = useState<Record<string, string[]>>({})
  const [duty, setDuty] = useState<string>('')
  const [muted, setMuted] = useState<boolean>(group.muted === true)
  const [agentNames, setAgentNames] = useState<string[]>([])
  const [agentsSheetOpen, setAgentsSheetOpen] = useState(false)
  const [mention, setMention] = useState<{ start: number; query: string } | undefined>(undefined)
  const [mentionIdx, setMentionIdx] = useState(0)
  const [busyVoices, setBusyVoices] = useState<Record<string, { label: string; at: number }>>({})
  // Live PROCESS feed of MY working agents in this group (local only; others just see the marker).
  const [workFeeds, setWorkFeeds] = useState<Record<string, { items: ApiChatItem[]; running: boolean }>>({})
  const [openWork, setOpenWork] = useState<ReadonlySet<string>>(new Set())
  const feedTimers = useRef<Record<string, ReturnType<typeof setTimeout>>>({})
  const inputRef = useRef<HTMLTextAreaElement>(null)
  const scroller = useRef<HTMLDivElement>(null)
  const following = useRef(true)
  const prevFirstSeq = useRef<number | undefined>(undefined)
  const prevHeight = useRef(0)
  // The alter's pending drafts for THIS group (a reply is waiting for review).
  const net = useSyncExternalStore(networkStore.subscribe, networkStore.getSnapshot)
  const groupDrafts = net.inbox.drafts.filter(d => d.gid === gid)

  // Switching groups: reset the composer and follow the tail again.
  useEffect(() => {
    setText('')
    setVoices(undefined)
    setVoiceCommanders({})
    setDuty('')
    setMention(undefined)
    setAgentsSheetOpen(false)
    setBusyVoices({})
    setWorkFeeds({})
    setOpenWork(new Set())
    following.current = true
  }, [gid])

  // "Working here" markers: remote seats via wire group_typing frames; my own
  // agents via the local SSE agent frames (their `gids`).
  useEffect(() => {
    const off = networkStore.onFrame((frame) => {
      if (frame.kind === 'group_typing' && frame.gid === gid) {
        const key = `${frame.fp}|${frame.agent ?? ''}`
        setBusyVoices((b) => {
          if (frame.on) {
            const senderName = members.find(m => m.fp === frame.fp)?.name ?? (frame.fp.length > 8 ? `${frame.fp.slice(0, 8)}…` : frame.fp)
            const label = frame.agent !== undefined && frame.agent !== '' ? `${senderName} · ${frame.agent}` : senderName
            return { ...b, [key]: { label, at: Date.now() } }
          }
          if (!(key in b)) return b
          const { [key]: _gone, ...rest } = b
          return rest
        })
        return
      }
      if (frame.kind === 'agent') {
        const key = `me|${frame.name}`
        const working = (frame.gids ?? []).includes(gid)
        setBusyVoices((b) => {
          if (working) return { ...b, [key]: { label: frame.name, at: Date.now() } }
          if (!(key in b)) return b
          const { [key]: _gone, ...rest } = b
          return rest
        })
        // The trace persists in the timeline: refetch on every frame of this agent
        // (streaming and the final state alike), never delete.
        const existing = feedTimers.current[frame.name]
        if (existing !== undefined) clearTimeout(existing)
        feedTimers.current[frame.name] = setTimeout(() => {
          delete feedTimers.current[frame.name]
          loadFeed(frame.name)
        }, 350)
      }
    })
    return () => {
      off()
      for (const timer of Object.values(feedTimers.current)) clearTimeout(timer)
      feedTimers.current = {}
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [gid])

  // The per-group "my alter participates" switch (client setting, group.settings).
  const profile = group.profile
  const showAlterChip = profile?.speakAgents === true
  const applySettings = (settings: { voices?: Record<string, { on: true; commanders?: string[] }>; duty?: string; muted?: boolean }): void => {
    const map: Record<string, boolean> = {}
    const cmds: Record<string, string[]> = {}
    for (const [name, v] of Object.entries(settings.voices ?? {})) {
      map[name] = v.on === true
      if (v.commanders !== undefined) cmds[name] = v.commanders
    }
    setVoices(map)
    setVoiceCommanders(cmds)
    setDuty(settings.duty ?? '')
    if (settings.muted !== undefined) setMuted(settings.muted === true)
  }
  useEffect(() => {
    if (!showAlterChip) return
    let cancelled = false
    api.groupSettingsGet(gid).then(({ settings }) => {
      if (cancelled) return
      applySettings(settings)
    }).catch(() => {})
    api.agentsList().then(({ agents }) => {
      if (cancelled) return
      setAgentNames(agents.map(a => a.name))
      // Their past work in this group re-enters the timeline (local only).
      for (const a of agents) loadFeed(a.name)
    }).catch(() => {})
    return () => { cancelled = true }
  }, [gid, showAlterChip])
  const toggleVoice = (name: string): void => {
    const before = voices
    const next = !(voices?.[name] === true)
    setVoices({ ...voices, [name]: next })
    api.groupSettingsSet(gid, { voice: { name, on: next } }).then(({ settings }) => { applySettings(settings) }).catch(() => { setVoices(before) })
  }
  const pickDuty = (value: string): void => {
    const prev = duty
    setDuty(value)
    api.groupSettingsSet(gid, { duty: value === '' ? null : value }).then(({ settings }) => { applySettings(settings) }).catch(() => { setDuty(prev) })
  }
  const toggleMuted = (): void => {
    const prev = muted
    const next = !muted
    setMuted(next)
    api.groupSettingsSet(gid, { muted: next }).then(({ settings }) => { applySettings(settings) }).catch(() => { setMuted(prev) })
  }
  /** Load one of MY agents' work-session trace for this group (local; persists in the timeline). */
  const loadFeed = (name: string): void => {
    api.agentHistory(name, 80, gid).then((h) => {
      setWorkFeeds(f => ({ ...f, [name]: { items: h.chat.items.filter(i => i.kind === 'thinking' || i.kind === 'tool' || i.kind === 'alter' || i.kind === 'turn-failed'), running: h.chat.running } }))
    }).catch(() => {})
  }
  const setCommanders = (name: string, list: string[]): void => {
    api.groupSettingsSet(gid, { voice: { name, on: true, commanders: list } }).then(({ settings }) => { applySettings(settings) }).catch(() => {})
  }

  // Auto-scroll (same behaviour as the friend pane).
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
    // workFeeds / busyVoices grow the timeline asynchronously — keep the tail in view.
  }, [entries, firstSeq, gid, workFeeds, busyVoices])

  const onScroll = (): void => {
    const el = scroller.current
    if (el === null) return
    following.current = el.scrollHeight - el.scrollTop - el.clientHeight < FOLLOW_SLACK
    if (el.scrollTop < 24 && !thread.complete && !thread.loading && thread.loaded) actions.loadOlder()
  }

  const names = new Map<string, string>(members.map(m => [m.fp, m.name]))
  const nameOf = (fp: string | undefined): string =>
    fp === undefined ? '?' : names.get(fp) ?? (fp.length > 8 ? `${fp.slice(0, 8)}…` : fp)

  // @-mention autocomplete: group members, their announced seat agents, my own agents (+ @all).
  const mentionPool: { name: string; owner?: string }[] = []
  {
    const seen = new Set<string>()
    const push = (name: string, owner?: string): void => {
      const key = `${name.toLowerCase()}|${(owner ?? '').toLowerCase()}`
      if (name === '' || seen.has(key)) return
      seen.add(key)
      mentionPool.push({ name, ...(owner === undefined ? {} : { owner }) })
    }
    for (const m of members) {
      push(m.name)
      for (const a of m.agents ?? []) push(a, m.name)
    }
    for (const a of agentNames) push(a, t('group.mention.agent'))
    push('all')
  }
  const mentionMatches = mention === undefined
    ? []
    : mentionPool.filter(p => p.name.toLowerCase().startsWith(mention.query.toLowerCase())).slice(0, 8)
  const detectMention = (value: string, caret: number): void => {
    const upto = value.slice(0, caret)
    const m = /(^|\s)@([^\s@]*)$/.exec(upto)
    setMention(m === null ? undefined : { start: caret - m[2]!.length - 1, query: m[2]! })
    setMentionIdx(0)
  }
  const pickMention = (name: string): void => {
    if (mention === undefined) return
    const caret = mention.start + 1 + mention.query.length
    setText(`${text.slice(0, mention.start)}@${name} ${text.slice(caret)}`)
    setMention(undefined)
    inputRef.current?.focus()
  }

  const send = (): void => {
    const body = text.trim()
    if (body === '' || !canSpeakHuman) return
    setText('')
    following.current = true
    void actions.send(body, { by: 'owner' })
  }

  // Why the human composer is closed, when it is.
  const mutedHint = canSpeakHuman
    ? undefined
    : profile?.speakHumans === false
      ? t('group.speak.humansOff')
      : profile?.speakWho === 'owner'
        ? t('group.speak.whoOwner')
        : t('group.speak.whoAdmins')

  // The per-group "my alter participates" switch + its strategy, shown wherever the
  // composer area is. The strategy can only TIGHTEN the group's wake policy.
  const anyVoiceOn = voices !== undefined && Object.values(voices).some(v => v)
  const alterSwitch = showAlterChip
    ? (
      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8, whiteSpace: 'nowrap', flexWrap: 'wrap' }}>
        <label className={`sm-switch${voices?.['alter'] === true ? ' sm-on' : ''}`} data-soulmirror-group-alter-toggle>
          <input type="checkbox" checked={voices?.['alter'] === true} disabled={voices === undefined} onChange={() => { toggleVoice('alter') }} />
          <span className="sm-switch-track" aria-hidden />
          {t('group.alter.toggle')}
        </label>
        {agentNames.map(name => (
          <label key={name} className={`sm-switch${voices?.[name] === true ? ' sm-on' : ''}`} data-soulmirror-group-voice={name}>
            <input type="checkbox" checked={voices?.[name] === true} disabled={voices === undefined} onChange={() => { toggleVoice(name) }} />
            <span className="sm-switch-track" aria-hidden />
            {name}
          </label>
        ))}
        {anyVoiceOn
          ? (
            <select
              className="sm-select"
              style={{ height: 22, fontSize: 11, padding: '0 4px' }}
              value={duty}
              onChange={(e) => { pickDuty(e.target.value) }}
              aria-label={t('group.voices.duty')}
              data-soulmirror-group-duty
            >
              <option value="">{t('group.voices.duty.none')}</option>
              {voices?.['alter'] === true ? <option value="alter">{t('group.voices.duty.alter')}</option> : null}
              {agentNames.filter(n => voices?.[n] === true).map(n => <option key={n} value={n}>{t('group.voices.duty.name', { name: n })}</option>)}
            </select>
          )
          : null}
        {agentNames.length > 0
          ? <button type="button" className="sm-linkbtn" onClick={() => { setAgentsSheetOpen(true) }} data-soulmirror-group-agents-open>{t('group.agents.button')}</button>
          : null}
      </span>
    )
    : null

  // The visible timeline (./timeline.ts): archive entries merged with my
  // agents' local work traces, day separators and (sender × voice) run
  // folding included — this component only maps rows to elements.
  const timeline: TimelineRow[] = buildTimeline(entries, workFeeds)
  const toggleWork = (key: string): void => {
    setOpenWork((prev) => {
      const next = new Set(prev)
      if (next.has(key)) next.delete(key)
      else next.add(key)
      return next
    })
  }

  return (
    <>
      <div ref={scroller} className="sm-thread" onScroll={onScroll} data-soulmirror-group-thread>
        <div className="sm-thread-inner">
          {thread.loaded && !thread.complete
            ? (
              <button type="button" className="sm-linkbtn sm-muted" style={{ alignSelf: 'center', fontSize: 11 }} disabled={thread.loading} onClick={() => { actions.loadOlder() }}>
                {thread.loading ? t('page.thread.loading') : t('page.thread.loadOlder')}
              </button>
            )
            : null}
          {!thread.loaded && thread.loading ? <span className="sm-muted" style={{ alignSelf: 'center', fontSize: 12 }}>{t('page.thread.loading')}</span> : null}
          {thread.error !== undefined
            ? (
              <span style={{ alignSelf: 'center', display: 'flex', alignItems: 'center', gap: 8, fontSize: 12, color: 'var(--dsw-alias-state-error-primary)' }} data-soulmirror-group-thread-error>
                {t('settings.error', { message: thread.error })}
                <button type="button" className="sm-linkbtn" onClick={() => { actions.reload() }} data-soulmirror-group-thread-retry>{t('page.status.retry')}</button>
              </span>
            )
            : null}
          {thread.loaded && entries.length === 0 && thread.error === undefined
            ? (
              <span className="sm-muted" style={{ alignSelf: 'center', fontSize: 12, padding: '24px 0', textAlign: 'center' }} data-soulmirror-group-thread-empty>
                {canSpeakHuman ? t('group.empty') : t('group.empty.quiet')}
              </span>
            )
            : null}
          {timeline.map(row => row.kind === 'day'
            ? <div key={row.key} className="sm-day">{formatDay(row.ts, Date.now(), { today: t('page.thread.today'), yesterday: t('page.thread.yesterday') })}</div>
            : row.kind === 'entry'
              ? <ChatBubble key={row.key} entry={row.entry} t={t} nameOf={nameOf} showHeader={row.showHeader} myName={me.name} onDiscard={(clientId) => { pageStore.discard(groupKey(gid), clientId) }} />
              : row.kind === 'work-head'
                ? (() => {
                    const name = `${me.name} · ${row.agent}`
                    const hue = hueOf(`voice:${name}`)
                    return (
                      <div key={row.key} className="sm-citem sm-wide" data-soulmirror-group-work-head={row.agent}>
                        <div className="sm-msg-meta" style={{ margin: '6px 0 0', display: 'flex', alignItems: 'center', gap: 6 }}>
                          <span
                            className="sm-gavatar"
                            aria-hidden
                            style={{ background: `hsl(${hue}deg 45% 52% / var(--sm-hue-avatar-a, 0.18))`, color: `hsl(${hue}deg 50% var(--sm-hue-avatar-l, 42%))` }}
                          >
                            {'🤖'}
                          </span>
                          <span className="sm-gname" style={{ color: `hsl(${hue}deg 50% var(--sm-hue-name-l, 45%))` }}>{name}</span>
                          <span className="sm-statepill">{t('group.voices.mine')}</span>
                        </div>
                      </div>
                    )
                  })()
              : row.kind === 'process'
                ? <ProcessItemView key={row.key} item={row.item} t={t} open={openWork.has(row.item.key)} onToggle={toggleWork} />
                : row.kind === 'note'
                  ? (
                    <div key={row.key} className="sm-citem sm-wide" data-soulmirror-group-note>
                      <div style={{ fontSize: 12, opacity: 0.65, fontStyle: 'italic', padding: '1px 4px', whiteSpace: 'pre-wrap' }}>
                        🗒 {row.agent} <span className="sm-statepill">{t('group.note.private')}</span> {row.text}
                      </div>
                    </div>
                  )
                  : (
                    <div key={row.key} className="sm-citem sm-center" data-soulmirror-group-work-failed>
                      <span className="sm-statepill sm-err">⚠ {row.agent}: {row.reason}</span>
                    </div>
                  ))}
          {Object.entries(busyVoices).filter(([, v]) => Date.now() - v.at < 120_000).map(([key, v]) => (
            <div key={key} className="sm-typing" style={{ padding: '2px 0', alignSelf: 'flex-start' }} data-soulmirror-group-busy={key}>
              <span className="sm-typing-dots"><span /><span /><span /></span>{t('group.busy', { label: v.label })}
            </div>
          ))}
        </div>
      </div>
      {groupDrafts.length > 0
        ? (
          <div className="sm-pendbar" data-soulmirror-group-draft-reminder>
            <span>{t('group.draftReminder')}</span>
            <button type="button" className="sm-linkbtn" onClick={() => { pageStore.select(ALTER_KEY) }}>{t('group.draftReminder.go')}</button>
          </div>
        )
        : null}
      {mutedHint !== undefined
        ? (
          // Humans cannot post here: one quiet bar instead of a dead composer.
          <div className="sm-mutebar" data-soulmirror-group-composer data-soulmirror-group-muted>
            <span>{mutedHint}</span>
            {alterSwitch}
          </div>
        )
        : (
          <div className="sm-composer" data-soulmirror-group-composer>
            <div style={{ display: 'flex', alignItems: 'center', gap: 14, padding: '0 16px 6px' }}>
              <label className={`sm-switch${muted ? ' sm-on' : ''}`} data-soulmirror-group-mute>
                <input type="checkbox" checked={muted} onChange={toggleMuted} />
                <span className="sm-switch-track" aria-hidden />
                {t('group.mute')}
              </label>
              {alterSwitch}
            </div>
            <div className="sm-composer-box">
              <div style={{ position: 'relative', flex: 1, display: 'flex' }}>
                {mention !== undefined && mentionMatches.length > 0
                  ? (
                    <div style={{ position: 'absolute', bottom: '100%', left: 0, marginBottom: 6, zIndex: 20, minWidth: 180, maxHeight: 220, overflowY: 'auto', borderRadius: 8, border: '1px solid rgba(127,127,127,.35)', background: 'var(--dsw-specific-menu, var(--dsw-alias-bg-layer-2, #fff))', boxShadow: '0 6px 24px rgba(0,0,0,.18)', padding: 4, display: 'grid' }} data-soulmirror-mention-pop>
                      {mentionMatches.map((p, i) => (
                        <button key={`${p.name}|${p.owner ?? ''}`} type="button" className="sm-linkbtn" style={{ textAlign: 'left', padding: '6px 8px', borderRadius: 6, ...(i === Math.min(mentionIdx, mentionMatches.length - 1) ? { background: 'var(--dsw-alias-interactive-bg-hover)' } : {}) }} onMouseEnter={() => { setMentionIdx(i) }} onMouseDown={(e) => { e.preventDefault(); pickMention(p.name) }}>
                          {p.owner !== undefined ? <span aria-hidden style={{ marginRight: 4 }}>🤖</span> : null}
                          @{p.name}
                          {p.owner !== undefined ? <span className="sm-statepill" style={{ marginLeft: 6 }}>{p.owner}</span> : null}
                        </button>
                      ))}
                    </div>
                  )
                  : null}
                <textarea
                  ref={inputRef}
                  className="sm-textarea"
                  rows={1}
                  value={text}
                  placeholder={t('group.composer.placeholder', { name: group.name })}
                  onChange={(e) => { setText(e.target.value); detectMention(e.target.value, e.target.selectionStart ?? e.target.value.length) }}
                  onBlur={() => { setTimeout(() => { setMention(undefined) }, 120) }}
                  onKeyDown={(e) => {
                    if (e.key === 'Escape' && mention !== undefined) {
                      // This Escape means "close the popup" - it must not bubble
                      // on to the page-level listener that closes SoulMirror.
                      e.preventDefault()
                      e.stopPropagation()
                      setMention(undefined)
                      return
                    }
                    if (mention !== undefined && mentionMatches.length > 0 && (e.key === 'ArrowDown' || e.key === 'ArrowUp')) {
                      e.preventDefault()
                      const len = mentionMatches.length
                      setMentionIdx(i => (Math.min(i, len - 1) + (e.key === 'ArrowDown' ? 1 : len - 1)) % len)
                      return
                    }
                    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                      e.preventDefault()
                      if (mention !== undefined && mentionMatches.length > 0) pickMention(mentionMatches[Math.min(mentionIdx, mentionMatches.length - 1)]!.name)
                      else send()
                    }
                  }}
                  data-soulmirror-group-input
                />
              </div>
              <Button variant="primary" size="sm" icon={<IconSendOutline16 size={14} />} disabled={text.trim() === ''} onClick={send} data-soulmirror-group-send>
                {t('group.send')}
              </Button>
            </div>
            <span className="sm-composer-hint" style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
              <span style={{ flex: 1 }}>{t('group.composer.hint')}</span>
            </span>
          </div>
        )}
      {agentsSheetOpen
        ? (
          <GroupAgentsSheet
            t={t}
            members={members}
            myFp={me.fp}
            agentNames={agentNames}
            voices={voices ?? {}}
            commanders={voiceCommanders}
            onToggle={toggleVoice}
            onCommanders={setCommanders}
            onClose={() => { setAgentsSheetOpen(false) }}
          />
        )
        : null}
    </>
  )
}
