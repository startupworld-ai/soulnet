/**
 * Open/closed state and selection of the SoulMirror page plus the per-friend
 * thread cache and the "My alter" transcript, shared between the sidebar
 * footer button (`sidebar.footer.action`), the `/soulmirror` popup, the view
 * tab and the page itself (`shell.overlay`). Threads are folded with the pure
 * functions of ./page-state.ts: archive fetches (`conversation.get`), SSE
 * `message` / `outbound` frames (through `networkStore.onFrame`), optimistic
 * debug sends and their reconcile. The alter transcript is fetched from
 * `session.history` and refetched (debounced) on every SSE `alter` frame.
 * One store per page; components read it through `useSyncExternalStore`.
 */
import { api, networkStore, type ApiChat, type NetworkEventFrame } from './api.ts'
import {
  addOptimistic, agentOf, ALTER_KEY, applyArchive, applyInbound, applyOutbound, DEFAULT_PANE_TAB, dropEntry, EMPTY_THREAD, failSend, gidOf, groupKey, PAGE_SIZE, reconcileSend,
  type Col2Tab, type PaneTab, type ThreadState,
} from './page-state.ts'

/** The alter transcript as the page holds it (P4). */
export interface AlterView {
  readonly sessionId: string | undefined
  readonly status: 'idle' | 'running'
  readonly chat: ApiChat
  readonly loading: boolean
  readonly loaded: boolean
  /** An instruction is being posted. */
  readonly instructing: boolean
  readonly error?: string
}

export const EMPTY_ALTER: AlterView = { sessionId: undefined, status: 'idle', chat: { items: [], running: false, seq: 0 }, loading: false, loaded: false, instructing: false }

export interface PageSnapshot {
  readonly open: boolean
  /** Selection: `ALTER_KEY` or a friend fingerprint (may point at a friend that is gone; the page resolves it). */
  readonly selected: string | undefined
  /** Which section the second column shows. */
  readonly col2Tab: Col2Tab
  /** Which panel of the third column (content area) is active. */
  readonly paneTab: PaneTab
  /** fp → thread (only for friends whose archive was fetched at least once). */
  readonly threads: Readonly<Record<string, ThreadState>>
  readonly alter: AlterView
  /** Draft ids with a decision in flight. */
  readonly deciding: readonly string[]
}

/** Typing signal cadence: re-send `on` at most every 4 s while typing, `off` after 5 s of silence. */
const TYPING_RESEND_MS = 4_000
const TYPING_IDLE_MS = 5_000
/** Debounce of the transcript refetch after an `alter` frame. */
const HISTORY_REFETCH_MS = 120
/** Items of the transcript fetched per load. */
export const HISTORY_LIMIT = 200

/** localStorage key for the page's navigation state (survives a refresh). */
const PAGE_STORAGE_KEY = 'soulmirror.page'
/** Debounce of the localStorage write (bursts of selection / tab changes). */
const PERSIST_MS = 250

const PANE_TABS: readonly PaneTab[] = ['chat', 'announce', 'home', 'members', 'admin', 'info', 'settings']
const COL2_TABS: readonly Col2Tab[] = ['messages', 'contacts']

/** The navigation state we persist, guarded for non-browser envs (unit tests run under node). */
function loadPersistedPage(): Pick<PageSnapshot, 'open' | 'selected' | 'col2Tab' | 'paneTab'> {
  const fallback = { open: false, selected: undefined, col2Tab: 'messages' as Col2Tab, paneTab: DEFAULT_PANE_TAB as PaneTab }
  try {
    if (typeof localStorage === 'undefined') return fallback
    const raw = localStorage.getItem(PAGE_STORAGE_KEY)
    if (raw === null) return fallback
    const p = JSON.parse(raw) as Partial<PageSnapshot>
    return {
      open: p.open === true,
      selected: typeof p.selected === 'string' ? p.selected : undefined,
      col2Tab: COL2_TABS.includes(p.col2Tab as Col2Tab) ? p.col2Tab as Col2Tab : 'messages',
      paneTab: PANE_TABS.includes(p.paneTab as PaneTab) ? p.paneTab as PaneTab : DEFAULT_PANE_TAB,
    }
  } catch {
    return fallback
  }
}

export class PageStore {
  private snapshot: PageSnapshot = { ...loadPersistedPage(), threads: {}, alter: EMPTY_ALTER, deciding: [] }
  private readonly listeners = new Set<() => void>()
  private readonly typingSentAt = new Map<string, number>()
  private readonly typingIdle = new Map<string, ReturnType<typeof setTimeout>>()
  private historyTimer: ReturnType<typeof setTimeout> | undefined
  private historyAgain = false
  private persistTimer: ReturnType<typeof setTimeout> | undefined
  private seq = 0
  /**
   * Thread keys with a fetch ACTUALLY in flight. Guards use this set, never the
   * persisted `loading` flag: a request that never settles (e.g. the server was
   * restarted under an open tab) would otherwise freeze `loading: true` forever
   * and block every retry, prefetch heal included.
   */
  private readonly inflightThreads = new Set<string>()

  constructor() {
    networkStore.onFrame(this.onFrame)
  }

  getSnapshot = (): PageSnapshot => this.snapshot

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener)
    return () => { this.listeners.delete(listener) }
  }

  private set(patch: Partial<PageSnapshot>): void {
    this.snapshot = { ...this.snapshot, ...patch }
    for (const l of this.listeners) l()
    this.schedulePersist()
  }

  /** Debounce the navigation-state write so a burst of selection/tab changes writes once. */
  private schedulePersist(): void {
    if (this.persistTimer !== undefined) clearTimeout(this.persistTimer)
    this.persistTimer = setTimeout(() => {
      this.persistTimer = undefined
      this.persist()
    }, PERSIST_MS)
  }

  private persist(): void {
    try {
      if (typeof localStorage === 'undefined') return
      localStorage.setItem(PAGE_STORAGE_KEY, JSON.stringify({
        open: this.snapshot.open,
        selected: this.snapshot.selected,
        col2Tab: this.snapshot.col2Tab,
        paneTab: this.snapshot.paneTab,
      }))
    } catch {
      // best effort: a persistence failure never breaks the page
    }
  }

  private setThread(fp: string, thread: ThreadState): void {
    this.set({ threads: { ...this.snapshot.threads, [fp]: thread } })
  }

  thread = (fp: string): ThreadState => this.snapshot.threads[fp] ?? EMPTY_THREAD

  private setAlter(patch: Partial<AlterView>): void {
    this.set({ alter: { ...this.snapshot.alter, ...patch } })
  }

  /** Fetch the alter transcript (`session.history`). */
  loadAlter = async (): Promise<void> => {
    if (this.snapshot.alter.loading) {
      this.historyAgain = true
      return
    }
    this.setAlter({ loading: true })
    try {
      const h = await api.sessionHistory(HISTORY_LIMIT)
      const { error: _dropped, ...rest } = this.snapshot.alter
      this.set({ alter: { ...rest, sessionId: h.sessionId ?? undefined, status: h.status, chat: h.chat, loading: false, loaded: true } })
    } catch (e: unknown) {
      this.setAlter({ loading: false, loaded: true, error: e instanceof Error ? e.message : String(e) })
    }
    if (this.historyAgain) {
      this.historyAgain = false
      void this.loadAlter()
    }
  }

  private scheduleHistory(): void {
    if (this.historyTimer !== undefined) return
    this.historyTimer = setTimeout(() => {
      this.historyTimer = undefined
      void this.loadAlter()
    }, HISTORY_REFETCH_MS)
  }

  /**
   * The owner instructs their alter: the text becomes an owner user/message
   * in the alter session and wakes a turn; the alter decides what to tell
   * which friend and sends it with soulmirror_send_message. The transcript
   * refetches as the session moves (SSE `alter`), the friend thread shows
   * the alter's send as an outbound bubble (SSE `outbound`).
   */
  instruct = async (text: string): Promise<boolean> => {
    const body = text.replace(/\s+$/, '')
    if (body === '') return false
    const { error: _dropped, ...rest } = this.snapshot.alter
    this.set({ alter: { ...rest, instructing: true } })
    try {
      await api.instruct(body)
      this.setAlter({ instructing: false, loaded: true })
      this.scheduleHistory()
      return true
    } catch (e: unknown) {
      this.setAlter({ instructing: false, error: e instanceof Error ? e.message : String(e) })
      return false
    }
  }

  /** Decide a pending draft (approve / approve edited / reject / revise). */
  decideDraft = async (id: string, decision: { action: 'approve'; body?: string } | { action: 'reject' } | { action: 'revise'; feedback: string }): Promise<boolean> => {
    if (this.snapshot.deciding.includes(id)) return false
    this.set({ deciding: [...this.snapshot.deciding, id] })
    try {
      const result = await api.decideDraft(id, decision)
      if (result.entry !== undefined) {
        const thread = this.snapshot.threads[result.draft.fp]
        if (thread !== undefined) this.setThread(result.draft.fp, applyOutbound(thread, result.entry))
        networkStore.applyOutbound(result.draft.fp, result.entry)
      }
      this.scheduleHistory()
      void networkStore.refresh()
      return true
    } catch (e: unknown) {
      this.setAlter({ error: e instanceof Error ? e.message : String(e) })
      return false
    } finally {
      this.set({ deciding: this.snapshot.deciding.filter(d => d !== id) })
    }
  }

  open = (selection?: string): void => {
    const selected = selection ?? this.snapshot.selected ?? ALTER_KEY
    this.set({ open: true, selected })
    this.prime(selected)
  }

  close = (): void => { if (this.snapshot.open) this.set({ open: false }) }

  toggle = (): void => {
    if (this.snapshot.open) this.close()
    else this.open()
  }

  select = (selection: string): void => {
    if (selection === this.snapshot.selected) return
    this.set({ selected: selection, paneTab: DEFAULT_PANE_TAB })
    this.prime(selection)
  }

  /** Switch the second-column section (messages / contacts). */
  setCol2Tab = (tab: Col2Tab): void => { if (tab !== this.snapshot.col2Tab) this.set({ col2Tab: tab }) }

  /** Switch the third-column panel (chat / announce / home / members / admin / info). */
  setPaneTab = (tab: PaneTab): void => { if (tab !== this.snapshot.paneTab) this.set({ paneTab: tab }) }

  /** Fetch what the selection needs (the transcript for the alter, the archive for a friend or group). */
  private prime(selection: string): void {
    if (selection === ALTER_KEY) {
      if (!this.snapshot.alter.loaded) void this.loadAlter()
      return
    }
    const gid = gidOf(selection)
    if (gid !== undefined) {
      void this.loadGroup(gid)
      return
    }
    if (agentOf(selection) !== undefined) return // the agent pane loads its own transcript
    void this.load(selection)
  }

  /** Fetch (or re-fetch) the last `window` entries of a friend's archive. */
  load = async (fp: string, window?: number): Promise<void> => {
    if (this.inflightThreads.has(fp)) return
    const current = this.thread(fp)
    const size = window ?? (current.window > 0 ? current.window : PAGE_SIZE)
    this.inflightThreads.add(fp)
    this.setThread(fp, { ...current, loading: true })
    try {
      const { entries } = await api.conversation(fp, { limit: size })
      this.setThread(fp, applyArchive(this.thread(fp), entries, size))
    } catch (e: unknown) {
      this.setThread(fp, { ...this.thread(fp), loading: false, loaded: true, error: e instanceof Error ? e.message : String(e) })
    } finally {
      this.inflightThreads.delete(fp)
    }
  }

  /** The owner scrolled to the top: widen the window by one page. */
  loadOlder = (fp: string): Promise<void> => {
    const t = this.thread(fp)
    if (t.complete || t.loading) return Promise.resolve()
    return this.load(fp, (t.window > 0 ? t.window : PAGE_SIZE) + PAGE_SIZE)
  }

  /**
   * Debug direct send (Settings → "send as myself"): optimistic bubble now,
   * reconcile with the archived entry the host answers, or mark it failed.
   */
  send = async (fp: string, body: string): Promise<boolean> => {
    const text = body.replace(/\s+$/, '')
    if (text === '') return false
    this.seq += 1
    const clientId = `local-${Date.now().toString(36)}-${this.seq}`
    this.setThread(fp, addOptimistic(this.thread(fp), { clientId, body: text, ts: Date.now() }))
    this.stopTyping(fp)
    try {
      const { entry } = await api.send(fp, text)
      this.setThread(fp, reconcileSend(this.thread(fp), clientId, entry))
      networkStore.applyOutbound(fp, entry)
      return true
    } catch (e: unknown) {
      this.setThread(fp, failSend(this.thread(fp), clientId, e instanceof Error ? e.message : String(e)))
      return false
    }
  }

  // ——— group threads (cached under the selection key `g:<gid>`) ———

  groupThread = (gid: string): ThreadState => this.snapshot.threads[groupKey(gid)] ?? EMPTY_THREAD

  /** Fetch (or re-fetch) the last `window` entries of a group's archive. */
  loadGroup = async (gid: string, window?: number): Promise<void> => {
    const key = groupKey(gid)
    if (this.inflightThreads.has(key)) return
    const current = this.thread(key)
    const size = window ?? (current.window > 0 ? current.window : PAGE_SIZE)
    this.inflightThreads.add(key)
    this.setThread(key, { ...current, loading: true })
    try {
      const { entries } = await api.groupConversation(gid, { limit: size })
      this.setThread(key, applyArchive(this.thread(key), entries, size))
    } catch (e: unknown) {
      this.setThread(key, { ...this.thread(key), loading: false, loaded: true, error: e instanceof Error ? e.message : String(e) })
    } finally {
      this.inflightThreads.delete(key)
    }
  }

  loadOlderGroup = (gid: string): Promise<void> => {
    const t = this.groupThread(gid)
    if (t.complete || t.loading) return Promise.resolve()
    return this.loadGroup(gid, (t.window > 0 ? t.window : PAGE_SIZE) + PAGE_SIZE)
  }

  /** Warm a friend thread (idle prefetch / search). No-op once loaded. */
  prefetchFriend = (fp: string): Promise<void> => {
    const t = this.thread(fp)
    if (t.loaded || this.inflightThreads.has(fp)) return Promise.resolve()
    return this.load(fp)
  }

  /** Warm a group thread ahead of the click (idle prefetch / row hover). No-op once loaded. */
  prefetchGroup = (gid: string): Promise<void> => {
    const t = this.groupThread(gid)
    // Guard on real in-flight work, not the persisted flag: a stale `loading: true`
    // (request lost to a server restart) must not stop the prefetch from healing it.
    if (t.loaded || this.inflightThreads.has(groupKey(gid))) return Promise.resolve()
    return this.loadGroup(gid)
  }

  /**
   * Warm many threads ONE AT A TIME. Sequential on purpose: a parallel warm-up
   * grabs every browser connection slot and queues the user's own clicks (and
   * even dsh's other requests) behind it.
   */
  warm = async (targets: readonly ({ kind: 'friend'; fp: string } | { kind: 'group'; gid: string })[]): Promise<void> => {
    for (const t of targets) {
      try {
        await (t.kind === 'friend' ? this.prefetchFriend(t.fp) : this.prefetchGroup(t.gid))
      } catch {
        // best effort: a failed warm-up never stops the rest
      }
    }
  }

  /** Send to the group: optimistic bubble, reconcile with the archived entry. `by` = provenance (default owner). */
  sendGroup = async (gid: string, body: string, opts?: { by?: 'owner' | 'alter' }): Promise<boolean> => {
    const text = body.replace(/\s+$/, '')
    if (text === '') return false
    const key = groupKey(gid)
    this.seq += 1
    const clientId = `local-${Date.now().toString(36)}-${this.seq}`
    this.setThread(key, addOptimistic(this.thread(key), { clientId, body: text, ts: Date.now() }))
    try {
      const { entry } = await api.groupSend(gid, text, opts?.by ?? 'owner')
      if (entry === null) {
        this.setThread(key, failSend(this.thread(key), clientId, 'not archived'))
        return false
      }
      this.setThread(key, reconcileSend(this.thread(key), clientId, entry))
      void networkStore.refresh()
      return entry.status !== 'error'
    } catch (e: unknown) {
      this.setThread(key, failSend(this.thread(key), clientId, e instanceof Error ? e.message : String(e)))
      return false
    }
  }

  /** Discard a failed bubble (returns its body so the composer can offer it again). */
  discard = (fp: string, clientId: string): string | undefined => {
    const entry = this.thread(fp).entries.find(e => e.clientId === clientId)
    this.setThread(fp, dropEntry(this.thread(fp), clientId))
    return entry?.body
  }

  /** The owner is typing in the debug composer: best-effort `message.typing on`, throttled; `off` after idle. */
  noteTyping = (fp: string): void => {
    const now = Date.now()
    const last = this.typingSentAt.get(fp) ?? 0
    if (now - last > TYPING_RESEND_MS) {
      this.typingSentAt.set(fp, now)
      void api.typing(fp, true).catch(() => {})
    }
    const idle = this.typingIdle.get(fp)
    if (idle !== undefined) clearTimeout(idle)
    this.typingIdle.set(fp, setTimeout(() => { this.stopTyping(fp) }, TYPING_IDLE_MS))
  }

  stopTyping = (fp: string): void => {
    const idle = this.typingIdle.get(fp)
    if (idle !== undefined) {
      clearTimeout(idle)
      this.typingIdle.delete(fp)
    }
    if (this.typingSentAt.has(fp)) {
      this.typingSentAt.delete(fp)
      void api.typing(fp, false).catch(() => {})
    }
  }

  private onFrame = (frame: NetworkEventFrame): void => {
    switch (frame.kind) {
      case 'message': {
        const fp = frame.message.from
        const thread = this.snapshot.threads[fp]
        if (thread !== undefined) this.setThread(fp, applyInbound(thread, frame.message))
        // Mail reaches the alter session too: the transcript shows it.
        if (this.snapshot.alter.loaded) this.scheduleHistory()
        return
      }
      case 'outbound': {
        const thread = this.snapshot.threads[frame.fp]
        if (thread === undefined) return
        this.setThread(frame.fp, applyOutbound(thread, frame.entry))
        return
      }
      case 'alter':
        this.setAlter({ sessionId: frame.state.sessionId, status: frame.state.status })
        if (this.snapshot.alter.loaded || this.snapshot.open) this.scheduleHistory()
        return
      case 'draft':
        if (this.snapshot.alter.loaded) this.scheduleHistory()
        return
      case 'group_message': {
        const key = groupKey(frame.gid)
        const thread = this.snapshot.threads[key]
        if (thread !== undefined) this.setThread(key, applyInbound(thread, frame.message))
        return
      }
      case 'group_outbound': {
        const key = groupKey(frame.gid)
        const thread = this.snapshot.threads[key]
        if (thread === undefined) return
        this.setThread(key, applyOutbound(thread, frame.entry))
        return
      }
      default:
        return
    }
  }
}

export const pageStore = new PageStore()
