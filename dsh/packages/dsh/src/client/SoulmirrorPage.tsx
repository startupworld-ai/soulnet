/**
 * `shell.overlay` entry `soulmirror-page`: the full SoulMirror page — the
 * product's chat page inside dsh (prototype #B). It covers the frame to the
 * RIGHT of dsh's own sidebar (the sidebar keeps working: New Session, the
 * session list, the footer buttons), with the middle column (the pinned "My
 * alter" item, pending requests, friends, add friend) and the right pane:
 * "My alter" = the chat with the alter + composer (the only place the owner
 * talks), a friend = the read-only alter ↔ alter thread with the action bar.
 * Escape / the close button return to dsh.
 *
 * Geometry: `ctx.layout` (ui-layout's ILayout) exposes panel ACTIONS only
 * (toggleSidebar / openDetails / closeDetails) — the width store is private
 * to the root entry — so the left edge is measured from the DOM: the sidebar
 * column is the ancestor of our own footer button whose parent is the frame
 * (the overlay layer's parent). A ResizeObserver on it plus the window
 * resize event re-measure on drag / collapse.
 */
import { useCallback, useEffect, useLayoutEffect, useRef, useState, useSyncExternalStore } from 'react'
import type { SettingsScope } from '@deepseek-ai/dsh-client-runtime/client'
import type { PropsLocale, PropsRenderSlots } from '@deepseek-ai/dsh-client-ui-slots'
import { AgentPane } from './AgentPane.tsx'
import { AlterPane } from './AlterPane.tsx'
import { networkStore } from './api.ts'
import { FriendList } from './FriendList.tsx'
import { FriendPane } from './FriendPane.tsx'
import type {} from './group-room.ts'
import { GroupPane } from './GroupPane.tsx'
import type { NS } from './locales.ts'
import { agentOf, ALTER_KEY, gidOf, groupKey, resolveSelection } from './page-state.ts'
import { pageStore } from './page-store.ts'
import type { SoulmirrorSettingsValues } from './SettingsSection.tsx'

export interface SoulmirrorPageInjected {
  /** Select a session as current (ctx.sessions.open). */
  openSession: (sessionId: string) => void
  /** Bound settings scope of the `soulmirror` namespace (the page reads the debug `directSend` toggle live). */
  scope: SettingsScope<SoulmirrorSettingsValues>
}

/** The page declares (and thereby renders) the `group.room` seat; the GroupPane receives `renderSlot` as plain props. */
export type SoulmirrorPageProps = SoulmirrorPageInjected & PropsLocale<typeof NS> & PropsRenderSlots<'group.room' | 'alter.card'>

/** Fallback left edge when the sidebar column cannot be found (ui-layout SIDEBAR_DEFAULT). */
const SIDEBAR_FALLBACK = 280

/**
 * dsh's sidebar column and the frame it sits in: the ancestor of our own
 * footer button whose parent also contains this page (the slot framework
 * wraps overlay entries, so the page's parent is not the layer itself). The
 * page is absolutely positioned inside the overlay layer, which covers the
 * frame, so `column.right - frame.left` is the page's left edge.
 */
function sidebarColumnOf(pageRoot: HTMLElement | null): { column: HTMLElement; frame: HTMLElement } | undefined {
  if (pageRoot === null) return undefined
  const footer = document.querySelector('[data-soulmirror-footer]')
  let node: HTMLElement | null = footer instanceof HTMLElement ? footer : null
  while (node !== null) {
    const parent: HTMLElement | null = node.parentElement
    if (parent !== null && parent !== node && parent.contains(pageRoot) && !node.contains(pageRoot)) return { column: node, frame: parent }
    node = parent
  }
  return undefined
}

export function SoulmirrorPage({ openSession, scope, t, renderSlot }: SoulmirrorPageProps) {
  const page = useSyncExternalStore(pageStore.subscribe, pageStore.getSnapshot)
  const net = useSyncExternalStore(networkStore.subscribe, networkStore.getSnapshot)
  const subscribeScope = useCallback((listener: () => void) => scope.subscribe(listener), [scope])
  const readScope = useCallback(() => scope.getSnapshot(), [scope])
  const settings = useSyncExternalStore(subscribeScope, readScope)
  // Debug "send as myself": the live settings document, else what the host resolved.
  const directSend = settings.value?.directSend === true || (settings.value?.directSend === undefined && net.state?.alter?.directSend === true)
  const root = useRef<HTMLDivElement>(null)
  const [left, setLeft] = useState(SIDEBAR_FALLBACK)
  const selected = resolveSelection(net.inbox.friends, net.inbox.groups, net.state?.agents ?? [], page.selected)
  const gid = gidOf(selected)
  const group = gid === undefined ? undefined : net.inbox.groups.find(g => g.gid === gid)
  const agentName = agentOf(selected)
  const seatAgent = agentName === undefined ? undefined : net.state?.agents?.find(a => a.name === agentName)
  const friend = selected === ALTER_KEY || gid !== undefined || agentName !== undefined ? undefined : net.inbox.friends.find(f => f.fp === selected)

  // Left edge = right edge of dsh's sidebar column; re-measured on resize / collapse.
  useLayoutEffect(() => {
    if (!page.open) return
    const measure = (): void => {
      const found = sidebarColumnOf(root.current)
      if (found === undefined) {
        setLeft(SIDEBAR_FALLBACK)
        return
      }
      const edge = found.column.getBoundingClientRect().right - found.frame.getBoundingClientRect().left
      setLeft(Math.max(0, Math.round(edge)))
    }
    measure()
    const found = sidebarColumnOf(root.current)
    const observer = typeof ResizeObserver === 'undefined' || found === undefined ? undefined : new ResizeObserver(measure)
    if (found !== undefined) observer?.observe(found.column)
    window.addEventListener('resize', measure)
    // The sidebar collapse is a CSS transition; poll briefly so the edge follows it.
    const timer = window.setInterval(measure, 300)
    return () => {
      observer?.disconnect()
      window.removeEventListener('resize', measure)
      window.clearInterval(timer)
    }
  }, [page.open])

  // Escape returns to dsh — unless the card popover is open (the friend pane
  // closes it on the same key and the page stays).
  useEffect(() => {
    if (!page.open) return
    const onKey = (e: KeyboardEvent): void => {
      if (e.key !== 'Escape') return
      if (document.querySelector('[data-soulmirror-page-card-pop]') !== null) return
      if (document.querySelector('[data-soulmirror-modal]') !== null) return // open dialogs close themselves
      pageStore.close()
    }
    document.addEventListener('keydown', onKey)
    return () => { document.removeEventListener('keydown', onKey) }
  }, [page.open])

  // Clicking dsh's own sidebar (anywhere outside the page) returns to the
  // normal dsh workspace. The SoulMirror footer button that toggles the page
  // is excluded (it handles its own open/close).
  useEffect(() => {
    if (!page.open) return
    const onDown = (e: MouseEvent): void => {
      const rootEl = root.current
      if (rootEl === null) return
      const target = e.target instanceof Node ? e.target : (e.target as Element | null)
      if (target === null) return
      // A click inside the page can unmount its own target before this
      // document-level listener runs (the mention popup does: picking an item
      // closes it synchronously). A detached node is NOT "outside the page".
      if (!target.isConnected) return
      if (rootEl.contains(target)) return
      if ((target as Element | null)?.closest?.('[data-soulmirror-footer]') != null) return
      // A modal (agent sheet, group agents sheet, add-friend / join / create
      // dialogs) closes itself on mousedown and detaches its target before this
      // document listener runs — the click is still page content, never a
      // "return to dsh" click.
      if ((target as Element | null)?.closest?.('[data-soulmirror-modal]') != null) return
      // The @-mention box removes itself on mousedown (before this document
      // listener runs), so its target is already detached from root — but it is
      // still page content and must never close the page.
      if ((target as Element | null)?.closest?.('[data-soulmirror-mention-pop]') != null) return
      // Any floating surface that belongs to the page (memory popup, toasts,
      // future cards) lives outside the page root in the overlay layer; a click
      // there is page content, not "clicking dsh's sidebar". Every such surface
      // opts in by carrying this one attribute — no per-popup exclusions.
      if ((target as Element | null)?.closest?.('[data-soulmirror-float]') != null) return
      // The whole overlay layer (every shell.overlay entry: this page, the
      // memory popup, future cards) is page surface, never "dsh's sidebar".
      if ((target as Element | null)?.closest?.('[data-shell-overlay]') != null) return
      pageStore.close()
    }
    document.addEventListener('mousedown', onDown)
    return () => { document.removeEventListener('mousedown', onDown) }
  }, [page.open])

  // Warm every thread while the page is open so clicking a row is always a cache
  // hit and the content search has archives to look through (guarded; loaded
  // threads are skipped).
  const groupGids = net.inbox.groups.map(g => g.gid).join(',')
  const friendFps = net.inbox.friends.map(f => f.fp).join(',')
  useEffect(() => {
    if (!page.open || (groupGids === '' && friendFps === '')) return
    const timer = window.setTimeout(() => {
      void pageStore.warm([
        ...(groupGids === '' ? [] : groupGids.split(',').slice(0, 15)).map(gid => ({ kind: 'group' as const, gid })),
        ...(friendFps === '' ? [] : friendFps.split(',').slice(0, 15)).map(fp => ({ kind: 'friend' as const, fp })),
      ])
    }, 150)
    return () => { window.clearTimeout(timer) }
  }, [page.open, groupGids, friendFps])

  if (!page.open) return null

  const goAlter = (): void => { pageStore.select(ALTER_KEY) }
  const goFriend = (fp: string): void => { pageStore.select(fp) }
  const goGroup = (gid: string): void => { pageStore.select(groupKey(gid)) }
  const openContact = (fp: string): void => {
    pageStore.select(fp)
    pageStore.setPaneTab('home')
  }
  const openAlterSession = (sessionId: string): void => {
    openSession(sessionId)
    pageStore.close()
  }

  return (
    <div ref={root} className="sm-page-root" style={{ left }} role="dialog" aria-label={t('page.title')} data-soulmirror-page data-soulmirror-page-left={left}>
      <FriendList
        t={t}
        selected={selected}
        onSelect={(key) => { pageStore.select(key) }}
        onSelectContact={openContact}
        onAccepted={(fp) => { pageStore.select(fp) }}
        onClose={() => { pageStore.close() }}
      />
      {friend !== undefined
        ? <FriendPane t={t} friend={friend} visible={page.open} onGoAlter={goAlter} directSend={directSend} />
        : group !== undefined
          ? <GroupPane t={t} group={group} visible={page.open} onGoAlter={goAlter} renderRoom={renderSlot} />
          : seatAgent !== undefined
            ? <AgentPane t={t} agent={seatAgent} onOpenSession={openAlterSession} onRemoved={goAlter} />
            : <AlterPane t={t} onOpenSession={openAlterSession} onGoFriend={goFriend} onGoGroup={goGroup} renderCards={renderSlot} scope={scope} />}
    </div>
  )
}
